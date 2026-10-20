// Package jobrunner renders and runs the one-shot faultctl/evaluator
// Kubernetes Jobs that rcactl's inject/validate/full-run subcommands
// dispatch, replacing the hand-written deploy/fault-manager/*.yaml
// pairs. See the plan's Phase 2 for the exact fields this must match.
package jobrunner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/duyquang47/llm-rca/internal/scenario"
)

type Kind int

const (
	KindInject Kind = iota
	KindValidate
)

// Options carries every value that varies between clusters/environments
// (image registry tag, namespaces, ClickHouse coordinates for the
// evaluator container) — everything else in the rendered Job is fixed,
// copied field-for-field from the manifests this replaces.
type Options struct {
	ImageTag      string
	Namespace     string
	DemoNamespace string

	ClickHouseAddr     string
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseDatabase string
}

// k8sName converts a scenario name (snake_case, as scenario.Load()
// reads it) into a valid Kubernetes object name — k8s names disallow
// underscores.
func k8sName(scenarioName string) string {
	return strings.ReplaceAll(scenarioName, "_", "-")
}

// NewClientset loads a kubeconfig the same way kubectl does: an
// explicit path if given, otherwise $KUBECONFIG or ~/.kube/config.
func NewClientset(kubeconfigPath string) (*kubernetes.Clientset, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

var (
	jobResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"cpu": resource.MustParse("50m"), "memory": resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{"cpu": resource.MustParse("500m"), "memory": resource.MustParse("256Mi")},
	}
)

// Render builds the ConfigMap+Job pair for one scenario run. Field
// values below are copied verbatim from
// deploy/fault-manager/faultctl-scenario1.yaml (kind=inject) and
// evaluator-scenario1.yaml (kind=validate) — see the plan's Phase 2 for
// the line-by-line verification.
func Render(scenarioPath string, kind Kind, opts Options) (*corev1.ConfigMap, *batchv1.Job, error) {
	raw, err := os.ReadFile(scenarioPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read scenario file: %w", err)
	}
	sc, err := scenario.Load(scenarioPath)
	if err != nil {
		return nil, nil, err
	}

	name := k8sName(sc.Name)
	cmName := "scenario-" + name

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: opts.Namespace},
		Data:       map[string]string{"scenario.yaml": string(raw)},
	}

	scenarioVolume := corev1.Volume{
		Name:         "scenario",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}},
	}
	groundTruthVolume := corev1.Volume{
		Name:         "ground-truth",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "ground-truth-pvc"}},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "scenario", MountPath: "/etc/faultctl"},
		{Name: "ground-truth", MountPath: "/var/lib/faultctl/ground-truth"},
	}

	var job *batchv1.Job
	switch kind {
	case KindInject:
		runAsRoot := int64(0)
		job = &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "faultctl-" + name, Namespace: opts.Namespace},
			Spec: batchv1.JobSpec{
				BackoffLimit: int32Ptr(0),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						// Always attached: the faultctl ServiceAccount
						// (deploy/fault-manager/rbac.yaml) only grants
						// get/patch on demo namespace Deployments, so
						// attaching it to every scenario (not just the
						// 4 that need direct k8s writes) is harmless
						// and avoids inspecting step actions here to
						// decide.
						ServiceAccountName: "faultctl",
						Containers: []corev1.Container{{
							Name:            "faultctl",
							Image:           "docker.io/quang47/llm-rca-faultctl:" + opts.ImageTag,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{RunAsUser: &runAsRoot},
							Args:            []string{"-scenario=/etc/faultctl/scenario.yaml", "-demo-namespace=" + opts.DemoNamespace},
							Env:             []corev1.EnvVar{{Name: "GROUND_TRUTH_DIR", Value: "/var/lib/faultctl/ground-truth"}},
							VolumeMounts:    volumeMounts,
							Resources:       jobResources,
						}},
						Volumes: []corev1.Volume{scenarioVolume, groundTruthVolume},
					},
				},
			},
		}
	case KindValidate:
		job = &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "evaluator-" + name, Namespace: opts.Namespace},
			Spec: batchv1.JobSpec{
				BackoffLimit: int32Ptr(0),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:            "evaluator",
							Image:           "docker.io/quang47/llm-rca-evaluator:" + opts.ImageTag,
							ImagePullPolicy: corev1.PullAlways,
							Args: []string{
								"-scenario=" + sc.Name,
								"-scenario-file=/etc/faultctl/scenario.yaml",
							},
							Env: []corev1.EnvVar{
								{Name: "GROUND_TRUTH_DIR", Value: "/var/lib/faultctl/ground-truth"},
								{Name: "CLICKHOUSE_ADDR", Value: opts.ClickHouseAddr},
								{Name: "CLICKHOUSE_USER", Value: opts.ClickHouseUser},
								{Name: "CLICKHOUSE_PASSWORD", Value: opts.ClickHousePassword},
								{Name: "CLICKHOUSE_DATABASE", Value: opts.ClickHouseDatabase},
							},
							VolumeMounts: volumeMounts,
							Resources:    jobResources,
						}},
						Volumes: []corev1.Volume{scenarioVolume, groundTruthVolume},
					},
				},
			},
		}
	default:
		return nil, nil, fmt.Errorf("unknown jobrunner.Kind %d", kind)
	}

	return cm, job, nil
}

// Run applies the ConfigMap then the Job, waits for it to reach
// Complete/Failed, optionally streaming the pod's logs to stdout as it
// runs, then always deletes both the Job (foreground propagation, so
// its pod is cleaned up too) and the ConfigMap — success or failure —
// so a fixed Job name never blocks the next run. Cancel ctx (or give it
// a deadline) to bound how long this waits.
func Run(ctx context.Context, clientset *kubernetes.Clientset, cm *corev1.ConfigMap, job *batchv1.Job, detach bool) error {
	ns := job.Namespace

	if _, err := clientset.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create configmap %s: %w", cm.Name, err)
	}
	defer cleanup(clientset, cm, job)

	created, err := clientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job %s: %w", job.Name, err)
	}

	if !detach {
		go streamLogs(ctx, clientset, ns, created.Name)
	}

	return waitForCompletion(ctx, clientset, ns, created.Name)
}

func waitForCompletion(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		j, err := clientset.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %s: %w", name, err)
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return fmt.Errorf("job %s failed: %s", name, c.Message)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for job %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// streamLogs finds the Job's pod and follows its logs to stdout. Best
// effort: a failure here only means less visibility into a running
// scenario, not a failed run, so errors are reported but not fatal.
func streamLogs(ctx context.Context, clientset *kubernetes.Clientset, ns, jobName string) {
	var podName string
	for i := 0; i < 60; i++ {
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
		if err == nil && len(pods.Items) > 0 {
			podName = pods.Items[0].Name
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if podName == "" {
		fmt.Fprintln(os.Stderr, "[rcactl] warning: could not find pod for job", jobName, "to stream logs")
		return
	}

	stream, err := clientset.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[rcactl] warning: log stream failed:", err)
		return
	}
	defer stream.Close()
	io.Copy(os.Stdout, stream)
}

func cleanup(clientset *kubernetes.Clientset, cm *corev1.ConfigMap, job *batchv1.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propagation := metav1.DeletePropagationForeground
	if err := clientset.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintln(os.Stderr, "[rcactl] warning: delete job", job.Name, "failed:", err)
	}
	if err := clientset.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintln(os.Stderr, "[rcactl] warning: delete configmap", cm.Name, "failed:", err)
	}
}

func int32Ptr(v int32) *int32 { return &v }
