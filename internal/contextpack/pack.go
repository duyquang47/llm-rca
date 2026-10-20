// Package contextpack defines the ContextPack schema shared between the
// context-builder (Phase 1) and the prompt serializer (Phase 2). See
// CLAUDE.md §6.4 for the canonical spec.
package contextpack

import "time"

type Window struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Service string    `json:"service"`
}

type AlertSummary struct {
	Name     string `json:"name"`
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Graph struct {
	Edges []GraphEdge `json:"edges"`
}

type MetricDelta struct {
	Name     string            `json:"name"`
	Labels   map[string]string `json:"labels"`
	Current  float64           `json:"current"`
	Baseline float64           `json:"baseline"`
	PctDiff  float64           `json:"pct_diff"`
}

type Span struct {
	Name       string `json:"name"`
	Service    string `json:"service"`
	DurationMs int64  `json:"duration_ms"`
	StatusCode string `json:"status_code"`
	// HTTPStatusCode is separate from StatusCode (OTel's abstract
	// Ok/Error/Unset) because otelhttp only marks a *server* span
	// Error for 5xx — a 4xx like 429 leaves StatusCode "Unset" by
	// design (OTel semconv treats 4xx as the caller's fault, not the
	// server's), which made a real rate-limit fault invisible in a
	// trace even though the actual status code was sitting in
	// SpanAttributes the whole time (case #11's evidence-recall miss).
	HTTPStatusCode int `json:"http_status_code,omitempty"`
}

type TraceDetail struct {
	TraceID      string   `json:"trace_id"`
	Spans        []Span   `json:"spans"`
	CriticalPath []string `json:"critical_path"`
}

type TemplateStat struct {
	TemplateID    int            `json:"template_id"`
	Template      string         `json:"template"`
	Count         int            `json:"count"`
	FirstSeen     time.Time      `json:"first_seen"`
	LastSeen      time.Time      `json:"last_seen"`
	ByService     map[string]int `json:"by_service"`
	BaselineCount int            `json:"baseline_count"`
	RateShape     string         `json:"rate_shape"`
	// Targets aggregates the "target" log attribute (which downstream
	// service a failed call was aimed at) separately from the
	// clustered template text. Drain's own similarity merging
	// wildcards short varying tokens like a target name — two log
	// lines differing only in "target=payment" vs
	// "target=thirdparty-mock" are ~75% similar, well above the 0.4
	// merge threshold — so folding the target into the template text
	// itself would just get it wildcarded away again. Keeping it as a
	// separate aggregation is what actually preserves it (case #11's
	// evidence-recall miss: the real root cause's name never survived
	// into the pack because of exactly this).
	Targets map[string]int `json:"targets,omitempty"`
}

type K8sEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int32     `json:"count"`
}

type K8sContext struct {
	Events []K8sEvent `json:"events"`
}

// ContextPack is the versioned boundary between Phase 1 (deterministic
// collection/compression) and Phase 2 (stochastic LLM inference).
// See CLAUDE.md §6.4.
type ContextPack struct {
	Version    string       `json:"version"`
	IncidentID string       `json:"incident_id"`
	Window     Window       `json:"window"`
	Alert      AlertSummary `json:"alert"`

	DependencyGraph Graph          `json:"dependency_graph"`
	MetricDiffs     []MetricDelta  `json:"metric_diffs"`
	Exemplars       []TraceDetail  `json:"exemplar_traces"`
	LogTemplates    []TemplateStat `json:"log_templates"`
	K8sContext      K8sContext     `json:"k8s_context"`

	TokenEstimate int      `json:"token_estimate"`
	Truncated     []string `json:"truncated"`

	// NotScanned lists pack sections that were never collected (manual
	// investigation flow, where an SRE can request just the "service"
	// or just the "k8s" scan group before analyzing) — as opposed to
	// collected but empty. omitempty keeps the auto path's pack (which
	// always scans everything) byte-identical to before this field
	// existed.
	NotScanned []string `json:"not_scanned,omitempty"`
}
