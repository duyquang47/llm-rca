// Command builder queries live ClickHouse+Prometheus, assembles a real
// ContextPack (internal/builder), and — same as cmd/phase0 — calls an
// LLM with it. This is the Phase 1 successor to Phase 0's hand-written
// fixture: same prompt/LLM plumbing, real data instead.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/duyquang47/llm-rca/internal/builder"
	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/llm/gemini"
	"github.com/duyquang47/llm-rca/internal/prompt"
	"github.com/duyquang47/llm-rca/internal/telemetry"
	chsource "github.com/duyquang47/llm-rca/internal/telemetry/clickhouse"
	promsource "github.com/duyquang47/llm-rca/internal/telemetry/prometheus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	service := flag.String("service", "gateway", "service that raised the alert (narrowing start point)")
	since := flag.Duration("since", 10*time.Minute, "how far back the window starts")
	callLLM := flag.Bool("llm", false, "also call the LLM with the built pack (needs GEMINI_API_KEY)")
	flag.Parse()

	chAddr := envOr("CLICKHOUSE_ADDR", "localhost:9000")
	chUser := envOr("CLICKHOUSE_USER", "otel")
	chPassword := envOr("CLICKHOUSE_PASSWORD", "otel")
	chDatabase := envOr("CLICKHOUSE_DATABASE", "otel")
	promURL := envOr("PROMETHEUS_URL", "http://localhost:9090")

	ch, err := chsource.New(chAddr, chDatabase, chUser, chPassword)
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	prom := promsource.New(promURL)
	source := telemetry.NewComposite(ch, prom)

	now := time.Now()
	w := telemetry.Window{From: now.Add(-*since), To: now, Service: *service}
	alert := contextpack.AlertSummary{
		Name: "ManualBuilderRun", Service: *service, Severity: "info",
		Message: fmt.Sprintf("manually triggered builder run for %s", *service),
	}

	ctx := context.Background()
	pack, err := builder.Build(ctx, source, w, alert, fmt.Sprintf("manual-%d", now.Unix()))
	if err != nil {
		return fmt.Errorf("build context pack: %w", err)
	}

	pretty, _ := json.MarshalIndent(pack, "", "  ")
	fmt.Println("===== CONTEXT PACK (thật, từ ClickHouse/Prometheus) =====")
	fmt.Println(string(pretty))
	fmt.Printf("\n===== token_estimate=%d log_templates=%d exemplar_traces=%d metric_diffs=%d k8s_events=%d =====\n",
		pack.TokenEstimate, len(pack.LogTemplates), len(pack.Exemplars), len(pack.MetricDiffs), len(pack.K8sContext.Events))

	if !*callLLM {
		return nil
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY chưa được set")
	}
	model := envOr("GEMINI_MODEL", "gemini-3.6-flash")

	p, err := prompt.Serialize(pack)
	if err != nil {
		return fmt.Errorf("serialize prompt: %w", err)
	}

	client := gemini.New(apiKey, model)
	result, usage, err := client.Analyze(ctx, p)
	if err != nil {
		return fmt.Errorf("analyze: %w", err)
	}

	fmt.Println("\n===== KẾT QUẢ RCAResult =====")
	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(resultJSON))
	fmt.Printf("\n===== USAGE ===== model=%s input_tokens=%d output_tokens=%d\n", client.Model(), usage.InputTokens, usage.OutputTokens)

	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
