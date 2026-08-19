// Command phase0 is the thin-slice validation described in CLAUDE.md §10
// Phase 0: read a hand-written ContextPack fixture, serialize it to a
// prompt, call a real LLM once, and print the result. No cluster required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/duyquang47/llm-rca/internal/contextpack"
	"github.com/duyquang47/llm-rca/internal/llm/gemini"
	"github.com/duyquang47/llm-rca/internal/prompt"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY chưa được set — lấy key miễn phí tại https://aistudio.google.com/apikey")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.6-flash"
	}

	fixturePath := "testdata/phase0/scenario1_timeout_cascade.json"
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		return fmt.Errorf("read fixture: %w", err)
	}

	var pack contextpack.ContextPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return fmt.Errorf("unmarshal fixture: %w", err)
	}

	p, err := prompt.Serialize(pack)
	if err != nil {
		return fmt.Errorf("serialize prompt: %w", err)
	}

	fmt.Println("===== PROMPT GỬI CHO LLM =====")
	fmt.Println(p)
	fmt.Println()

	client := gemini.New(apiKey, model)
	ctx := context.Background()

	result, usage, err := client.Analyze(ctx, p)
	if err != nil {
		return fmt.Errorf("analyze: %w", err)
	}

	fmt.Println("===== KẾT QUẢ RCAResult =====")
	pretty, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(pretty))
	fmt.Println()
	fmt.Printf("===== USAGE ===== model=%s input_tokens=%d output_tokens=%d\n",
		client.Model(), usage.InputTokens, usage.OutputTokens)

	return nil
}
