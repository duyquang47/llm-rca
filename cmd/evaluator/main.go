// Command evaluator scores a scenario run: reads ground truth from
// internal/groundtruth, finds the matching incident in ClickHouse (by
// time proximity — incidents don't carry the scenario name, only the
// alerting service), and computes top-1 accuracy / MRR / distractor
// confusion per CLAUDE.md §7.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/groundtruth"
	"github.com/duyquang47/llm-rca/internal/llm"
	"github.com/duyquang47/llm-rca/internal/scenario"
)

type incidentRow struct {
	IncidentID  string
	StartedAt   time.Time
	LLMOutput   string
	LLMModel    string
	ContextPack string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	scenarioName := flag.String("scenario", "", "scenario name to score (required, matches ground truth's scenario_name)")
	scenarioFile := flag.String("scenario-file", "", "path to the scenario YAML (for context_assertions; optional, skips pack checks if omitted)")
	groundTruthDir := flag.String("ground-truth-dir", envOr("GROUND_TRUTH_DIR", "/var/lib/faultctl/ground-truth"), "where faultctl wrote ground truth")
	chAddr := flag.String("clickhouse-addr", envOr("CLICKHOUSE_ADDR", "clickhouse.observability:9000"), "")
	chUser := flag.String("clickhouse-user", envOr("CLICKHOUSE_USER", "otel"), "")
	chPassword := flag.String("clickhouse-password", envOr("CLICKHOUSE_PASSWORD", "otel"), "")
	chDatabase := flag.String("clickhouse-database", envOr("CLICKHOUSE_DATABASE", "otel"), "")
	searchWindow := flag.Duration("search-window", 10*time.Minute, "how far after ground truth's started_at to look for a matching incident")
	flag.Parse()

	if *scenarioName == "" {
		return fmt.Errorf("-scenario is required")
	}

	gtStore := groundtruth.NewStore(*groundTruthDir)
	gt, err := gtStore.Latest(*scenarioName)
	if err != nil {
		return fmt.Errorf("load ground truth: %w", err)
	}

	conn, err := ch.Open(&ch.Options{
		Addr: []string{*chAddr},
		Auth: ch.Auth{Database: *chDatabase, Username: *chUser, Password: *chPassword},
	})
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}

	ctx := context.Background()
	incident, err := findIncident(ctx, conn, gt.StartedAt, *searchWindow)
	if err != nil {
		return err
	}

	var result llm.RCAResult
	if err := json.Unmarshal([]byte(incident.LLMOutput), &result); err != nil {
		return fmt.Errorf("parse llm_output for incident %s: %w", incident.IncidentID, err)
	}

	causes := gt.AllCauses()
	isMultiCause := len(causes) > 1

	var top1, mrr float64
	var distractorHit bool
	var recall float64
	var perCauseRank map[string]int
	if isMultiCause {
		recall, perCauseRank = scoreMultiCause(causes, result)
		// Reuse the two existing score columns rather than add a
		// schema column for a shape only one of the 13 scenarios has:
		// recall stands in for top1, average per-cause MRR for mrr.
		top1 = recall
		var mrrSum float64
		for _, rank := range perCauseRank {
			if rank > 0 {
				mrrSum += 1 / float64(rank)
			}
		}
		if len(causes) > 0 {
			mrr = mrrSum / float64(len(causes))
		}
	} else {
		top1, mrr, distractorHit = score(gt, result)
	}

	if *scenarioFile != "" {
		sc, err := scenario.Load(*scenarioFile)
		if err != nil {
			return fmt.Errorf("load scenario file for assertions: %w", err)
		}

		var pack contextpack.ContextPack
		if err := json.Unmarshal([]byte(incident.ContextPack), &pack); err != nil {
			return fmt.Errorf("parse stored context_pack: %w", err)
		}

		violations := sc.ContextAssertions.CheckPack(pack)
		var rankedServices []string
		for _, c := range result.RankedCauses {
			rankedServices = append(rankedServices, c.Service)
		}
		if !sc.ContextAssertions.RankInTop3(rankedServices) {
			violations = append(violations, fmt.Sprintf("must_rank_in_top3: %q not in the top 3 ranked causes %v", sc.ContextAssertions.MustRankInTop3, rankedServices))
		}

		fmt.Println("context_assertions:")
		if len(violations) == 0 {
			fmt.Println("  PASS (no violations)")
		} else {
			for _, v := range violations {
				fmt.Printf("  FAIL: %s\n", v)
			}
		}
	}

	fmt.Printf("scenario:        %s\n", gt.ScenarioName)
	if isMultiCause {
		fmt.Printf("ground_truth:    %d causes: %v distractors=%v\n", len(causes), causes, gt.Distractors)
	} else {
		fmt.Printf("ground_truth:    service=%s cause=%s distractors=%v\n", gt.Service, gt.Cause, gt.Distractors)
	}
	fmt.Printf("incident_id:     %s (llm_model=%s)\n", incident.IncidentID, incident.LLMModel)
	fmt.Printf("ranked_causes:   ")
	for _, c := range result.RankedCauses {
		fmt.Printf("[%d]%s(%.2f) ", c.Rank, c.Service, c.Confidence)
	}
	fmt.Println()
	if isMultiCause {
		fmt.Printf("recall:          %.3f (fraction of the %d true causes found anywhere in the ranking)\n", recall, len(causes))
		for svc, rank := range perCauseRank {
			if rank > 0 {
				fmt.Printf("  %s: rank %d\n", svc, rank)
			} else {
				fmt.Printf("  %s: not ranked at all\n", svc)
			}
		}
		fmt.Printf("avg_mrr:         %.3f\n", mrr)
	} else {
		fmt.Printf("top1:            %.0f\n", top1)
		fmt.Printf("mrr:             %.3f\n", mrr)
		fmt.Printf("distractor_hit:  %v (top-ranked service matched a known distractor, not ground truth)\n", distractorHit)
	}

	if err := updateScores(ctx, conn, incident.IncidentID, top1, mrr); err != nil {
		return fmt.Errorf("write scores back to incidents table: %w", err)
	}
	fmt.Println("scores written back to otel.incidents")

	return nil
}

