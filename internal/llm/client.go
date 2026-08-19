// Package llm defines the provider-agnostic Client interface used by the
// RCA pipeline. See CLAUDE.md §6.5/§6.6.
package llm

import "context"

type RankedCause struct {
	Rank       int      `json:"rank"`
	Service    string   `json:"service"`
	Cause      string   `json:"cause"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence"`
	Reasoning  string   `json:"reasoning"`
}

type RCAResult struct {
	RankedCauses     []RankedCause `json:"ranked_causes"`
	Summary          string        `json:"summary"`
	SuggestedActions []string      `json:"suggested_actions"`
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Client talks to one LLM provider. Adapters (gemini, cerebras, openrouter)
// implement this against their OpenAI-compatible endpoint.
type Client interface {
	Analyze(ctx context.Context, prompt string) (RCAResult, Usage, error)
	Model() string
}
