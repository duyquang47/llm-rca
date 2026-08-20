// Package incident implements incident-orchestrator's dedupe/grouping
// and state machine. See CLAUDE.md §6.1.
package incident

import (
	"fmt"
	"sync/atomic"
	"time"
)

type State string

const (
	StateReceived   State = "RECEIVED"
	StateCollecting State = "COLLECTING"
	StateAnalyzing  State = "ANALYZING"
	StateDone       State = "DONE"
	StateFailed     State = "FAILED"
)

type Alert struct {
	Name     string
	Service  string
	Severity string
	Message  string
	FiredAt  time.Time
}

type Incident struct {
	ID        string
	Services  []string // union of every alert's service in this incident
	Alerts    []Alert
	State     State
	StartedAt time.Time
	UpdatedAt time.Time
}

var idCounter int64

func newID() string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("inc-%d-%d", time.Now().Unix(), n)
}

func addServiceIfNew(services []string, service string) []string {
	for _, s := range services {
		if s == service {
			return services
		}
	}
	return append(services, service)
}
