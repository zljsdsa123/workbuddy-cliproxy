package executor

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// aggregateSSE reads an upstream SSE stream and emits one chunk per data event.
// Empty-valued delta fields are stripped and the trailing [DONE] is dropped
// (the host appends its own stream terminator). When sseFramed is true each
// payload is emitted as a "data: " line for cross-format translators; otherwise
// the payload is the raw JSON object and the host chat-completions writer adds
// the framing itself. The credits consumed by the response are returned too.
func aggregateSSE(r io.Reader, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, float64) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	spent := 0.0
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned, credit, hasCredit := cleanChunkJSON(content)
		if hasCredit && credit > spent {
			spent = credit
		}
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	return chunks, spent
}

// cleanChunkJSON strips empty-valued fields (null/""/[]/{}) from choice deltas
// so strict clients don't trip on {"function_call":null,"tool_calls":[]}.
// It also reports the credit consumption the chunk carries, so callers can
// track spend without parsing the payload a second time.
func cleanChunkJSON(s string) (string, float64, bool) {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s, 0, false
	}
	credit, hasCredit := chunkCredit(obj)
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s, credit, hasCredit
	}
	return string(out), credit, hasCredit
}

// creditFieldNames are the spellings CodeBuddy may use for the credits consumed
// by a request. The value is a cost, not a balance.
var creditFieldNames = []string{"credit", "credits", "creditUsed", "credit_used", "creditCost", "credit_cost"}

// chunkCredit extracts the credit consumption reported by one response chunk,
// checking both the top level and the usage block.
func chunkCredit(obj map[string]any) (float64, bool) {
	if credit, ok := creditFromMap(obj); ok {
		return credit, true
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		return creditFromMap(usage)
	}
	return 0, false
}

func creditFromMap(source map[string]any) (float64, bool) {
	for _, name := range creditFieldNames {
		value, ok := source[name]
		if !ok {
			continue
		}
		if credit, ok := numericValue(value); ok {
			return credit, true
		}
	}
	return 0, false
}

// numericValue reads a credit amount that may arrive as a JSON number or as a
// string (CodeBuddy serializes precise decimals as strings elsewhere).
func numericValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		parsed, errParse := v.Float64()
		return parsed, errParse == nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		return parsed, errParse == nil
	}
	return 0, false
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// aggregateCompletion folds an SSE stream into a single non-streaming
// chat.completion object (used for non-stream client requests).
func aggregateCompletion(r io.Reader, model string) ([]byte, float64, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	var toolCalls []map[string]any
	spent := 0.0

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if credit, ok := chunkCredit(chunk); ok && credit > spent {
			spent = credit
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							toolCalls = append(toolCalls, call)
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, spent, err
	}
	return out, spent, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}
