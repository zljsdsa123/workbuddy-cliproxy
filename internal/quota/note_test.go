package quota

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/hostrpc"
)

func TestCreditsNoteRendering(t *testing.T) {
	now := time.Now()
	if got := creditsNote(creditsState{}, now); got != "" {
		t.Fatalf("an unknown balance must render no note, got %q", got)
	}

	funded := creditsState{known: true, remaining: 699.01, checkedAt: now}
	if got := creditsNote(funded, now); got != "剩余积分 699.01" {
		t.Fatalf("creditsNote() = %q, want \"剩余积分 699.01\"", got)
	}

	cooling := creditsState{known: true, remaining: 0, checkedAt: now, exhaustedAt: now}
	if got := creditsNote(cooling, now); got != "剩余积分 0.00 · 冷却中" {
		t.Fatalf("creditsNote() = %q, want the cooling-down variant", got)
	}

	// Once the cooldown lapses the note drops the suffix again.
	if got := creditsNote(cooling, now.Add(cooldown+time.Minute)); got != "剩余积分 0.00" {
		t.Fatalf("creditsNote() after cooldown = %q, want \"剩余积分 0.00\"", got)
	}
}

func TestCreditsNoteMarksEstimatedBalance(t *testing.T) {
	now := time.Now()
	state := creditsState{known: true, remaining: 98.5, checkedAt: now, estimated: true}
	if got := creditsNote(state, now); got != "剩余积分 ~98.50" {
		t.Fatalf("creditsNote() = %q, want \"剩余积分 ~98.50\"", got)
	}
}

// TestPersistNotePreservesHostManagedFields guards the regression where the
// credits note write re-marshaled StoredAuth alone and silently dropped the
// host-managed "type"/"disabled" top-level fields. That orphaned the credential
// (its provider type wiped) and the next CPA rescan could rebind the file to
// another provider — the "workbuddy credential shown as a traework card" bug.
func TestPersistNotePreservesHostManagedFields(t *testing.T) {
	const key = "test-persist-preserves-host-fields"
	t.Cleanup(func() { creditsByAuth.Delete(key) })
	storeCreditsBalance(key, 55.5, time.Now()) // fresh balance, cache hit path

	type saveCall struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}
	var got saveCall
	hostrpc.SetTransport(func(_ string, request []byte) ([]byte, error) {
		_ = json.Unmarshal(request, &got)
		return nil, nil
	})
	t.Cleanup(func() { hostrpc.SetTransport(nil) })

	// The host hands the plugin the full credential document, including
	// top-level fields StoredAuth does not model (type/disabled/priority/…).
	raw := []byte(`{"account":{"uid":"470ad8f7","nickname":"欢乐马"},"auth":{"accessToken":"tok","refreshToken":"rt","domain":"www.codebuddy.cn"},"type":"workbuddy","disabled":true,"priority":100,"proxy_url":"socks5://127.0.0.1:1080"}`)
	sa, err := codebuddy.ParseStored(raw)
	if err != nil {
		t.Fatalf("ParseStored: %v", err)
	}

	persistNote(sa, key)

	if got.Name != codebuddy.AuthFileName {
		t.Fatalf("AuthSave name = %q, want %q", got.Name, codebuddy.AuthFileName)
	}
	var doc map[string]any
	if err := json.Unmarshal(got.JSON, &doc); err != nil {
		t.Fatalf("AuthSave body is not valid JSON: %v", err)
	}
	if doc["type"] != "workbuddy" {
		t.Fatalf("AuthSave dropped type: %v", doc["type"])
	}
	if doc["disabled"] != true {
		t.Fatalf("AuthSave dropped disabled: %v", doc["disabled"])
	}
	if doc["priority"] != float64(100) {
		t.Fatalf("AuthSave dropped priority: %v", doc["priority"])
	}
	if doc["proxy_url"] != "socks5://127.0.0.1:1080" {
		t.Fatalf("AuthSave dropped proxy_url: %v", doc["proxy_url"])
	}
	auth, _ := doc["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "tok" || auth["refreshToken"] != "rt" {
		t.Fatalf("AuthSave dropped the token fields: %v", doc["auth"])
	}
	if doc["note"] != "剩余积分 55.50" {
		t.Fatalf("note = %v, want \"剩余积分 55.50\"", doc["note"])
	}
}
