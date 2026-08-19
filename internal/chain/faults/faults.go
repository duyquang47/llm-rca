// Package faults implements the /_fault control endpoint and in-memory
// fault injection state described in CLAUDE.md §4.4.
//
// Absolute constraint (CLAUDE.md §2.4): this package must never log,
// create a span, or expose a metric about fault state. A request with
// injected latency must be indistinguishable from a genuinely slow
// request. Do not import log/slog or go.opentelemetry.io/otel here.
package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type LatencyFault struct {
	Probability float64 `json:"probability"`
	Ms          int     `json:"ms"`
}

type ErrorFault struct {
	Probability float64 `json:"probability"`
	Code        int     `json:"code"`
}

// Spec is the payload accepted by POST /_fault. PoolSize is a pointer so
// "absent" (no override) is distinguishable from "explicitly set to 0".
type Spec struct {
	Route    string        `json:"route"`
	Latency  *LatencyFault `json:"latency,omitempty"`
	Error    *ErrorFault   `json:"error,omitempty"`
	CPUBurn  bool          `json:"cpu_burn"`
	MemLeak  bool          `json:"mem_leak"`
	PoolSize *int          `json:"pool_size,omitempty"`
}

// Controller holds fault state in memory, reset on pod restart per §4.4.
type Controller struct {
	mu    sync.RWMutex
	specs map[string]Spec // keyed by route
	leaks [][]byte        // accumulates under mem_leak, never freed
}

func NewController() *Controller {
	return &Controller{specs: make(map[string]Spec)}
}

// Set installs (or replaces) the fault spec for a route.
func (c *Controller) Set(spec Spec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.specs[spec.Route] = spec
}

// Clear removes any fault spec for a route.
func (c *Controller) Clear(route string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.specs, route)
}

func (c *Controller) get(route string) (Spec, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.specs[route]
	return s, ok
}

// EffectivePoolSize resolves the precedence rule added to CLAUDE.md §4.4:
// a live /_fault pool_size override always wins over the static ConfigMap
// value while the fault is active; falls back to configured otherwise.
func (c *Controller) EffectivePoolSize(route string, configured int) int {
	spec, ok := c.get(route)
	if !ok || spec.PoolSize == nil {
		return configured
	}
	return *spec.PoolSize
}

// Apply probabilistically injects latency and/or an error for route,
// blocking the caller's goroutine for the injected latency. Returns a
// non-nil error when the fault dictates the request should fail.
func (c *Controller) Apply(ctx context.Context, route string) error {
	spec, ok := c.get(route)
	if !ok {
		return nil
	}

	if spec.MemLeak {
		c.mu.Lock()
		c.leaks = append(c.leaks, make([]byte, 1<<20)) // leak 1MiB per call
		c.mu.Unlock()
	}

	if spec.CPUBurn {
		burnCPU(200 * time.Millisecond)
	}

	if spec.Latency != nil && rand.Float64() < spec.Latency.Probability {
		select {
		case <-time.After(time.Duration(spec.Latency.Ms) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if spec.Error != nil && rand.Float64() < spec.Error.Probability {
		return fmt.Errorf("injected fault: status %d", spec.Error.Code)
	}

	return nil
}

func burnCPU(d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
	}
}

// Handler serves POST /_fault (set/replace a spec) and DELETE /_fault?route=
// (clear). Bind this only on an internal listener — never through the
// public k8s Service (CLAUDE.md §4.4).
func (c *Controller) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var spec Spec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				http.Error(w, "invalid fault spec: "+err.Error(), http.StatusBadRequest)
				return
			}
			if spec.Route == "" {
				http.Error(w, "route is required", http.StatusBadRequest)
				return
			}
			c.Set(spec)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			route := r.URL.Query().Get("route")
			if route == "" {
				http.Error(w, "route query param is required", http.StatusBadRequest)
				return
			}
			c.Clear(route)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
