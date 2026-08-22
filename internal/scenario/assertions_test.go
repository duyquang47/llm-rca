package scenario

import (
	"strings"
	"testing"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

func TestCheckPack_CatchesForbiddenStringLeak(t *testing.T) {
	// Shape of the real leak found running scenario #1 for real: a
	// k8s Event naming the fault-controller's own pod ends up in
	// K8sContext.
	pack := contextpack.ContextPack{
		K8sContext: contextpack.K8sContext{
			Events: []contextpack.K8sEvent{
				{Namespace: "chaos", Pod: "faultctl-timeout-cascade-nhmv7", Reason: "Scheduled"},
			},
		},
	}
	a := ContextAssertions{MustNotInclude: []string{"fault", "inject", "chaos", "_fault"}}

	violations := a.CheckPack(pack)
	if len(violations) == 0 {
		t.Fatal("want a violation for the leaked chaos-namespace event, got none")
	}
	// "fault" matches first since "faultctl" contains it as a
	// substring — any of the forbidden keywords firing is the point,
	// not which one specifically.
	joined := strings.Join(violations, "; ")
	if !strings.Contains(joined, "isolation leak") {
		t.Errorf("want a violation flagging the isolation leak, got %v", violations)
	}
}

func TestCheckPack_CleanPackPasses(t *testing.T) {
	pack := contextpack.ContextPack{
		TokenEstimate: 500,
		DependencyGraph: contextpack.Graph{Edges: []contextpack.GraphEdge{
			{From: "gateway", To: "order"},
			{From: "order", To: "inventory"},
		}},
		MetricDiffs: []contextpack.MetricDelta{
			{Name: "http_server_request_duration_p99_seconds", Labels: map[string]string{"service": "inventory"}, PctDiff: 120},
		},
		LogTemplates: []contextpack.TemplateStat{
			{Template: "downstream call failed", ByService: map[string]int{"order": 3}},
		},
	}
	a := ContextAssertions{
		MustIncludeService:             []string{"inventory", "order", "gateway"},
		MustIncludeLogTemplateMatching: "(?i)downstream call failed|timeout",
		MustIncludeMetricDelta:         []MetricDeltaAssertion{{NameMatches: "http_server_request_duration", MinPctDiff: 50}},
		MustNotInclude:                 []string{"fault", "inject", "chaos", "_fault"},
		MaxTokens:                      8000,
		MaxServicesIncluded:            6,
	}

	violations := a.CheckPack(pack)
	if len(violations) != 0 {
		t.Fatalf("want a clean pack to pass, got violations: %v", violations)
	}
}

func TestCheckPack_MissingServiceFails(t *testing.T) {
	pack := contextpack.ContextPack{
		DependencyGraph: contextpack.Graph{Edges: []contextpack.GraphEdge{{From: "gateway", To: "order"}}},
	}
	a := ContextAssertions{MustIncludeService: []string{"inventory"}}

	violations := a.CheckPack(pack)
	if len(violations) != 1 {
		t.Fatalf("want 1 violation for the missing service, got %d: %v", len(violations), violations)
	}
}

func TestCheckPack_MaxTokensExceeded(t *testing.T) {
	pack := contextpack.ContextPack{TokenEstimate: 9000}
	a := ContextAssertions{MaxTokens: 8000}

	violations := a.CheckPack(pack)
	if len(violations) != 1 {
		t.Fatalf("want 1 violation for exceeding max_tokens, got %d: %v", len(violations), violations)
	}
}

func TestRankInTop3(t *testing.T) {
	a := ContextAssertions{MustRankInTop3: "inventory"}

	if !a.RankInTop3([]string{"inventory", "order"}) {
		t.Error("want rank-1 match to satisfy top-3")
	}
	if !a.RankInTop3([]string{"order", "gateway", "inventory"}) {
		t.Error("want rank-3 match to satisfy top-3")
	}
	if a.RankInTop3([]string{"order", "gateway", "payment", "inventory"}) {
		t.Error("want rank-4 match to NOT satisfy top-3")
	}
	if a.RankInTop3(nil) {
		t.Error("want empty ranking to NOT satisfy top-3")
	}
}
