// Package clickhouse is the only place allowed to contain ClickHouse
// SQL (CLAUDE.md §6.2). It implements every telemetry.Source method
// except MetricDiffVsBaseline, which prometheus.Source covers —
// telemetry.CompositeSource combines the two into the full interface.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/telemetry"
)

type Source struct {
	conn ch.Conn
}

func New(addr, database, username, password string) (*Source, error) {
	conn, err := ch.Open(&ch.Options{
		Addr: []string{addr},
		Auth: ch.Auth{Database: database, Username: username, Password: password},
	})
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	return &Source{conn: conn}, nil
}

func (s *Source) TopErrorSpans(ctx context.Context, w telemetry.Window, limit int) ([]telemetry.SpanAgg, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT ServiceName, SpanName,
		       quantile(0.50)(Duration) AS p50,
		       quantile(0.95)(Duration) AS p95,
		       quantile(0.99)(Duration) AS p99,
		       countIf(StatusCode = 'Error') AS errors,
		       count() AS total
		FROM otel_traces
		WHERE Timestamp BETWEEN ? AND ?
		GROUP BY ServiceName, SpanName
		ORDER BY errors DESC, p99 DESC
		LIMIT ?`, w.From, w.To, limit)
	if err != nil {
		return nil, fmt.Errorf("query top error spans: %w", err)
	}
	defer rows.Close()

	var out []telemetry.SpanAgg
	for rows.Next() {
		var (
			svc, span     string
			p50, p95, p99 float64
			errors, total uint64
		)
		if err := rows.Scan(&svc, &span, &p50, &p95, &p99, &errors, &total); err != nil {
			return nil, fmt.Errorf("scan span agg: %w", err)
		}
		out = append(out, telemetry.SpanAgg{
			ServiceName: svc, SpanName: span,
			P50: time.Duration(p50), P95: time.Duration(p95), P99: time.Duration(p99),
			Errors: errors, Total: total,
		})
	}
	return out, rows.Err()
}

// DependencyGraph infers service->service edges by self-joining spans
// on ParentSpanId within the same trace and keeping only hops that
// cross a service boundary (a client span calling into a different
// service's server span).
func (s *Source) DependencyGraph(ctx context.Context, w telemetry.Window, hops int) (contextpack.Graph, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT DISTINCT parent.ServiceName AS from_svc, child.ServiceName AS to_svc
		FROM otel_traces AS child
		INNER JOIN otel_traces AS parent
			ON child.TraceId = parent.TraceId AND child.ParentSpanId = parent.SpanId
		WHERE child.Timestamp BETWEEN ? AND ? AND child.ServiceName != parent.ServiceName`,
		w.From, w.To)
	if err != nil {
		return contextpack.Graph{}, fmt.Errorf("query dependency graph: %w", err)
	}
	defer rows.Close()

	var graph contextpack.Graph
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return contextpack.Graph{}, fmt.Errorf("scan edge: %w", err)
		}
		graph.Edges = append(graph.Edges, contextpack.GraphEdge{From: from, To: to})
	}
	return graph, rows.Err()
}

// ExemplarTraces implements the diversity rule from CLAUDE.md §6.8:
// the slowest trace, one with an error, and one baseline/normal trace
// — not just the top-N slowest, which tend to look identical.
func (s *Source) ExemplarTraces(ctx context.Context, w telemetry.Window, n int) ([]contextpack.TraceDetail, error) {
	traceIDs, err := s.pickExemplarTraceIDs(ctx, w, n)
	if err != nil {
		return nil, err
	}

	var out []contextpack.TraceDetail
	for _, id := range traceIDs {
		detail, err := s.traceDetail(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("trace detail %s: %w", id, err)
		}
		out = append(out, detail)
	}
	return out, nil
}

func (s *Source) pickExemplarTraceIDs(ctx context.Context, w telemetry.Window, n int) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	var slowest string
	if err := s.conn.QueryRow(ctx, `
		SELECT TraceId FROM otel_traces
		WHERE Timestamp BETWEEN ? AND ? AND ParentSpanId = ''
		ORDER BY Duration DESC LIMIT 1`, w.From, w.To).Scan(&slowest); err == nil {
		add(slowest)
	}

	var withError string
	if err := s.conn.QueryRow(ctx, `
		SELECT TraceId FROM otel_traces
		WHERE Timestamp BETWEEN ? AND ? AND StatusCode = 'Error'
		ORDER BY Duration DESC LIMIT 1`, w.From, w.To).Scan(&withError); err == nil {
		add(withError)
	}

	var baseline string
	if err := s.conn.QueryRow(ctx, `
		SELECT TraceId FROM otel_traces
		WHERE Timestamp BETWEEN ? AND ? AND ParentSpanId = '' AND StatusCode != 'Error'
		ORDER BY abs(Duration - (
			SELECT median(Duration) FROM otel_traces
			WHERE Timestamp BETWEEN ? AND ? AND ParentSpanId = ''
		)) ASC LIMIT 1`, w.From, w.To, w.From, w.To).Scan(&baseline); err == nil {
		add(baseline)
	}

	if len(ids) > n {
		ids = ids[:n]
	}
	return ids, nil
}

