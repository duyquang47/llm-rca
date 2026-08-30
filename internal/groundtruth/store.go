// Package groundtruth stores fault-controller's ground truth records,
// deliberately separate from the shared ClickHouse instance
// context-builder/orchestrator read from. CLAUDE.md §2.4: "NetworkPolicy
// chặn context-builder truy cập ground truth store" only makes sense
// against a genuinely separate store — reusing ClickHouse for both
// would make that isolation claim meaningless. A local JSON file per
// scenario run, readable only within the fault-manager namespace's own
// filesystem/pod, is the simplest thing that's actually isolated.
package groundtruth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Cause is one valid root cause: a (service, cause) pair. Mirrors
// internal/scenario.Cause — kept as a separate type so this package
// doesn't need to import scenario just for one struct shape.
type Cause struct {
	Service string `json:"service"`
	Cause   string `json:"cause"`
}

type Record struct {
	ScenarioName string    `json:"scenario_name"`
	Service      string    `json:"service"`
	Cause        string    `json:"cause"`
	Causes       []Cause   `json:"causes,omitempty"`
	Distractors  []string  `json:"distractors"`
	StartedAt    time.Time `json:"started_at"`
}

// AllCauses mirrors scenario.GroundTruth.AllCauses — normalizes the
// single-cause and multi-cause (case #13) shapes into one list.
func (r Record) AllCauses() []Cause {
	if len(r.Causes) > 0 {
		return r.Causes
	}
	if r.Service == "" {
		return nil
	}
	return []Cause{{Service: r.Service, Cause: r.Cause}}
}

type Store struct {
	dir string
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Save(r Record) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create ground truth dir: %w", err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ground truth: %w", err)
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%s-%d.json", r.ScenarioName, r.StartedAt.Unix()))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write ground truth: %w", err)
	}
	return nil
}

// Latest returns the most recently started ground truth record for a
// scenario name — what evaluator needs to score the run that just
// happened.
func (s *Store) Latest(scenarioName string) (Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return Record{}, fmt.Errorf("read ground truth dir: %w", err)
	}

	var latest Record
	found := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if r.ScenarioName != scenarioName {
			continue
		}
		if !found || r.StartedAt.After(latest.StartedAt) {
			latest = r
			found = true
		}
	}
	if !found {
		return Record{}, fmt.Errorf("no ground truth found for scenario %q", scenarioName)
	}
	return latest, nil
}
