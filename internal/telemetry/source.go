// Package telemetry defines Source, the single interface between
// context-builder and whatever telemetry backend is in use. See
// CLAUDE.md §6.2 — this is the isolation boundary that keeps all
// ClickHouse-specific SQL out of internal/builder.
package telemetry

import (
	"context"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

// Window reuses contextpack.Window: the query time range plus the
// service that raised the alert (the narrowing starting point, §6.2).
type Window = contextpack.Window

type SpanAgg struct {
	ServiceName, SpanName string
	P50, P95, P99         time.Duration
	Errors, Total         uint64
}

type LogLine struct {
	Timestamp time.Time
	TraceID   string
	Service   string
	Severity  string
	Body      string
}

type ChangeRecord struct {
	Timestamp   time.Time
	Service     string
	Description string
}

// Source is the only place context-builder talks to a telemetry
// backend. A production deployment on Datadog/Elastic/Grafana Cloud
// needs a new implementation of this interface only — internal/builder
// never changes. See CLAUDE.md §6.2, §12.
type Source interface {
	TopErrorSpans(ctx context.Context, w Window, limit int) ([]SpanAgg, error)
	DependencyGraph(ctx context.Context, w Window, hops int) (contextpack.Graph, error)
	ExemplarTraces(ctx context.Context, w Window, n int) ([]contextpack.TraceDetail, error)
	LogsForTraces(ctx context.Context, traceIDs []string) ([]LogLine, error)
	LogsByService(ctx context.Context, w Window) ([]LogLine, error)
	MetricDiffVsBaseline(ctx context.Context, w Window) ([]contextpack.MetricDelta, error)
	K8sEvents(ctx context.Context, w Window) ([]contextpack.K8sEvent, error)
	RecentChanges(ctx context.Context, w Window) ([]ChangeRecord, error)
}