func (s *Source) traceDetail(ctx context.Context, traceID string) (contextpack.TraceDetail, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT SpanName, ServiceName, Duration, StatusCode
		FROM otel_traces WHERE TraceId = ?
		ORDER BY Timestamp ASC`, traceID)
	if err != nil {
		return contextpack.TraceDetail{}, err
	}
	defer rows.Close()

	var spans []contextpack.Span
	var maxDuration uint64
	for rows.Next() {
		var name, svc, status string
		var duration uint64
		if err := rows.Scan(&name, &svc, &duration, &status); err != nil {
			return contextpack.TraceDetail{}, err
		}
		spans = append(spans, contextpack.Span{
			Name: name, Service: svc,
			DurationMs: int64(duration / uint64(time.Millisecond)),
			StatusCode: status,
		})
		if duration > maxDuration {
			maxDuration = duration
		}
	}
	if err := rows.Err(); err != nil {
		return contextpack.TraceDetail{}, err
	}

	// Critical path heuristic: spans contributing at least half the
	// root span's duration materially explain where the time went.
	var criticalPath []string
	threshold := maxDuration / 2
	for _, sp := range spans {
		if uint64(sp.DurationMs)*uint64(time.Millisecond) >= threshold {
			criticalPath = append(criticalPath, sp.Name)
		}
	}

	return contextpack.TraceDetail{TraceID: traceID, Spans: spans, CriticalPath: criticalPath}, nil
}

func (s *Source) LogsForTraces(ctx context.Context, traceIDs []string) ([]telemetry.LogLine, error) {
	if len(traceIDs) == 0 {
		return nil, nil
	}
	rows, err := s.conn.Query(ctx, `
		SELECT Timestamp, TraceId, ServiceName, SeverityText, Body
		FROM otel_logs WHERE TraceId IN ? ORDER BY Timestamp`, traceIDs)
	if err != nil {
		return nil, fmt.Errorf("query logs for traces: %w", err)
	}
	defer rows.Close()
	return scanLogLines(rows)
}

// LogsByService is the fallback CLAUDE.md §6.2 requires when
// LogsForTraces returns empty (a service logging without trace_id).
func (s *Source) LogsByService(ctx context.Context, w telemetry.Window) ([]telemetry.LogLine, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT Timestamp, TraceId, ServiceName, SeverityText, Body
		FROM otel_logs
		WHERE Timestamp BETWEEN ? AND ? AND ServiceName = ?
		ORDER BY Timestamp`, w.From, w.To, w.Service)
	if err != nil {
		return nil, fmt.Errorf("query logs by service: %w", err)
	}
	defer rows.Close()
	return scanLogLines(rows)
}

func scanLogLines(rows driver.Rows) ([]telemetry.LogLine, error) {
	var out []telemetry.LogLine
	for rows.Next() {
		var l telemetry.LogLine
		if err := rows.Scan(&l.Timestamp, &l.TraceID, &l.Service, &l.Severity, &l.Body); err != nil {
			return nil, fmt.Errorf("scan log line: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// K8sEvents reads events written by cmd/eventwatcher, which land in
// otel_logs via the collector's filelog receiver (no ServiceName set
// for that pipeline, so events are identified by Body shape instead).
func (s *Source) K8sEvents(ctx context.Context, w telemetry.Window) ([]contextpack.K8sEvent, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT Timestamp,
		       JSONExtractString(Body, 'namespace'),
		       JSONExtractString(Body, 'involved_object_name'),
		       JSONExtractString(Body, 'reason'),
		       JSONExtractString(Body, 'message'),
		       JSONExtractInt(Body, 'count')
		FROM otel_logs
		WHERE Timestamp BETWEEN ? AND ?
		  AND Body LIKE '%involved_object_kind%'
		ORDER BY Timestamp`, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("query k8s events: %w", err)
	}
	defer rows.Close()

	var out []contextpack.K8sEvent
	for rows.Next() {
		var e contextpack.K8sEvent
		var count int64
		if err := rows.Scan(&e.Timestamp, &e.Namespace, &e.Pod, &e.Reason, &e.Message, &count); err != nil {
			return nil, fmt.Errorf("scan k8s event: %w", err)
		}
		e.Count = int32(count)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentChanges (deploy history) isn't implemented yet — nothing
// tracks Deployment revisions to a ClickHouse-queryable log. Returns
// empty rather than guessing at a schema prematurely.
func (s *Source) RecentChanges(ctx context.Context, w telemetry.Window) ([]telemetry.ChangeRecord, error) {
	return nil, nil
}
