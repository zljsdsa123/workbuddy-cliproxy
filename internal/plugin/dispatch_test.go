package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestHandleMethodCountTokens: executor.count_tokens 必须路由到本地估算，
// 否则宿主会把 unknown_method 当上游故障，轮换凭据后报 auth_unavailable。
func TestHandleMethodCountTokens(t *testing.T) {
	req := pluginapi.ExecutorRequest{
		Model:   "deepseek-v4.1-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"你好"}]}`),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := HandleMethod(pluginabi.MethodExecutorCountTokens, raw)
	if err != nil {
		t.Fatalf("HandleMethod: %v", err)
	}
	var env wireEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got error %+v", env.Error)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal executor response: %v", err)
	}
	if want := `{"total_tokens":2,"usage":{"prompt_tokens":2}}`; string(resp.Payload) != want {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), want)
	}
}

type wireEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
