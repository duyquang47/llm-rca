// Package scenario parses the YAML fault-scenario files fault-controller
// executes and evaluator scores against. See CLAUDE.md §7.
package scenario

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Step struct {
	Target   string         `yaml:"target,omitempty"`
	Action   string         `yaml:"action"`
	Params   map[string]any `yaml:"params,omitempty"`
	Duration string         `yaml:"duration,omitempty"`
}

// Cause is one valid root cause: a (service, cause) pair.
type Cause struct {
	Service string `yaml:"service"`
	Cause   string `yaml:"cause"`
}

// GroundTruth's Service/Cause fields cover the common single-cause
// case (12 of the 13 CLAUDE.md §7 scenarios). Causes exists for case
// #13 specifically — two independent faults injected concurrently,
// where the whole point of the scenario is testing whether the model
// ranks *both* real causes instead of picking one and ignoring the
// other. Kept as a separate field rather than replacing Service/Cause
// so the 12 single-cause scenario YAMLs don't need touching.
type GroundTruth struct {
	Service     string   `yaml:"service"`
	Cause       string   `yaml:"cause"`
	Causes      []Cause  `yaml:"causes,omitempty"`
	Distractors []string `yaml:"distractors"`
}

// AllCauses normalizes the single-cause and multi-cause shapes into
// one list, so callers (evaluator) don't need to branch on which
// field was populated.
func (g GroundTruth) AllCauses() []Cause {
	if len(g.Causes) > 0 {
		return g.Causes
	}
	if g.Service == "" {
		return nil
	}
	return []Cause{{Service: g.Service, Cause: g.Cause}}
}

type MetricDeltaAssertion struct {
	NameMatches string  `yaml:"name_matches"`
	MinPctDiff  float64 `yaml:"min_pct_diff"`
}

// SpanRepeatedAssertion checks for a span name repeated MinCount+ times
// within a single exemplar trace — the distinguishing signal for case
// #4 (N+1 query): the count of "pg.query" spans in one trace, not any
// log line or metric, is what tells N+1 apart from a single slow query.
type SpanRepeatedAssertion struct {
	Name     string `yaml:"name"`
	MinCount int    `yaml:"min_count"`
}

// ContextAssertions are CLAUDE.md §10 GĐ1's declarative acceptance
// checks for evidence recall — run against the ContextPack, not the
// LLM output, so a failure points at Phase 1 (collection), never
// Phase 2 (prompt/model).
type ContextAssertions struct {
	MustIncludeService             []string                `yaml:"must_include_service"`
	MustRankInTop3                 string                  `yaml:"must_rank_in_top3"`
	MustIncludeLogTemplateMatching string                  `yaml:"must_include_log_template_matching"`
	MustIncludeMetricDelta         []MetricDeltaAssertion  `yaml:"must_include_metric_delta"`
	MustIncludeSpanRepeated        []SpanRepeatedAssertion `yaml:"must_include_span_repeated"`
	MustIncludeK8sEventMatching    string                  `yaml:"must_include_k8s_event_matching"`
	MustNotInclude                 []string                `yaml:"must_not_include"`
	MaxTokens                      int                     `yaml:"max_tokens"`
	MaxServicesIncluded            int                     `yaml:"max_services_included"`
}

type Scenario struct {
	Name              string            `yaml:"name"`
	Steps             []Step            `yaml:"steps"`
	GroundTruth       GroundTruth       `yaml:"ground_truth"`
	ContextAssertions ContextAssertions `yaml:"context_assertions"`
}

func Load(path string) (Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("read scenario %s: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return Scenario{}, fmt.Errorf("parse scenario %s: %w", path, err)
	}
	if s.Name == "" {
		return Scenario{}, fmt.Errorf("scenario %s: name is required", path)
	}
	return s, nil
}
