// Command orchestrator is incident-orchestrator: receives Alertmanager
// webhooks, dedupes/groups alerts (internal/incident.Grouper), runs
// the RECEIVED->COLLECTING->ANALYZING->DONE/FAILED state machine, and
// on each new incident calls internal/builder + the LLM, storing the
// result in ClickHouse's incidents table. See CLAUDE.md §6.1.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/duyquang47/llm-rca/internal/builder"
	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/incident"
	"github.com/duyquang47/llm-rca/internal/llm"
	"github.com/duyquang47/llm-rca/internal/llm/gemini"
	"github.com/duyquang47/llm-rca/internal/prompt"
	"github.com/duyquang47/llm-rca/internal/telemetry"
	chsource "github.com/duyquang47/llm-rca/internal/telemetry/clickhouse"
	promsource "github.com/duyquang47/llm-rca/internal/telemetry/prometheus"
)

// collectionWindow is how long incident-orchestrator waits after the
// first alert before building the context pack — lets more alerts
// from the same incident group in first, per CLAUDE.md §6.1 ("một sự
// cố sinh ~20 alert... gọi LLM 20 lần vừa tốn tiền vừa ra 20 kết luận
// mâu thuẫn").
const collectionWindow = 20 * time.Second

// graphRefreshInterval controls how often the grouper's dependency
// graph is refreshed; a stale graph makes case #13-style grouping
// decisions silently wrong as the topology changes.
const graphRefreshInterval = time.Minute

type alertmanagerWebhook struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
	} `json:"alerts"`
}

type server struct {
	grouper     *incident.Grouper
	source      telemetry.Source
	store       *incident.Store
	manualScans *incident.ManualScanStore
	logger      *slog.Logger
	llm         *gemini.Client
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	chAddr := envOr("CLICKHOUSE_ADDR", "clickhouse.observability:9000")
	chUser := envOr("CLICKHOUSE_USER", "otel")
	chPassword := envOr("CLICKHOUSE_PASSWORD", "otel")
	chDatabase := envOr("CLICKHOUSE_DATABASE", "otel")
	promURL := envOr("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.observability:9090")
	listen := envOr("LISTEN_ADDR", ":8080")
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is required")
	}
	model := envOr("GEMINI_MODEL", "gemini-3.6-flash")

	chSource, err := chsource.New(chAddr, chDatabase, chUser, chPassword)
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	promSource := promsource.New(promURL)
	source := telemetry.NewComposite(chSource, promSource)

	rawConn, err := ch.Open(&ch.Options{Addr: []string{chAddr}, Auth: ch.Auth{Database: chDatabase, Username: chUser, Password: chPassword}})
	if err != nil {
		return fmt.Errorf("connect clickhouse (store): %w", err)
	}
	store := incident.NewStore(rawConn)
	ctx := context.Background()
	if err := store.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure incidents schema: %w", err)
	}

	manualScans := incident.NewManualScanStore(rawConn)
	if err := manualScans.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure manual_scans schema: %w", err)
	}

	srv := &server{
		grouper:     incident.NewGrouper(),
		source:      source,
		store:       store,
		manualScans: manualScans,
		logger:      logger,
		llm:         gemini.New(apiKey, model),
	}

	go srv.refreshGraphLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", srv.handleWebhook)
	mux.HandleFunc("/webhook/manual", srv.handleWebhookManual)
	mux.HandleFunc("GET /incidents", srv.handleListIncidents)
	mux.HandleFunc("GET /incidents/{id}", srv.handleGetIncident)
	mux.HandleFunc("POST /incidents/{id}/scan-service", srv.handleScanService)
	mux.HandleFunc("POST /incidents/{id}/scan-k8s", srv.handleScanK8s)
	mux.HandleFunc("POST /incidents/{id}/analyze", srv.handleAnalyze)
	logger.Info("incident-orchestrator listening", "addr", listen)
	return http.ListenAndServe(listen, mux)
}

