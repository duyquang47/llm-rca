// Package gemini implements llm.Client against Google AI Studio's
// OpenAI-compatible endpoint for the Gemini API. See CLAUDE.md §6.6.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/duyquang47/llm-rca/internal/llm"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

func New(apiKey, model string) *Client {
	return &Client{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 180 * time.Second},
	}
}

func (c *Client) Model() string { return c.model }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Analyze retries exactly once on a JSON parse failure, feeding the
// parse error back to the model, before giving up — per CLAUDE.md
// §6.5: "Nếu LLM trả về JSON sai định dạng, retry 1 lần với thông báo
// lỗi kèm theo, sau đó đánh dấu incident là FAILED — không tự sửa
// bằng regex." The parse-error rate is a metric to report, not a
// defect to paper over silently.
func (c *Client) Analyze(ctx context.Context, prompt string) (llm.RCAResult, llm.Usage, error) {
	messages := []chatMessage{{Role: "user", Content: prompt}}

	result, usage, rawContent, err := c.chatOnce(ctx, messages)
	if err == nil {
		return result, usage, nil
	}
	firstErr := err

	messages = append(messages,
		chatMessage{Role: "assistant", Content: rawContent},
		chatMessage{Role: "user", Content: fmt.Sprintf(
			"Output đó không phải JSON hợp lệ, lỗi parse: %v. Trả lời lại DUY NHẤT bằng JSON hợp lệ đúng schema đã yêu cầu, không thêm markdown hay giải thích.", err)},
	)
	result, retryUsage, _, err := c.chatOnce(ctx, messages)
	usage.InputTokens += retryUsage.InputTokens
	usage.OutputTokens += retryUsage.OutputTokens
	if err != nil {
		return llm.RCAResult{}, usage, fmt.Errorf("parse failed twice, giving up: retry error: %w (first attempt: %v)", err, firstErr)
	}
	return result, usage, nil
}

// chatOnce makes one chat-completion call and tries to parse the
// result as RCAResult. rawContent is returned even on parse failure
// so Analyze can feed it back to the model for the retry.
func (c *Client) chatOnce(ctx context.Context, messages []chatMessage) (llm.RCAResult, llm.Usage, string, error) {
	reqBody := chatRequest{Model: c.model, Messages: messages}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("call gemini: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("read response: %w", err)
	}

	parsed, err := parseChatResponse(respBody)
	if err != nil {
		return llm.RCAResult{}, llm.Usage{}, "", err
	}
	if parsed.Error != nil {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("gemini error (status %d): %s", resp.StatusCode, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return llm.RCAResult{}, llm.Usage{}, "", fmt.Errorf("no choices in response (raw: %s)", respBody)
	}

	usage := llm.Usage{
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
	}
	rawContent := parsed.Choices[0].Message.Content

	content := extractJSON(rawContent)
	var result llm.RCAResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return llm.RCAResult{}, usage, rawContent, fmt.Errorf("parse RCAResult JSON: %w (raw model output: %s)", err, rawContent)
	}

	return result, usage, rawContent, nil
}

// parseChatResponse handles both the normal object response and the
// array-wrapped error format some Google API error paths return
// (e.g. `[{"error": {...}}]`).
func parseChatResponse(body []byte) (chatResponse, error) {
	var obj chatResponse
	if err := json.Unmarshal(body, &obj); err == nil {
		return obj, nil
	}

	var arr []chatResponse
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		return arr[0], nil
	}

	return chatResponse{}, fmt.Errorf("unmarshal response as object or array (raw: %s)", body)
}

// extractJSON strips markdown code fences (```json ... ```) that models
// sometimes wrap structured output in, even when asked for raw JSON.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}
