package incident

import (
	"sync"
	"time"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

const (
	defaultWindow  = 5 * time.Minute
	defaultMaxHops = 2
)

// Grouper implements CLAUDE.md §6.1's dedupe/grouping, amended per the
// accepted case #13 fix: a literal `(service, time_window)` key splits
// two related alerts on different-but-connected services (e.g. a
// latency alert on gateway and a 429 alert on payment, both within a
// few hops via order) into two incidents instead of one with two
// candidate causes to rank — defeating case #13's purpose as a
// multi-cause-ranking test. Grouping is dependency-graph-aware instead:
// two alerts merge if their services are within maxHops of each other
// AND within the time window.
type Grouper struct {
	mu        sync.Mutex
	window    time.Duration
	maxHops   int
	graph     contextpack.Graph
	incidents []*Incident
}

func NewGrouper() *Grouper {
	return &Grouper{window: defaultWindow, maxHops: defaultMaxHops}
}

// SetGraph refreshes the dependency graph used for connectivity
// checks. The caller (cmd/orchestrator) is expected to call this
// periodically from telemetry.Source.DependencyGraph — the graph is
// not static, and grouping decisions get stale silently otherwise.
func (g *Grouper) SetGraph(graph contextpack.Graph) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.graph = graph
}

// Ingest assigns alert to an existing incident (dependency-connected,
// within the window) or starts a new one. Returns the incident and
// whether it was newly created.
func (g *Grouper) Ingest(alert Alert) (*Incident, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expire(alert.FiredAt)

	for _, inc := range g.incidents {
		if g.connected(inc, alert) {
			inc.Alerts = append(inc.Alerts, alert)
			inc.Services = addServiceIfNew(inc.Services, alert.Service)
			inc.UpdatedAt = alert.FiredAt
			return inc, false
		}
	}

	inc := &Incident{
		ID:        newID(),
		Services:  []string{alert.Service},
		Alerts:    []Alert{alert},
		State:     StateReceived,
		StartedAt: alert.FiredAt,
		UpdatedAt: alert.FiredAt,
	}
	g.incidents = append(g.incidents, inc)
	return inc, true
}

func (g *Grouper) connected(inc *Incident, alert Alert) bool {
	if alert.FiredAt.Sub(inc.UpdatedAt) > g.window {
		return false
	}
	for _, svc := range inc.Services {
		if withinHops(g.graph, svc, alert.Service, g.maxHops) {
			return true
		}
	}
	return false
}

// expire drops incidents whose window has closed relative to "now" so
// the incident list doesn't grow without bound and old incidents
// don't keep absorbing unrelated new alerts on a since-reused service.
func (g *Grouper) expire(now time.Time) {
	kept := g.incidents[:0]
	for _, inc := range g.incidents {
		if now.Sub(inc.UpdatedAt) <= g.window {
			kept = append(kept, inc)
		}
	}
	g.incidents = kept
}
