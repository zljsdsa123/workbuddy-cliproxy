package executor

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

func countTokensRequest(t *testing.T, payload []byte) []byte {
	t.Helper()
	req := pluginapi.ExecutorRequest{
		Model:   "deepseek-v4.1-flash",
		Payload: payload,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

func decodeCountTokensEnvelope(t *testing.T, raw []byte) countTokensResponse {
	t.Helper()
	var env wire.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got error %+v", env.Error)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal executor response: %v", err)
	}
	var count countTokensResponse
	if err := json.Unmarshal(resp.Payload, &count); err != nil {
		t.Fatalf("unmarshal payload %q: %v", string(resp.Payload), err)
	}
	return count
}

func mustCountTokens(t *testing.T, payload []byte) []byte {
	t.Helper()
	out, err := CountTokens(countTokensRequest(t, payload))
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	return out
}

// TestCountTokensMixedScriptEstimate: 4 个 CJK 字符 + 6 个 ASCII 字符
// （含空格）→ 4 + ceil(6/4) = 6 tokens。
func TestCountTokensMixedScriptEstimate(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"你好世界 hello"}]}`)
	count := decodeCountTokensEnvelope(t, mustCountTokens(t, payload))
	if count.TotalTokens != 6 {
		t.Fatalf("total_tokens = %d, want 6", count.TotalTokens)
	}
	if count.Usage.PromptTokens != 6 {
		t.Fatalf("usage.prompt_tokens = %d, want 6", count.Usage.PromptTokens)
	}
}

// TestCountTokensArrayContent: content 为分段数组时汇总各段的 text。
func TestCountTokensArrayContent(t *testing.T) {
	payload := []byte(`{"messages":[
		{"role":"system","content":"系统提示"},
		{"role":"user","content":[{"type":"text","text":"你好"},{"type":"text","text":"世界"}]}
	]}`)
	count := decodeCountTokensEnvelope(t, mustCountTokens(t, payload))
	if count.TotalTokens != 8 {
		t.Fatalf("total_tokens = %d, want 8", count.TotalTokens)
	}
}

// TestCountTokensFallsBackToOriginalRequest: Payload 为空时按原始请求体估算。
func TestCountTokensFallsBackToOriginalRequest(t *testing.T) {
	req := pluginapi.ExecutorRequest{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"你好"}]}`),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := CountTokens(raw)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	count := decodeCountTokensEnvelope(t, out)
	if count.TotalTokens != 2 {
		t.Fatalf("total_tokens = %d, want 2", count.TotalTokens)
	}
}

// TestCountTokensSkipsAuth: 计数是纯本地估算，StorageJSON 缺失也不能失败。
func TestCountTokensSkipsAuth(t *testing.T) {
	count := decodeCountTokensEnvelope(t, mustCountTokens(t, []byte(`{"messages":[]}`)))
	if count.TotalTokens != 0 {
		t.Fatalf("total_tokens = %d, want 0", count.TotalTokens)
	}
}
