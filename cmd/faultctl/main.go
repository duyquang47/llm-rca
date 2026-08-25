// Command faultctl reads a scenario YAML (CLAUDE.md §7), executes its
// steps against the demo namespace's /_fault endpoints, and records
// ground truth to a store isolated from context-builder/orchestrator.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/duyquang47/llm-rca/internal/groundtruth"
	"github.com/duyquang47/llm-rca/internal/scenario"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	scenarioPath := flag.String("scenario", "", "path to scenario YAML (required)")
	demoNamespace := flag.String("demo-namespace", "demo", "namespace the target services run in")
	groundTruthDir := flag.String("ground-truth-dir", envOr("GROUND_TRUTH_DIR", "/var/lib/faultctl/ground-truth"), "where to write ground truth records")
	flag.Parse()

	if *scenarioPath == "" {
		return fmt.Errorf("-scenario is required")
	}

	sc, err := scenario.Load(*scenarioPath)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	startedAt := time.Now()

	// Lazily built — most scenarios never touch the k8s API at all
	// (everything goes through /_fault), so only construct a Clientset
	// once a step actually needs one (case #6's CPU-limit patch).
	var k8sClient *kubernetes.Clientset
	// original CPU limits, keyed by Deployment name, so reset_cpu_limit
	// can restore exactly what patch_cpu_limit overwrote without the
	// scenario YAML needing to hardcode a value that could drift from
	// deploy/demo/*.yaml.
	originalCPULimit := map[string]string{}
	originalReplicas := map[string]int32{}
	// original env var values, keyed by "<target>/<key>" (a Deployment
	// can have multiple env vars removed across a scenario, unlike
	// CPU limit/replicas which only ever have one active value).
	originalEnvVars := map[string]string{}
	ctx := context.Background()

	for i, step := range sc.Steps {
		fmt.Printf("[%s] step %d: action=%s target=%s\n", sc.Name, i+1, step.Action, step.Target)
		switch step.Action {
		case "set_config":
			if err := setConfig(client, *demoNamespace, step); err != nil {
				return fmt.Errorf("step %d (set_config): %w", i+1, err)
			}
		case "reset":
			retryCleanup(fmt.Sprintf("step %d (reset)", i+1), func() error {
				return resetConfig(client, *demoNamespace, step)
			})
		case "wait":
			d, err := time.ParseDuration(step.Duration)
			if err != nil {
				return fmt.Errorf("step %d (wait): invalid duration %q: %w", i+1, step.Duration, err)
			}
			time.Sleep(d)
		case "burst_load":
			if err := burstLoad(client, step); err != nil {
				return fmt.Errorf("step %d (burst_load): %w", i+1, err)
			}
		case "sustain_fault":
			if err := sustainFault(client, *demoNamespace, step); err != nil {
				return fmt.Errorf("step %d (sustain_fault): %w", i+1, err)
			}
		case "patch_cpu_limit":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (patch_cpu_limit): %w", i+1, err)
				}
			}
			if err := patchCPULimit(ctx, k8sClient, *demoNamespace, step, originalCPULimit); err != nil {
				return fmt.Errorf("step %d (patch_cpu_limit): %w", i+1, err)
			}
		case "reset_cpu_limit":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (reset_cpu_limit): %w", i+1, err)
				}
			}
			retryCleanup(fmt.Sprintf("step %d (reset_cpu_limit)", i+1), func() error {
				return resetCPULimit(ctx, k8sClient, *demoNamespace, step, originalCPULimit)
			})
		case "scale_deployment":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (scale_deployment): %w", i+1, err)
				}
			}
			if err := scaleDeployment(ctx, k8sClient, *demoNamespace, step, originalReplicas); err != nil {
				return fmt.Errorf("step %d (scale_deployment): %w", i+1, err)
			}
		case "reset_scale":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (reset_scale): %w", i+1, err)
				}
			}
			retryCleanup(fmt.Sprintf("step %d (reset_scale)", i+1), func() error {
				return resetScale(ctx, k8sClient, *demoNamespace, step, originalReplicas)
			})
		case "remove_env_var":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (remove_env_var): %w", i+1, err)
				}
			}
			if err := removeEnvVar(ctx, k8sClient, *demoNamespace, step, originalEnvVars); err != nil {
				return fmt.Errorf("step %d (remove_env_var): %w", i+1, err)
			}
		case "restore_env_var":
			if k8sClient == nil {
				var err error
				k8sClient, err = newK8sClient()
				if err != nil {
					return fmt.Errorf("step %d (restore_env_var): %w", i+1, err)
				}
			}
			retryCleanup(fmt.Sprintf("step %d (restore_env_var)", i+1), func() error {
				return restoreEnvVar(ctx, k8sClient, *demoNamespace, step, originalEnvVars)
			})
		case "publish_poison_message":
			if err := publishPoisonMessages(ctx, step); err != nil {
				return fmt.Errorf("step %d (publish_poison_message): %w", i+1, err)
			}
		default:
			return fmt.Errorf("step %d: unknown action %q", i+1, step.Action)
		}
	}

	var causes []groundtruth.Cause
	for _, c := range sc.GroundTruth.Causes {
		causes = append(causes, groundtruth.Cause{Service: c.Service, Cause: c.Cause})
	}

	store := groundtruth.NewStore(*groundTruthDir)
	if err := store.Save(groundtruth.Record{
		ScenarioName: sc.Name,
		Service:      sc.GroundTruth.Service,
		Cause:        sc.GroundTruth.Cause,
		Causes:       causes,
		Distractors:  sc.GroundTruth.Distractors,
		StartedAt:    startedAt,
	}); err != nil {
		return fmt.Errorf("save ground truth: %w", err)
	}

	fmt.Printf("[%s] done, ground truth recorded (%d cause(s))\n", sc.Name, len(sc.GroundTruth.AllCauses()))
	return nil
}

