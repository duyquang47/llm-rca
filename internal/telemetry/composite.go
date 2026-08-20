package telemetry

import (
	"context"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

// tracesLogsSource is what clickhouse.Source implements: every
// Source method except MetricDiffVsBaseline.
type tracesLogsSource interface {
	TopErrorSpans(ctx context.Context, w Window, limit int) ([]SpanAgg, error)
	DependencyGraph(ctx context.Context, w Window, hops int) (contextpack.Graph, error)
	ExemplarTraces(ctx context.Context, w Window, n int) ([]contextpack.TraceDetail, error)
	LogsForTraces(ctx context.Context, traceIDs []string) ([]LogLine, error)
	LogsByService(ctx context.Context, w Window) ([]LogLine, error)
	K8sEvents(ctx context.Context, w Window) ([]contextpack.K8sEvent, error)
	RecentChanges(ctx context.Context, w Window) ([]ChangeRecord, error)
}

type metricsSource interface {
	MetricDiffVsBaseline(ctx context.Context, w Window) ([]contextpack.MetricDelta, error)
}

// CompositeSource combines a traces+logs backend (ClickHouse) with a
// metrics backend (Prometheus) into the full Source interface, per
// CLAUDE.md §6.2: "Implementation ở POC: clickhouse.Source (traces +
// logs) và prometheus.Source (metrics), gộp lại bằng một CompositeSource."
type CompositeSource struct {
	tracesLogsSource
	metricsSource
}

func NewComposite(tl tracesLogsSource, m metricsSource) *CompositeSource {
	return &CompositeSource{tracesLogsSource: tl, metricsSource: m}
}

var _ Source = (*CompositeSource)(nil)
