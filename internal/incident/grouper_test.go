package incident

import (
	"testing"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

// demoGraph mirrors the actual demo namespace topology:
// gateway -> order -> {inventory, payment}; payment -> thirdparty-mock.
func demoGraph() contextpack.Graph {
	return contextpack.Graph{Edges: []contextpack.GraphEdge{
		{From: "gateway", To: "order"},
		{From: "order", To: "inventory"},
		{From: "order", To: "payment"},
		{From: "payment", To: "thirdparty-mock"},
	}}
}

func TestIngest_SameServiceAlertsMergeIntoOneIncident(t *testing.T) {
	g := NewGrouper()
	g.SetGraph(demoGraph())
	now := time.Now()

	_, isNew1 := g.Ingest(Alert{Service: "inventory", FiredAt: now})
	_, isNew2 := g.Ingest(Alert{Service: "inventory", FiredAt: now.Add(30 * time.Second)})

	if !isNew1 {
		t.Error("first alert should start a new incident")
	}
	if isNew2 {
		t.Error("second alert on the same service should merge into the first incident")
	}
}

// TestIngest_Case13_ConnectedServicesMergeIntoOneIncident is the fix
// for the accepted A4 finding: case #13 combines fault #1 (latency at
// inventory, alerting at gateway) and fault #11 (429 from
// thirdparty-mock, alerting at payment) within the same window. A
// literal (service, window) key would split these into two incidents,
// defeating case #13's purpose as a multi-cause-ranking test — gateway
// and payment are 2 hops apart via order, so they must merge.
func TestIngest_Case13_ConnectedServicesMergeIntoOneIncident(t *testing.T) {
	g := NewGrouper()
	g.SetGraph(demoGraph())
	now := time.Now()

	inc1, isNew1 := g.Ingest(Alert{Service: "gateway", FiredAt: now})
	inc2, isNew2 := g.Ingest(Alert{Service: "payment", FiredAt: now.Add(10 * time.Second)})

	if !isNew1 {
		t.Fatal("first alert should start a new incident")
	}
	if isNew2 {
		t.Error("want gateway+payment alerts to merge into one incident (connected within 2 hops via order), got a separate incident")
	}
	if inc1.ID != inc2.ID {
		t.Errorf("want same incident id, got %q vs %q", inc1.ID, inc2.ID)
	}
	if len(inc1.Services) != 2 {
		t.Errorf("want both services recorded on the merged incident, got %v", inc1.Services)
	}
	if len(inc1.Alerts) != 2 {
		t.Errorf("want both alerts recorded, got %d", len(inc1.Alerts))
	}
}

func TestIngest_UnrelatedServicesStaySeparate(t *testing.T) {
	// A graph where "unrelated-svc" has no edges at all to the demo
	// chain — simulates two genuinely independent incidents, not a
	// missed correlation.
	graph := demoGraph()
	g := NewGrouper()
	g.SetGraph(graph)
	now := time.Now()

	inc1, _ := g.Ingest(Alert{Service: "gateway", FiredAt: now})
	inc2, isNew2 := g.Ingest(Alert{Service: "unrelated-svc", FiredAt: now.Add(5 * time.Second)})

	if !isNew2 {
		t.Error("want a disconnected service to start its own incident")
	}
	if inc1.ID == inc2.ID {
		t.Error("want unrelated services to stay in separate incidents")
	}
}

func TestIngest_OutsideWindowStartsNewIncident(t *testing.T) {
	g := NewGrouper()
	g.SetGraph(demoGraph())
	now := time.Now()

	inc1, _ := g.Ingest(Alert{Service: "inventory", FiredAt: now})
	inc2, isNew2 := g.Ingest(Alert{Service: "inventory", FiredAt: now.Add(10 * time.Minute)})

	if !isNew2 {
		t.Error("want an alert 10 minutes later (outside the 5-minute window) to start a new incident")
	}
	if inc1.ID == inc2.ID {
		t.Error("want the stale incident not reused")
	}
}

func TestIngest_HopLimitPreventsOverMerging(t *testing.T) {
	// inventory and thirdparty-mock are 3 hops apart
	// (inventory <- order -> payment -> thirdparty-mock); with
	// maxHops=2 they must NOT merge, or every alert in the cluster
	// would eventually collapse into one incident regardless of
	// actual proximity.
	g := NewGrouper()
	g.SetGraph(demoGraph())
	now := time.Now()

	inc1, _ := g.Ingest(Alert{Service: "inventory", FiredAt: now})
	inc2, isNew2 := g.Ingest(Alert{Service: "thirdparty-mock", FiredAt: now.Add(time.Second)})

	if !isNew2 {
		t.Error("want services beyond maxHops to stay separate")
	}
	if inc1.ID == inc2.ID {
		t.Error("want inventory and thirdparty-mock (3 hops apart) in separate incidents")
	}
}
