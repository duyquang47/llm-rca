// Package prometheus implements the metrics half of telemetry.Source —
// specifically MetricDiffVsBaseline, comparing current values against
// the same time yesterday (CLAUDE.md §6.2).
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
const p99LatencyByService = `histogram_quantile(0.99, sum(rate(http_server_request_duration_seconds_bucket[5m])) by (le, job))`

func (s *Source) MetricDiffVsBaseline(ctx context.Context, w telemetry.Window) ([]contextpack.MetricDelta, error) {
	current, err := s.instantQuery(ctx, p99LatencyByService, w.To)
	if err != nil {
		return nil, fmt.Errorf("query current p99: %w", err)
	}
	baseline, err := s.instantQuery(ctx, p99LatencyByService, w.To.Add(-24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("query baseline p99: %w", err)
	}

	baselineByJob := map[string]float64{}
	for _, r := range baseline {
		baselineByJob[r.job] = r.value
	}

	var out []contextpack.MetricDelta
	for _, r := range current {
		base := baselineByJob[r.job]
		pctDiff := 0.0
		if base > 0 {
			pctDiff = (r.value - base) / base * 100
		} else if r.value > 0 {
			pctDiff = 100
		}
		out = append(out, contextpack.MetricDelta{
			Name:     "http_server_request_duration_p99_seconds",
			Labels:   map[string]string{"service": r.job},
			Current:  r.value,
			Baseline: base,
			PctDiff:  pctDiff,
		})
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
		if err != nil {
			continue
		}
		out = append(out, sample{job: r.Metric["job"], value: val})
	}
	return out, nil
}