// findIncident picks the incident whose started_at is the closest
// match at-or-after ground truth's started_at, within window —
// incidents don't record which scenario produced them (only the
// alerting service), so time proximity is what correlates the two.
func findIncident(ctx context.Context, conn ch.Conn, gtStartedAt time.Time, window time.Duration) (incidentRow, error) {
	rows, err := conn.Query(ctx, `
		SELECT incident_id, started_at, llm_output, llm_model, context_pack
		FROM incidents
		WHERE started_at >= ? AND started_at <= ?
		ORDER BY started_at ASC
		LIMIT 1`, gtStartedAt.Add(-30*time.Second), gtStartedAt.Add(window))
	if err != nil {
		return incidentRow{}, fmt.Errorf("query incidents: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return incidentRow{}, fmt.Errorf("no incident found within %s after ground truth started_at=%s", window, gtStartedAt)
	}
	var r incidentRow
	if err := rows.Scan(&r.IncidentID, &r.StartedAt, &r.LLMOutput, &r.LLMModel, &r.ContextPack); err != nil {
		return incidentRow{}, fmt.Errorf("scan incident: %w", err)
	}
	return r, rows.Err()
}

// score computes top-1 accuracy (1.0 if the rank-1 cause's service
// matches ground truth), MRR (1/rank of the first matching service,
// 0 if never ranked), and whether the model confidently picked a
// known distractor instead — CLAUDE.md §7's three metrics.
func score(gt groundtruth.Record, result llm.RCAResult) (top1, mrr float64, distractorHit bool) {
	if len(result.RankedCauses) == 0 {
		return 0, 0, false
	}

	if result.RankedCauses[0].Service == gt.Service {
		top1 = 1
	}
	for _, d := range gt.Distractors {
		if result.RankedCauses[0].Service == d {
			distractorHit = true
		}
	}

	for _, c := range result.RankedCauses {
		if c.Service == gt.Service {
			mrr = 1 / float64(c.Rank)
			break
		}
	}

	return top1, mrr, distractorHit
}

// scoreMultiCause handles case #13's shape: more than one valid root
// cause injected concurrently. A single top1/mrr number would hide
// the actual question CLAUDE.md §7 poses for this case — "hệ thống
// có xếp hạng được nguyên nhân, hay chỉ liệt kê mọi bất thường?" — so
// this reports recall (fraction of true causes found anywhere in the
// ranking) and each cause's individual rank instead.
func scoreMultiCause(causes []groundtruth.Cause, result llm.RCAResult) (recall float64, perCauseRank map[string]int) {
	perCauseRank = map[string]int{}
	found := 0
	for _, c := range causes {
		rank := 0
		for _, rc := range result.RankedCauses {
			if rc.Service == c.Service {
				rank = rc.Rank
				break
			}
		}
		perCauseRank[c.Service] = rank
		if rank > 0 {
			found++
		}
	}
	if len(causes) > 0 {
		recall = float64(found) / float64(len(causes))
	}
	return recall, perCauseRank
}

func updateScores(ctx context.Context, conn ch.Conn, incidentID string, top1, mrr float64) error {
	return conn.Exec(ctx, `ALTER TABLE incidents UPDATE score_top1 = ?, score_mrr = ? WHERE incident_id = ?`,
		float32(top1), float32(mrr), incidentID)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
