package incident

import (
	"context"
	"fmt"

	ch "github.com/ClickHouse/clickhouse-go/v2"
)

// Store persists analyzed incidents to ClickHouse per CLAUDE.md §6.7.
// human_verdict/human_note exist from day one even though the POC's
// evaluator fills ground_truth automatically — production has no
// automatic ground truth, only SRE feedback, and adding columns to a
// running table is far more annoying than reserving them now.
type Store struct {
	conn ch.Conn
}

func NewStore(conn ch.Conn) *Store { return &Store{conn: conn} }

func (s *Store) EnsureSchema(ctx context.Context) error {
	return s.conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS incidents (
			incident_id      String,
			scenario_name    String,
			started_at       DateTime64(3),
			context_pack     String,
			pack_version     String,
			prompt_version   String,
			llm_model        String,
			llm_output       String,
			ground_truth     String,
			token_count      UInt32,
			latency_ms       UInt32,
			score_top1       Float32,
			score_mrr        Float32,
			human_verdict    Enum8('' = 0, 'correct' = 1, 'partial' = 2, 'wrong' = 3),
			human_note       String,
			mode             Enum8('auto' = 0, 'manual' = 1)
		) ENGINE = MergeTree ORDER BY (started_at, incident_id)`)
}

type Record struct {
	IncidentID    string
	ScenarioName  string
	StartedAtUnix int64
	ContextPack   string
	PackVersion   string
	PromptVersion string
	LLMModel      string
	LLMOutput     string
	TokenCount    uint32
	LatencyMs     uint32
	// Mode is "auto" (the alert-driven pipeline in cmd/orchestrator's
	// analyze()) or "manual" (an SRE-driven rcactl scan/analyze
	// session) — evaluator.findIncident matches purely by time window,
	// so this disambiguates a manual investigation that happens to
	// overlap a benchmark scenario's window. Must be set explicitly by
	// every caller; there is no implicit default.
	Mode string
}

func (s *Store) Insert(ctx context.Context, r Record) error {
	err := s.conn.Exec(ctx, `
		INSERT INTO incidents
			(incident_id, scenario_name, started_at, context_pack, pack_version,
			 prompt_version, llm_model, llm_output, ground_truth, token_count,
			 latency_ms, score_top1, score_mrr, human_verdict, human_note, mode)
		VALUES (?, ?, fromUnixTimestamp64Milli(?), ?, ?, ?, ?, ?, '', ?, ?, 0, 0, '', '', ?)`,
		r.IncidentID, r.ScenarioName, r.StartedAtUnix*1000, r.ContextPack, r.PackVersion,
		r.PromptVersion, r.LLMModel, r.LLMOutput, r.TokenCount, r.LatencyMs, r.Mode)
	if err != nil {
		return fmt.Errorf("insert incident record: %w", err)
	}
	return nil
}
