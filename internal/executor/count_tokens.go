// count_tokens.go executor 的本地 token 估算：CodeBuddy 没有上游计数端点，
// executor.count_tokens 只做本地字符估算 —— CJK 字符按 1 token/字计，
// 其余字符按 4 字符/token 折算。结果是粗估值，但能让 Claude Code /
// Gemini 的 countTokens 调用正常返回，而不是把 unknown_method 当成
// 上游故障去轮换凭据。
package executor

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

type countUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
}

type countTokensResponse struct {
	TotalTokens int64      `json:"total_tokens"`
	Usage       countUsage `json:"usage"`
}

// CountTokens handles executor.count_tokens. Counting is purely local, so it
// deliberately skips auth parsing and quota gating: an expired or cooled-down
// credential must not make a local estimate fail.
func CountTokens(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	count := estimateInputTokens(body)
	payload, err := json.Marshal(countTokensResponse{
		TotalTokens: count,
		Usage:       countUsage{PromptTokens: count},
	})
	if err != nil {
		return nil, err
	}
	// total_tokens 是插件契约字段（见 CPA examples/plugin/executor/go）；
	// usage.prompt_tokens 供宿主的跨格式响应翻译器提取输入 token 数。
	return wire.OKEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// estimateInputTokens walks the chat-completions messages and sums a rough
// per-rune estimate: CJK runes count as one token each, everything else as
// one token per four runes. Tool definitions and JSON structure overhead are
// intentionally ignored — this is an estimate, not a tokenizer.
func estimateInputTokens(body []byte) int64 {
	var chat struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return 0
	}
	var cjk, rest int64
	for _, message := range chat.Messages {
		for _, r := range messageText(message) {
			if isCJK(r) {
				cjk++
			} else {
				rest++
			}
		}
	}
	return cjk + (rest+3)/4
}

// messageText extracts the text carried by one chat-completions message:
// content is either a plain string or an array of parts with "text" fields.
func messageText(message json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &m); err != nil {
		return ""
	}
	var plain string
	if err := json.Unmarshal(m.Content, &plain); err == nil {
		return plain
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return ""
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
