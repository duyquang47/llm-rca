// Package builder assembles a real ContextPack from telemetry.Source
// query results, replacing Phase 0's hand-written fixture. See
// CLAUDE.md §6.4.
//
// Scope note: this covers the exemplar-diversity dedup rule (§6.8,
// already implemented in telemetry/clickhouse) and literal log dedup
// via Drain, plus a sustained-vs-burst rate_shape heuristic. It does
// NOT yet implement baseline-count comparison for log templates
// (§6.4: "chỉ đưa vào context pack các template có tần suất tăng đột
// biến so với baseline") — there's no historical Drain state to diff
// against yet, so BaselineCount is always 0 and templates are instead
// capped to the top N by count. Nor does it implement narrowing across
// dependency-graph hops (§6.2) — DependencyGraph/MetricDiffs/K8sEvents
// are pulled for the whole window, not yet scoped outward from the
// alerting service. Both are real gaps to close before this is used
// for the 13-scenario benchmark, not just missing polish.
package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/drain"
	"github.com/duyquang47/llm-rca/internal/telemetry"
)

const (
	packVersion     = "v1"
	maxLogTemplates = 10
	maxExemplars    = 3
	maxK8sEvents    = 50
)

// ServiceScan holds the "service" scan group (metric/trace/log) —
// what `rcactl scan <id> service` collects and what the auto path
// always collects as half of Build.
type ServiceScan struct {
	MetricDiffs  []contextpack.MetricDelta  `json:"metric_diffs"`
	Exemplars    []contextpack.TraceDetail  `json:"exemplar_traces"`
	LogTemplates []contextpack.TemplateStat `json:"log_templates"`
}

// K8sScan holds the "k8s" scan group (dependency graph/events) — what
// `rcactl scan <id> k8s` collects and what the auto path always
// collects as the other half of Build.
type K8sScan struct {
	DependencyGraph contextpack.Graph      `json:"dependency_graph"`
	Events          []contextpack.K8sEvent `json:"events"`
	EventsTruncated bool                   `json:"events_truncated"`
}

// ScanService runs the metric/trace/log portion of context collection
// on its own, independent of ScanK8s — the manual investigation flow
// lets an SRE request this without the other.
func ScanService(ctx context.Context, source telemetry.Source, w telemetry.Window) (ServiceScan, error) {
	metricDiffs, err := source.MetricDiffVsBaseline(ctx, w)
	if err != nil {
		return ServiceScan{}, fmt.Errorf("metric diffs: %w", err)
	}

	exemplars, err := source.ExemplarTraces(ctx, w, maxExemplars)
	if err != nil {
		return ServiceScan{}, fmt.Errorf("exemplar traces: %w", err)
	}

	var traceIDs []string
	for _, e := range exemplars {
		traceIDs = append(traceIDs, e.TraceID)
	}
	logs, err := source.LogsForTraces(ctx, traceIDs)
	if err != nil {
		return ServiceScan{}, fmt.Errorf("logs for traces: %w", err)
	}
	if len(logs) == 0 {
		// Fallback CLAUDE.md §6.2 requires when a service logs without
		// trace_id.
		logs, err = source.LogsByService(ctx, w)
		if err != nil {
			return ServiceScan{}, fmt.Errorf("logs by service: %w", err)
		}
	}

	return ServiceScan{
		MetricDiffs:  metricDiffs,
		Exemplars:    exemplars,
		LogTemplates: clusterLogs(logs),
	}, nil
}

// ScanK8s runs the dependency-graph/events portion of context
// collection on its own, independent of ScanService.
func ScanK8s(ctx context.Context, source telemetry.Source, w telemetry.Window) (K8sScan, error) {
	graph, err := source.DependencyGraph(ctx, w, 2)
	if err != nil {
		return K8sScan{}, fmt.Errorf("dependency graph: %w", err)
	}

	events, truncated, err := source.K8sEvents(ctx, w, maxK8sEvents)
	if err != nil {
		return K8sScan{}, fmt.Errorf("k8s events: %w", err)
	}

	return K8sScan{DependencyGraph: graph, Events: events, EventsTruncated: truncated}, nil
}

