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
	ctx := context.Background()

	for i, step := range sc.Steps {
		fmt.Printf("[%s] step %d: action=%s target=%s\n", sc.Name, i+1, step.Action, step.Target)
		switch step.Action {
		case "set_config":
			if err := setConfig(client, *demoNamespace, step); err != nil {
				return fmt.Errorf("step %d (set_config): %w", i+1, err)
			}
		case "reset":
			if err := resetConfig(client, *demoNamespace, step); err != nil {
				return fmt.Errorf("step %d (reset): %w", i+1, err)
			}
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
			if err := resetCPULimit(ctx, k8sClient, *demoNamespace, step, originalCPULimit); err != nil {
				return fmt.Errorf("step %d (reset_cpu_limit): %w", i+1, err)
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
