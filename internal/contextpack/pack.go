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
}
