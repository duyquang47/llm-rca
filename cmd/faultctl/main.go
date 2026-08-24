// Command faultctl reads a scenario YAML (CLAUDE.md §7), executes its
// steps against the demo namespace's /_fault endpoints, and records
// ground truth to a store isolated from context-builder/orchestrator.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

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
