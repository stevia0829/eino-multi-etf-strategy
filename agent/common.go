package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/multi-agents-etf-trade-strategy/llm"
)

// extractJSON 从 LLM 输出中提取 JSON 字符串：去掉 markdown code fence、抓取首尾大括号。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// callLLMJSON 调用 LLM 并把输出尝试解析为目标结构体。
// 当 LLM 返回非 JSON 时，将原文本写入 raw 回调，便于上层兜底。
func callLLMJSON(ctx context.Context, c llm.Client, system, user string, target interface{}, onRaw func(raw string)) error {
	out, err := c.Chat(ctx, system, user, llm.ChatOptions{Temperature: 0.2})
	if err != nil {
		return err
	}
	if onRaw != nil {
		onRaw(out)
	}
	return json.Unmarshal([]byte(extractJSON(out)), target)
}