// setConfig POSTs step.Params directly as the /_fault JSON body — the
// scenario YAML's params keys are written to match faults.Spec's json
// tags exactly (route, latency, error, cpu_burn, mem_leak, pool_size),
// so no field-by-field mapping is needed.
func setConfig(client *http.Client, namespace string, step scenario.Step) error {
	body, err := json.Marshal(step.Params)
	if err != nil {
		return fmt.Errorf("marshal fault spec: %w", err)
	}
	url := fmt.Sprintf("http://%s.%s:9999/_fault", step.Target, namespace)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST %s: unexpected status %d", url, resp.StatusCode)
	}
	return nil
}

func resetConfig(client *http.Client, namespace string, step scenario.Step) error {
	route, _ := step.Params["route"].(string)
	if route == "" {
		return fmt.Errorf("reset step requires params.route")
	}
	url := fmt.Sprintf("http://%s.%s:9999/_fault?route=%s", step.Target, namespace, route)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE %s: unexpected status %d", url, resp.StatusCode)
	}
	return nil
}

// burstLoad hammers params.url with params.concurrency goroutines for
// step.Duration. Needed for scenarios like case #3 (pool exhaustion)
// where CLAUDE.md §7 explicitly calls for "pool_size=2 + tăng tải" —
// the background k6 rate (20rps) alone is nowhere near enough to
// contend for 2 Postgres connections when each query only takes a few
// ms, so the fault alone produces no observable signal without a
// concurrent burst forcing requests to actually queue on the pool.
func burstLoad(client *http.Client, step scenario.Step) error {
	url, _ := step.Params["url"].(string)
	if url == "" {
		return fmt.Errorf("burst_load requires params.url")
	}
	concurrency := paramInt(step.Params["concurrency"], 10)
	d, err := time.ParseDuration(step.Duration)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", step.Duration, err)
	}

	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				resp, err := client.Get(url)
				if err != nil {
					continue
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	return nil
}