// Assemble builds a ContextPack from whichever scans were performed.
// A nil svc or k8s means "not scanned" (manual flow, SRE chose not to
// run that group) — distinct from a scan that ran and came back
// empty — and is recorded in pack.NotScanned instead of silently
// leaving the section zero-valued.
func Assemble(w telemetry.Window, alert contextpack.AlertSummary, incidentID string, svc *ServiceScan, k8s *K8sScan) (contextpack.ContextPack, error) {
	pack := contextpack.ContextPack{
		Version:    packVersion,
		IncidentID: incidentID,
		Window:     w,
		Alert:      alert,
	}

	if svc != nil {
		pack.MetricDiffs = svc.MetricDiffs
		pack.Exemplars = svc.Exemplars
		pack.LogTemplates = svc.LogTemplates
	} else {
		pack.NotScanned = append(pack.NotScanned, "metric_diffs", "exemplar_traces", "log_templates")
	}

	if k8s != nil {
		pack.DependencyGraph = k8s.DependencyGraph
		pack.K8sContext = contextpack.K8sContext{Events: k8s.Events}
		if k8s.EventsTruncated {
			pack.Truncated = append(pack.Truncated, "k8s_context.events")
		}
	} else {
		pack.NotScanned = append(pack.NotScanned, "dependency_graph", "k8s_context")
	}

	raw, err := json.Marshal(pack)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("estimate token count: %w", err)
	}
	pack.TokenEstimate = len(raw) / 4 // rough chars-per-token approximation

	return pack, nil
}

// Build runs both scan groups and assembles a full pack — the auto
// path's entry point, unchanged in behavior from before this file was
// split: same fields collected, same assembly order, same
// TokenEstimate computation.
func Build(ctx context.Context, source telemetry.Source, w telemetry.Window, alert contextpack.AlertSummary, incidentID string) (contextpack.ContextPack, error) {
	svc, err := ScanService(ctx, source, w)
	if err != nil {
		return contextpack.ContextPack{}, err
	}
	k8s, err := ScanK8s(ctx, source, w)
	if err != nil {
		return contextpack.ContextPack{}, err
	}
	return Assemble(w, alert, incidentID, &svc, &k8s)
}

// clusterLogs runs every log line through Drain, aggregates each
// resulting template with the by_service/first_seen/last_seen/
// rate_shape fields §6.8 requires, and caps the result to the top N
// most frequent templates.
func clusterLogs(logs []telemetry.LogLine) []contextpack.TemplateStat {
	d := drain.New()

	type agg struct {
		template  string
		count     int
		firstSeen time.Time
		lastSeen  time.Time
		byService map[string]int
		targets   map[string]int
	}
	byTemplateID := map[int]*agg{}

	for _, l := range logs {
		id, template := d.Add(l.Body)
		a, ok := byTemplateID[id]
		if !ok {
			a = &agg{template: template, byService: map[string]int{}, targets: map[string]int{}, firstSeen: l.Timestamp, lastSeen: l.Timestamp}
			byTemplateID[id] = a
		}
		a.count++
		a.byService[l.Service]++
		// Kept separate from the template text on purpose — Drain
		// would just wildcard a target name away as one short varying
		// token among an otherwise-identical message (see contextpack.
		// TemplateStat.Targets doc comment for why this matters).
		if l.Target != "" {
			a.targets[l.Target]++
		}
		if l.Timestamp.Before(a.firstSeen) {
			a.firstSeen = l.Timestamp
		}
		if l.Timestamp.After(a.lastSeen) {
			a.lastSeen = l.Timestamp
		}
	}

	var stats []contextpack.TemplateStat
	id := 0
	for _, a := range byTemplateID {
		id++
		targets := a.targets
		if len(targets) == 0 {
			targets = nil // omitempty in the JSON tag needs a true nil, not an empty map
		}
		stats = append(stats, contextpack.TemplateStat{
			TemplateID: id,
			Template:   a.template,
			Count:      a.count,
			FirstSeen:  a.firstSeen,
			LastSeen:   a.lastSeen,
			ByService:  a.byService,
			Targets:    targets,
			RateShape:  rateShape(a.firstSeen, a.lastSeen),
		})
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].Count > stats[j].Count })
	if len(stats) > maxLogTemplates {
		stats = stats[:maxLogTemplates]
	}
	return stats
}

// rateShape distinguishes a sudden collapse from a slow resource leak
// (CLAUDE.md §6.8): a burst concentrated in a short span vs one spread
// across most of the observation window, at the same count, implies
// different root causes.
func rateShape(first, last time.Time) string {
	if last.Sub(first) < 15*time.Second {
		return "burst"
	}
	return "sustained"
}