func (s *server) refreshGraphLoop(ctx context.Context) {
	refresh := func() {
		graph, err := s.source.DependencyGraph(ctx, telemetry.Window{
			From: time.Now().Add(-30 * time.Minute), To: time.Now(),
		}, 3)
		if err != nil {
			s.logger.Error("refresh dependency graph failed", "err", err)
			return
		}
		s.grouper.SetGraph(graph)
	}
	refresh()
	for range time.Tick(graphRefreshInterval) {
		refresh()
	}
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload alertmanagerWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid webhook payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, a := range payload.Alerts {
		if a.Status != "firing" {
			continue
		}
		service := a.Labels["service"]
		if service == "" {
			continue
		}
		alert := incident.Alert{
			Name:     a.Labels["alertname"],
			Service:  service,
			Severity: a.Labels["severity"],
			Message:  a.Annotations["summary"],
			FiredAt:  a.StartsAt,
		}
		inc, isNew := s.grouper.Ingest(alert)
		s.logger.Info("alert ingested", "incident_id", inc.ID, "service", service, "is_new_incident", isNew)
		if isNew {
			go s.analyze(context.Background(), inc)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// analyze runs the COLLECTING->ANALYZING->DONE/FAILED tail of the
// state machine for one incident, exactly once (triggered only when
// Ingest reports a brand-new incident).
func (s *server) analyze(ctx context.Context, inc *incident.Incident) {
	log := s.logger.With("incident_id", inc.ID)

	inc.State = incident.StateCollecting
	log.Info("collecting", "window_seconds", collectionWindow.Seconds())
	time.Sleep(collectionWindow)

	inc.State = incident.StateAnalyzing
	log.Info("analyzing", "services", inc.Services)

	pack, err := builder.Build(ctx, s.source, windowFor(inc), alertSummaryFor(inc), inc.ID)
	if err != nil {
		log.Error("build context pack failed", "err", err)
		inc.State = incident.StateFailed
		return
	}

	p, err := prompt.Serialize(pack)
	if err != nil {
		log.Error("serialize prompt failed", "err", err)
		inc.State = incident.StateFailed
		return
	}

	start := time.Now()
	result, usage, err := s.llm.Analyze(ctx, p)
	latency := time.Since(start)
	if err != nil {
		log.Error("llm analyze failed", "err", err)
		inc.State = incident.StateFailed
		return
	}

	packJSON, _ := json.Marshal(pack)
	resultJSON, _ := json.Marshal(result)
	if err := s.store.Insert(ctx, incident.Record{
		IncidentID:    inc.ID,
		ScenarioName:  inc.Services[0],
		StartedAtUnix: inc.StartedAt.Unix(),
		ContextPack:   string(packJSON),
		PackVersion:   pack.Version,
		PromptVersion: "v1",
		LLMModel:      s.llm.Model(),
		LLMOutput:     string(resultJSON),
		TokenCount:    uint32(usage.InputTokens + usage.OutputTokens),
		LatencyMs:     uint32(latency.Milliseconds()),
		Mode:          "auto",
	}); err != nil {
		log.Error("store incident failed", "err", err)
		inc.State = incident.StateFailed
		return
	}

	inc.State = incident.StateDone
	log.Info("done", "top_cause", topCause(result), "latency_ms", latency.Milliseconds())
}

// windowFor and alertSummaryFor are shared by the auto path's
// analyze() and the manual path's handlers in manual.go — both derive
// the same telemetry.Window/contextpack.AlertSummary from an
// Incident's first-alert/primary-service data.
func windowFor(inc *incident.Incident) telemetry.Window {
	return telemetry.Window{
		From:    inc.StartedAt.Add(-2 * time.Minute),
		To:      time.Now(),
		Service: inc.Services[0],
	}
}

func alertSummaryFor(inc *incident.Incident) contextpack.AlertSummary {
	return contextpack.AlertSummary{
		Name:     inc.Alerts[0].Name,
		Service:  inc.Services[0],
		Severity: inc.Alerts[0].Severity,
		Message:  inc.Alerts[0].Message,
	}
}

func topCause(result llm.RCAResult) string {
	if len(result.RankedCauses) == 0 {
		return ""
	}
	return fmt.Sprintf("%s (%s)", result.RankedCauses[0].Service, result.RankedCauses[0].Cause)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