// sustainFault re-POSTs step.Params as the fault spec every few seconds
// for step.Duration. CLAUDE.md §4.4 says fault state is in-memory and
// reset on pod restart — for most scenarios that never matters because
// nothing crashes the pod, but case #5 (OOMKill) needs the fault
// re-applied after every restart or it only OOMs once instead of
// producing a real CrashLoopBackOff. Re-POSTing on a fixed interval
// achieves that without needing to watch pod restarts directly, and
// errors are swallowed since the target is expected to be briefly
// unreachable while it's crashing/restarting — that's not a failure of
// this step, it's the scenario working as intended.
func sustainFault(client *http.Client, namespace string, step scenario.Step) error {
	d, err := time.ParseDuration(step.Duration)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", step.Duration, err)
	}
	const interval = 3 * time.Second
	deadline := time.Now().Add(d)
	for {
		_ = setConfig(client, namespace, step)
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(interval)
	}
}

// retryCleanup runs a cleanup step (reset / reset_cpu_limit) with a
// few retries, but never fails the run — cleanup steps can race a
// pod that's mid-restart from the fault that just ran (case #6 found
// this for real: reset_cpu_limit's own patch triggers another
// rollout, and the immediately-following reset raced it and got
// "context deadline exceeded"). Treating that as fatal aborted run()
// before ground truth ever got saved, even though the actual fault
// injection had already succeeded — losing an entire scenario run
// over a cleanup timing issue unrelated to the experiment's validity.
func retryCleanup(label string, fn func() error) {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("warning: %s failed after %d attempts, continuing anyway: %v\n", label, attempts, err)
}

func newK8sClient() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// patchCPULimit and resetCPULimit exist for case #6 (CPU throttling):
// unlike every other fault, this one isn't reachable through /_fault —
// it has to change the Deployment's actual resource limit, which
// CLAUDE.md's own §2.4/§7 notes flags as a real isolation risk (the
// resulting rollout generates ordinary k8s Events in namespace "demo"
// that the chaos-namespace exclusion rule never touches, unlike
// events from a pod faultctl itself owns). Nothing here sets any
// custom label/annotation/message mentioning faultctl or chaos — the
// Events k8s generates for a routine Deployment rollout (e.g.
// "ScalingReplicaSet") look identical to a human-initiated change, and
// the existing must_not_include assertion plus
// assertions_test.go's isolation-shape tests are what actually verify
// that, not this code.
func patchCPULimit(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]string) error {
	millicores := paramInt(step.Params["millicores"], 0)
	if millicores <= 0 {
		return fmt.Errorf("patch_cpu_limit requires params.millicores > 0")
	}

	deploy, err := client.AppsV1().Deployments(namespace).Get(ctx, step.Target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", step.Target, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("deployment %s has no containers", step.Target)
	}
	if current, ok := containers[0].Resources.Limits["cpu"]; ok {
		original[step.Target] = current.String()
	}

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"resources":{"limits":{"cpu":"%dm"}}}]}}}}`,
		containers[0].Name, millicores)
	_, err = client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

func resetCPULimit(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]string) error {
	orig, ok := original[step.Target]
	if !ok {
		return fmt.Errorf("no original cpu limit recorded for %s (patch_cpu_limit must run first)", step.Target)
	}

	deploy, err := client.AppsV1().Deployments(namespace).Get(ctx, step.Target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", step.Target, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("deployment %s has no containers", step.Target)
	}

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"resources":{"limits":{"cpu":%q}}}]}}}}`,
		containers[0].Name, orig)
	_, err = client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

// scaleDeployment and resetScale exist for case #7 (cache down): redis
// isn't a chain-service instance and has no /_fault endpoint, so
// taking it down means scaling its Deployment to 0 directly — same
// class of fault as patch_cpu_limit (case #6), same isolation
// reasoning applies (see patchCPULimit's doc comment).
func scaleDeployment(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]int32) error {
	replicas := paramInt(step.Params["replicas"], -1)
	if replicas < 0 {
		return fmt.Errorf("scale_deployment requires params.replicas >= 0")
	}

	deploy, err := client.AppsV1().Deployments(namespace).Get(ctx, step.Target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", step.Target, err)
	}
	if deploy.Spec.Replicas != nil {
		original[step.Target] = *deploy.Spec.Replicas
	} else {
		original[step.Target] = 1
	}

	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	if _, err := client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

func resetScale(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]int32) error {
	orig, ok := original[step.Target]
	if !ok {
		return fmt.Errorf("no original replica count recorded for %s (scale_deployment must run first)", step.Target)
	}
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, orig)
	if _, err := client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

