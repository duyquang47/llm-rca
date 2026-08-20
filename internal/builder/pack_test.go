package builder

import (
	"context"
	"testing"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/telemetry"
)

// fakeSource is a hand-rolled telemetry.Source for testing Build
// without a live ClickHouse/Prometheus — the equivalent of the
// testdata-fixture approach CLAUDE.md §1.2 describes for the offline
// builder tests, just inline since there's only one test file so far.
type fakeSource struct {
	graph       contextpack.Graph
	metricDiffs []contextpack.MetricDelta
	exemplars   []contextpack.TraceDetail
	logs        []telemetry.LogLine
	events      []contextpack.K8sEvent
}

func (f *fakeSource) TopErrorSpans(context.Context, telemetry.Window, int) ([]telemetry.SpanAgg, error) {
	return nil, nil
}
func (f *fakeSource) DependencyGraph(context.Context, telemetry.Window, int) (contextpack.Graph, error) {
	return f.graph, nil
}
func (f *fakeSource) ExemplarTraces(context.Context, telemetry.Window, int) ([]contextpack.TraceDetail, error) {
	return f.exemplars, nil
}
func (f *fakeSource) LogsForTraces(context.Context, []string) ([]telemetry.LogLine, error) {
	return f.logs, nil
}
func (f *fakeSource) LogsByService(context.Context, telemetry.Window) ([]telemetry.LogLine, error) {
	return nil, nil // never hit in these tests: LogsForTraces already returns data
}
func (f *fakeSource) MetricDiffVsBaseline(context.Context, telemetry.Window) ([]contextpack.MetricDelta, error) {
	return f.metricDiffs, nil
}
func (f *fakeSource) K8sEvents(context.Context, telemetry.Window) ([]contextpack.K8sEvent, error) {
	return f.events, nil
}
func (f *fakeSource) RecentChanges(context.Context, telemetry.Window) ([]telemetry.ChangeRecord, error) {
	return nil, nil
}

var _ telemetry.Source = (*fakeSource)(nil)

func TestBuild_ClustersRepeatedLogsIntoOneTemplate(t *testing.T) {
	now := time.Now()
	src := &fakeSource{
		exemplars: []contextpack.TraceDetail{{TraceID: "abc"}},
		logs: []telemetry.LogLine{
			{Timestamp: now, TraceID: "abc", Service: "inventory", Body: "failed to acquire connection: timeout after 120ms"},
			{Timestamp: now.Add(time.Second), TraceID: "abc", Service: "inventory", Body: "failed to acquire connection: timeout after 340ms"},
			{Timestamp: now.Add(2 * time.Second), TraceID: "abc", Service: "inventory", Body: "failed to acquire connection: timeout after 500ms"},
		},
	}

	pack, err := Build(context.Background(), src, telemetry.Window{Service: "gateway"}, contextpack.AlertSummary{}, "incident-1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(pack.LogTemplates) != 1 {
		t.Fatalf("want 1 clustered template from 3 near-identical lines, got %d: %+v", len(pack.LogTemplates), pack.LogTemplates)
	}
	tpl := pack.LogTemplates[0]
	if tpl.Count != 3 {
		t.Errorf("want count 3, got %d", tpl.Count)
	}
	if tpl.ByService["inventory"] != 3 {
		t.Errorf("want all 3 attributed to inventory, got %+v", tpl.ByService)
	}
}

func TestBuild_FallsBackToLogsByServiceWhenNoTraceLogsFound(t *testing.T) {
	src := &fakeSource{
		exemplars: []contextpack.TraceDetail{{TraceID: "abc"}},
		logs:      nil, // LogsForTraces returns empty -> Build must fall back
	}

	pack, err := Build(context.Background(), src, telemetry.Window{Service: "gateway"}, contextpack.AlertSummary{}, "incident-2")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pack.LogTemplates) != 0 {
		t.Errorf("fake LogsByService returns nothing, so want 0 templates, got %d", len(pack.LogTemplates))
	}
}

func TestBuild_SetsVersionAndTokenEstimate(t *testing.T) {
	src := &fakeSource{}
	pack, err := Build(context.Background(), src, telemetry.Window{Service: "gateway"}, contextpack.AlertSummary{Service: "gateway"}, "incident-3")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if pack.Version != "v1" {
		t.Errorf("want version v1, got %q", pack.Version)
	}
	if pack.TokenEstimate <= 0 {
		t.Errorf("want a positive token estimate, got %d", pack.TokenEstimate)
	}
	if pack.IncidentID != "incident-3" {
		t.Errorf("want incident id threaded through, got %q", pack.IncidentID)
	}
}

func TestBuild_CapsLogTemplatesAtMax(t *testing.T) {
	now := time.Now()
	src := &fakeSource{exemplars: []contextpack.TraceDetail{{TraceID: "abc"}}}
	// 15 totally distinct single-word messages -> 15 distinct templates,
	// should be capped to maxLogTemplates.
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
		"india", "juliet", "kilo", "lima", "mike", "november", "oscar"}
	for _, w := range words {
		src.logs = append(src.logs, telemetry.LogLine{Timestamp: now, TraceID: "abc", Service: "svc", Body: w})
	}

	pack, err := Build(context.Background(), src, telemetry.Window{Service: "gateway"}, contextpack.AlertSummary{}, "incident-4")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pack.LogTemplates) != maxLogTemplates {
		t.Errorf("want capped at %d templates, got %d", maxLogTemplates, len(pack.LogTemplates))
	}
}
