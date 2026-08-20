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
)

func Build(ctx context.Context, source telemetry.Source, w telemetry.Window, alert contextpack.AlertSummary, incidentID string) (contextpack.ContextPack, error) {
	graph, err := source.DependencyGraph(ctx, w, 2)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("dependency graph: %w", err)
	}

	metricDiffs, err := source.MetricDiffVsBaseline(ctx, w)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("metric diffs: %w", err)
	}

	exemplars, err := source.ExemplarTraces(ctx, w, maxExemplars)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("exemplar traces: %w", err)
	}

	var traceIDs []string
	for _, e := range exemplars {
		traceIDs = append(traceIDs, e.TraceID)
	}
	logs, err := source.LogsForTraces(ctx, traceIDs)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("logs for traces: %w", err)
	}
	if len(logs) == 0 {
		// Fallback CLAUDE.md §6.2 requires when a service logs without
		// trace_id.
		logs, err = source.LogsByService(ctx, w)
		if err != nil {
			return contextpack.ContextPack{}, fmt.Errorf("logs by service: %w", err)
		}
	}

	events, err := source.K8sEvents(ctx, w)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("k8s events: %w", err)
	}

	templates := clusterLogs(logs)

	pack := contextpack.ContextPack{
		Version:         packVersion,
		IncidentID:      incidentID,
		Window:          w,
		Alert:           alert,
		DependencyGraph: graph,
		MetricDiffs:     metricDiffs,
		Exemplars:       exemplars,
		LogTemplates:    templates,
		K8sContext:      contextpack.K8sContext{Events: events},
	}

	raw, err := json.Marshal(pack)
	if err != nil {
		return contextpack.ContextPack{}, fmt.Errorf("estimate token count: %w", err)
	}
	pack.TokenEstimate = len(raw) / 4 // rough chars-per-token approximation

	return pack, nil
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
	}
	byTemplateID := map[int]*agg{}

	for _, l := range logs {
		id, template := d.Add(l.Body)
		a, ok := byTemplateID[id]
		if !ok {
			a = &agg{template: template, byService: map[string]int{}, firstSeen: l.Timestamp, lastSeen: l.Timestamp}
			byTemplateID[id] = a
		}
		a.count++
		a.byService[l.Service]++
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
		stats = append(stats, contextpack.TemplateStat{
			TemplateID: id,
			Template:   a.template,
			Count:      a.count,
			FirstSeen:  a.firstSeen,
			LastSeen:   a.lastSeen,
			ByService:  a.byService,
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