// removeEnvVar and restoreEnvVar exist for case #12 (config error): a
// required env var (CONFIG_PATH) removed from the Deployment's pod
// template. Unlike case #5's mem_leak, this fault is persistent across
// pod restarts on its own — it's baked into the Deployment spec, not
// in-memory app state — so no sustain_fault trick is needed; the pod
// just keeps CrashLoopBackOff-ing until restore_env_var runs.
//
// env's patchMergeKey is "name", so a strategic merge patch merges by
// name rather than replacing the whole list — meaning removal needs
// the explicit `"$patch":"delete"` directive (a plain patch would only
// ever add/update entries, never remove one), while restoring is just
// a normal named-entry patch.
func removeEnvVar(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]string) error {
	key, _ := step.Params["key"].(string)
	if key == "" {
		return fmt.Errorf("remove_env_var requires params.key")
	}

	deploy, err := client.AppsV1().Deployments(namespace).Get(ctx, step.Target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", step.Target, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("deployment %s has no containers", step.Target)
	}
	for _, e := range containers[0].Env {
		if e.Name == key {
			original[step.Target+"/"+key] = e.Value
			break
		}
	}

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"env":[{"name":%q,"$patch":"delete"}]}]}}}}`,
		containers[0].Name, key)
	if _, err := client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

func restoreEnvVar(ctx context.Context, client *kubernetes.Clientset, namespace string, step scenario.Step, original map[string]string) error {
	key, _ := step.Params["key"].(string)
	if key == "" {
		return fmt.Errorf("restore_env_var requires params.key")
	}
	val, ok := original[step.Target+"/"+key]
	if !ok {
		return fmt.Errorf("no original value recorded for %s/%s (remove_env_var must run first)", step.Target, key)
	}

	deploy, err := client.AppsV1().Deployments(namespace).Get(ctx, step.Target, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", step.Target, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("deployment %s has no containers", step.Target)
	}

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":%q,"env":[{"name":%q,"value":%q}]}]}}}}`,
		containers[0].Name, key, val)
	if _, err := client.AppsV1().Deployments(namespace).Patch(ctx, step.Target, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment %s: %w", step.Target, err)
	}
	return nil
}

// publishPoisonMessages exists for case #10: order's own publish()
// always writes a well-formed message (internal/chain/handler.go
// always sets "published_at"), so a poison message has to be injected
// directly via a redis client — no /_fault knob can reach this, since
// it's not the demo services' own behavior that's misbehaving, it's
// an externally-injected bad message.
//
// count > 1 matters: internal/chain/consumer.processEntry panics
// (crashing the whole notify-worker process, undoing the read via
// XReadGroup's "0" recovery, see the package doc) on a missing
// "published_at" field, but a single crash's restart has no backoff
// delay (only the *second+* restart does) — nowhere near enough
// downtime to cross DeploymentUnavailable's for:30s threshold. Each
// XReadGroup call fetches up to 10 entries at once and panics on the
// first poison one in that batch, leaving the rest still pending for
// the *next* restart to immediately re-hit — a large enough burst
// keeps that restart-panic-restart cycle going long enough for the
// growing crash-backoff delays to actually accumulate real downtime.
func publishPoisonMessages(ctx context.Context, step scenario.Step) error {
	addr, _ := step.Params["addr"].(string)
	if addr == "" {
		return fmt.Errorf("publish_poison_message requires params.addr")
	}
	streamName, _ := step.Params["stream"].(string)
	if streamName == "" {
		return fmt.Errorf("publish_poison_message requires params.stream")
	}
	count := paramInt(step.Params["count"], 1)

	cli := redis.NewClient(&redis.Options{Addr: addr})
	defer cli.Close()

	for i := 0; i < count; i++ {
		// Deliberately missing "published_at" — the exact field
		// consumer.processEntry checks for.
		if err := cli.XAdd(ctx, &redis.XAddArgs{
			Stream: streamName,
			Values: map[string]any{"malformed": "true"},
		}).Err(); err != nil {
			return fmt.Errorf("xadd poison message %d: %w", i, err)
		}
	}
	return nil
}

func paramInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return fallback
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
