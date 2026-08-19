// Package prompt serializes a ContextPack into a prompt string. Kept
// separate from the ContextPack schema itself so prompt iteration never
// touches the query/collection layer. See CLAUDE.md §6.4.
package prompt

import (
	"encoding/json"
	"fmt"

	"github.com/duyquang47/llm-rca/internal/contextpack"
)

const v1Instructions = `Bạn là một hệ thống phân tích nguyên nhân gốc (root cause analysis) cho microservices chạy trên Kubernetes.

Dưới đây là context pack chứa dependency graph, chênh lệch metric so với baseline, exemplar trace, log template đã gộp trùng lặp, và k8s context liên quan tới một sự cố.

Hãy trả lời DUY NHẤT bằng JSON hợp lệ theo đúng schema sau, không thêm markdown, không thêm giải thích ngoài JSON:

{
  "ranked_causes": [
    {
      "rank": 1,
      "service": "<tên service>",
      "cause": "<mã nguyên nhân ngắn gọn, snake_case>",
      "confidence": <0.0-1.0>,
      "evidence": ["<trích dẫn bằng chứng cụ thể từ context pack, VD trace/log/metric>"],
      "reasoning": "<giải thích ngắn gọn>"
    }
  ],
  "summary": "<tóm tắt sự cố>",
  "suggested_actions": ["<hành động đề xuất>"]
}

Xếp hạng theo mức độ tin cậy giảm dần. Chỉ dựa vào bằng chứng có trong context pack, không suy đoán ngoài dữ liệu được cung cấp.

Context pack:
`

// Serialize turns a ContextPack into a v1 prompt string.
func Serialize(pack contextpack.ContextPack) (string, error) {
	packJSON, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal context pack: %w", err)
	}
	return v1Instructions + string(packJSON), nil
}
