package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

// CheckPack runs the CLAUDE.md §10 GĐ1 acceptance assertions against a
// built ContextPack — deliberately pack-only, no LLM call involved, so
// a failure here always points at Phase 1 (collection), never Phase 2
// (prompt/model). Returns one message per violation; empty means pass.
//
// must_rank_in_top3 is intentionally NOT checked here — ranking only
// exists once an LLM has produced ranked_causes, so cmd/evaluator
// checks it against the stored llm_output instead.
func (a ContextAssertions) CheckPack(pack contextpack.ContextPack) []string {
	var violations []string

	services := servicesIn(pack)

	for _, want := range a.MustIncludeService {
		if !services[want] {
			violations = append(violations, fmt.Sprintf("must_include_service: %q not found anywhere in the pack", want))
		}
	}

	if a.MustIncludeLogTemplateMatching != "" {
		re, err := regexp.Compile(a.MustIncludeLogTemplateMatching)
		if err != nil {
			violations = append(violations, fmt.Sprintf("must_include_log_template_matching: invalid regexp %q: %v", a.MustIncludeLogTemplateMatching, err))
		} else {
			found := false
			for _, t := range pack.LogTemplates {
				if re.MatchString(t.Template) {
					found = true
					break
				}
			}
			if !found {
				violations = append(violations, fmt.Sprintf("must_include_log_template_matching: no log template matches %q", a.MustIncludeLogTemplateMatching))
			}
		}
	}

	for _, want := range a.MustIncludeMetricDelta {
		re, err := regexp.Compile(want.NameMatches)
		if err != nil {
			violations = append(violations, fmt.Sprintf("must_include_metric_delta: invalid regexp %q: %v", want.NameMatches, err))
			continue
		}
		found := false
		for _, m := range pack.MetricDiffs {
			if re.MatchString(m.Name) && m.PctDiff >= want.MinPctDiff {
				found = true
				break
			}
		}
		if !found {
			violations = append(violations, fmt.Sprintf("must_include_metric_delta: no metric matching %q with pct_diff >= %.1f", want.NameMatches, want.MinPctDiff))
		}
	}

	if len(a.MustNotInclude) > 0 {
		raw, err := json.Marshal(pack)
		if err != nil {
			violations = append(violations, fmt.Sprintf("must_not_include: failed to serialize pack for scanning: %v", err))
		} else {
			text := strings.ToLower(string(raw))
			for _, forbidden := range a.MustNotInclude {
				if strings.Contains(text, strings.ToLower(forbidden)) {
					violations = append(violations, fmt.Sprintf("must_not_include: pack contains forbidden string %q — isolation leak", forbidden))
				}
			}
		}
	}

	if a.MaxTokens > 0 && pack.TokenEstimate > a.MaxTokens {
		violations = append(violations, fmt.Sprintf("max_tokens: pack estimate %d exceeds limit %d", pack.TokenEstimate, a.MaxTokens))
	}

	if a.MaxServicesIncluded > 0 && len(services) > a.MaxServicesIncluded {
		violations = append(violations, fmt.Sprintf("max_services_included: pack references %d services, limit is %d", len(services), a.MaxServicesIncluded))
	}

	return violations
}

// RankInTop3 is the LLM-output half of must_rank_in_top3 — see the
// CheckPack doc comment for why it lives separately.
func (a ContextAssertions) RankInTop3(rankedServices []string) bool {
	if a.MustRankInTop3 == "" {
		return true
	}
	for i, svc := range rankedServices {
		if i >= 3 {
			break
		}
		if svc == a.MustRankInTop3 {
			return true
		}
	}
	return false
}

func servicesIn(pack contextpack.ContextPack) map[string]bool {
	out := map[string]bool{}
	for _, e := range pack.DependencyGraph.Edges {
		out[e.From] = true
		out[e.To] = true
	}
	for _, ex := range pack.Exemplars {
		for _, s := range ex.Spans {
			out[s.Service] = true
		}
	}
	for _, m := range pack.MetricDiffs {
		if svc, ok := m.Labels["service"]; ok {
			out[svc] = true
		}
	}
	for _, t := range pack.LogTemplates {
		for svc := range t.ByService {
			out[svc] = true
		}
		for target := range t.Targets {
			out[target] = true
		}
	}
	return out
}
