package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChunkCreditExtraction(t *testing.T) {
	tests := []struct {
		name string
		body string
		want float64
		ok   bool
	}{
		{name: "top level number", body: `{"credit":1.5}`, want: 1.5, ok: true},
		{name: "top level string", body: `{"credit":"0.99"}`, want: 0.99, ok: true},
		{name: "inside usage", body: `{"usage":{"credit":2.25}}`, want: 2.25, ok: true},
		{name: "camel case alias", body: `{"creditUsed":3}`, want: 3, ok: true},
		{name: "snake case alias", body: `{"credit_used":4}`, want: 4, ok: true},
		{name: "plural alias", body: `{"credits":5}`, want: 5, ok: true},
		{name: "absent", body: `{"choices":[]}`, want: 0, ok: false},
		{name: "unparsable string", body: `{"credit":"n/a"}`, want: 0, ok: false},
		{name: "top level wins over usage", body: `{"credit":1,"usage":{"credit":9}}`, want: 1, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]any
			if err := json.Unmarshal([]byte(tc.body), &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, ok := chunkCredit(obj)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("chunkCredit() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCleanChunkJSONReportsCreditAndStillCleans(t *testing.T) {
	in := `{"credit":0.99,"choices":[{"delta":{"content":"hi","tool_calls":[],"function_call":null}}]}`
	cleaned, credit, ok := cleanChunkJSON(in)
	if !ok || credit != 0.99 {
		t.Fatalf("credit = (%v, %v), want (0.99, true)", credit, ok)
	}
	// The existing empty-field stripping must survive the signature change.
	if strings.Contains(cleaned, "tool_calls") || strings.Contains(cleaned, "function_call") {
		t.Fatalf("empty delta fields were not stripped: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"content":"hi"`) {
		t.Fatalf("content was lost: %s", cleaned)
	}
}

func TestCleanChunkJSONPassesThroughNonJSON(t *testing.T) {
	cleaned, credit, ok := cleanChunkJSON("not json")
	if cleaned != "not json" || ok || credit != 0 {
		t.Fatalf("got (%q, %v, %v), want passthrough with no credit", cleaned, credit, ok)
	}
}

func TestAggregateSSEReportsCreditSpend(t *testing.T) {
	// CodeBuddy reports a running total, so the largest value is the spend.
	stream := "data: {\"credit\":0.4,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
		"data: {\"credit\":0.9,\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n" +
		"data: [DONE]\n"
	chunks, spent := aggregateSSE(strings.NewReader(stream), false)
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	if spent != 0.9 {
		t.Fatalf("spent = %v, want 0.9 (the running total, not the sum)", spent)
	}
}

func TestAggregateCompletionReportsCreditSpend(t *testing.T) {
	stream := "data: {\"credit\":0.5,\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n" +
		"data: {\"credit\":1.25,\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n"
	payload, spent, err := aggregateCompletion(strings.NewReader(stream), "hy3")
	if err != nil {
		t.Fatalf("aggregateCompletion: %v", err)
	}
	if spent != 1.25 {
		t.Fatalf("spent = %v, want 1.25", spent)
	}
	if !strings.Contains(string(payload), `"content":"hello"`) {
		t.Fatalf("aggregated content is wrong: %s", payload)
	}
}
