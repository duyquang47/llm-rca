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

type GroundTruth struct {
	Service     string   `yaml:"service"`
	Cause       string   `yaml:"cause"`
	Distractors []string `yaml:"distractors"`
}

type MetricDeltaAssertion struct {
	NameMatches string  `yaml:"name_matches"`
	MinPctDiff  float64 `yaml:"min_pct_diff"`
}

// ContextAssertions are CLAUDE.md §10 GĐ1's declarative acceptance
// checks for evidence recall — run against the ContextPack, not the
// LLM output, so a failure points at Phase 1 (collection), never
// Phase 2 (prompt/model).
type ContextAssertions struct {
	MustIncludeService             []string               `yaml:"must_include_service"`
	MustRankInTop3                 string                 `yaml:"must_rank_in_top3"`
	MustIncludeLogTemplateMatching string                 `yaml:"must_include_log_template_matching"`
	MustIncludeMetricDelta         []MetricDeltaAssertion `yaml:"must_include_metric_delta"`
	MustNotInclude                 []string               `yaml:"must_not_include"`
	MaxTokens                      int                    `yaml:"max_tokens"`
	MaxServicesIncluded            int                    `yaml:"max_services_included"`
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
