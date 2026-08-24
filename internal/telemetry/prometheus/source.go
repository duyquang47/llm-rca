// Package prometheus implements the metrics half of telemetry.Source —
// specifically MetricDiffVsBaseline, comparing current values against
// the same time yesterday (CLAUDE.md §6.2).
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/telemetry"
)

type Source struct {
	baseURL string
	client  *http.Client
}

func New(baseURL string) *Source {
	return &Source{baseURL: baseURL, client: &http.Client{Timeout: 15 * time.Second}}
}

// p99LatencyByService is deliberately the one metric wired up for
// Phase 1's vertical slice — otelhttp's built-in http.server.request.
// duration histogram, present for every demo service with no extra
// instrumentation needed. Extend this list once more scenarios need
// error-rate or resource-usage diffs.
//
// Groups by exported_job, not job: every demo service is scraped
// through the single otel-collector Prometheus exporter target, so
// Prometheus's ServiceMonitor scrape renames the OTel resource's own
// `job` (the actual service name) to `exported_job` and overwrites
// `job` with the scrape target's own name ("otel-collector") — the
// default relabeling behavior when honor_labels isn't set. Grouping
// by `job` here would silently collapse every service into one series.
const p99LatencyByService = `histogram_quantile(0.99, sum(rate(http_server_request_duration_seconds_bucket[5m])) by (le, exported_job))`

// dbPoolWaitP99 is case #3's dedicated signal — see handler.dbPoolWait's
// doc comment for why a starved connection pool needs its own metric
// rather than relying on overall request latency, which looks identical
// to a genuinely slow downstream call.
const dbPoolWaitP99 = `histogram_quantile(0.99, sum(rate(db_pool_wait_seconds_bucket[5m])) by (le, exported_job))`

type metricQuery struct {
	name  string
	query string
}

var diffQueries = []metricQuery{
	{name: "http_server_request_duration_p99_seconds", query: p99LatencyByService},
	{name: "db_pool_wait_seconds_p99", query: dbPoolWaitP99},
}

func (s *Source) MetricDiffVsBaseline(ctx context.Context, w telemetry.Window) ([]contextpack.MetricDelta, error) {
	var out []contextpack.MetricDelta
	for _, mq := range diffQueries {
		current, err := s.instantQuery(ctx, mq.query, w.To)
		if err != nil {
			return nil, fmt.Errorf("query current %s: %w", mq.name, err)
		}
		baseline, err := s.instantQuery(ctx, mq.query, w.To.Add(-24*time.Hour))
		if err != nil {
			return nil, fmt.Errorf("query baseline %s: %w", mq.name, err)
		}

		baselineByJob := map[string]float64{}
		for _, r := range baseline {
			baselineByJob[r.job] = r.value
		}

		for _, r := range current {
			base := baselineByJob[r.job]
			pctDiff := 0.0
			if base > 0 {
				pctDiff = (r.value - base) / base * 100
			} else if r.value > 0 {
				pctDiff = 100
			}
			out = append(out, contextpack.MetricDelta{
				Name:     mq.name,
				Labels:   map[string]string{"service": r.job},
				Current:  r.value,
				Baseline: base,
				PctDiff:  pctDiff,
			})
		}
	}
	return out, nil
}

type sample struct {
	job   string
	value float64
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (s *Source) instantQuery(ctx context.Context, query string, at time.Time) ([]sample, error) {
	u := s.baseURL + "/api/v1/query?" + url.Values{
		"query": {query},
		"time":  {strconv.FormatInt(at.Unix(), 10)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal prometheus response: %w (raw: %s)", err, body)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed (raw: %s)", body)
	}

	var out []sample
	for _, r := range parsed.Data.Result {
		valStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		// histogram_quantile returns "NaN" for a series with no
		// observations in the window; math.IsNaN both because
		// ParseFloat happily accepts the literal "NaN" and because
		// json.Marshal errors on NaN float64s later in Build.
		if err != nil || math.IsNaN(val) {
			continue
		}
		out = append(out, sample{job: r.Metric["exported_job"], value: val})
	}
	return out, nil
}
