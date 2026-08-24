package main

import (
	"testing"

	"github.com/duyquang47/llm-rca/internal/groundtruth"
	"github.com/duyquang47/llm-rca/internal/llm"
)

func TestScoreMultiCause(t *testing.T) {
	causes := []groundtruth.Cause{
		{Service: "inventory", Cause: "downstream_latency"},
		{Service: "thirdparty-mock", Cause: "third_party_rate_limit"},
	}

	tests := []struct {
		name         string
		ranked       []llm.RankedCause
		wantRecall   float64
		wantPerCause map[string]int
	}{
		{
			name: "both causes found",
			ranked: []llm.RankedCause{
				{Rank: 1, Service: "inventory"},
				{Rank: 2, Service: "thirdparty-mock"},
			},
			wantRecall:   1.0,
			wantPerCause: map[string]int{"inventory": 1, "thirdparty-mock": 2},
		},
		{
			name: "only one cause found",
			ranked: []llm.RankedCause{
				{Rank: 1, Service: "inventory"},
				{Rank: 2, Service: "gateway"},
			},
			wantRecall:   0.5,
			wantPerCause: map[string]int{"inventory": 1, "thirdparty-mock": 0},
		},
		{
			name: "neither cause found — model listed only a distractor",
			ranked: []llm.RankedCause{
				{Rank: 1, Service: "gateway"},
			},
			wantRecall:   0.0,
			wantPerCause: map[string]int{"inventory": 0, "thirdparty-mock": 0},
		},
		{
			name:         "empty ranking",
			ranked:       nil,
			wantRecall:   0.0,
			wantPerCause: map[string]int{"inventory": 0, "thirdparty-mock": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := llm.RCAResult{RankedCauses: tt.ranked}
			gotRecall, gotPerCause := scoreMultiCause(causes, result)
			if gotRecall != tt.wantRecall {
				t.Errorf("recall = %v, want %v", gotRecall, tt.wantRecall)
			}
			for svc, wantRank := range tt.wantPerCause {
				if gotPerCause[svc] != wantRank {
					t.Errorf("perCauseRank[%s] = %d, want %d", svc, gotPerCause[svc], wantRank)
				}
			}
		})
	}
}
