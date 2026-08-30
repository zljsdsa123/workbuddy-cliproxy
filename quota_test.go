package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sampleUserResource is a real get-user-resource response: three stacked
// CodeBuddy packages whose per-cycle remainders are 499.01, 100 and 100.
const sampleUserResource = `{
  "Response": {
    "Data": {
      "TotalCount": 3,
      "Accounts": [
        {
          "ProductCode": "p_tcaca",
          "Status": 0,
          "CapacityRemain": 500,
          "CycleCapacityRemain": 499,
          "CycleCapacityRemainPrecise": "499.01"
        },
        {
          "ProductCode": "p_tcaca",
          "Status": 0,
          "CapacityRemain": 100,
          "CycleCapacityRemain": 100,
          "CycleCapacityRemainPrecise": "100"
        },
        {
          "ProductCode": "p_tcaca",
          "Status": 0,
          "CapacityRemain": 100,
          "CycleCapacityRemain": 100,
          "CycleCapacityRemainPrecise": "100"
        }
      ]
    },
    "RequestId": "b10be111-3c1b-42d7-878c-785d3df8e894"
  }
}`

func TestTotalRemainingCreditsSumsPackages(t *testing.T) {
	var resource userResourceResponse
	if err := json.Unmarshal([]byte(sampleUserResource), &resource); err != nil {
		t.Fatalf("unmarshal sample response: %v", err)
	}
	got := resource.totalRemainingCredits()
	want := 699.01
	if got < want-0.001 || got > want+0.001 {
		t.Fatalf("totalRemainingCredits() = %v, want %v", got, want)
	}
}

func TestTotalRemainingCreditsPrefersPreciseField(t *testing.T) {
	// CycleCapacityRemain rounds down to 499; the precise string is authoritative.
	const body = `{"Response":{"Data":{"Accounts":[
		{"ProductCode":"p_tcaca","CycleCapacityRemain":499,"CycleCapacityRemainPrecise":"499.87"}]}}}`
	var resource userResourceResponse
	if err := json.Unmarshal([]byte(body), &resource); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resource.totalRemainingCredits(); got != 499.87 {
		t.Fatalf("totalRemainingCredits() = %v, want 499.87", got)
	}
}

func TestTotalRemainingCreditsFallsBackWhenPreciseMissing(t *testing.T) {
	const body = `{"Response":{"Data":{"Accounts":[
		{"ProductCode":"p_tcaca","CycleCapacityRemain":42}]}}}`
	var resource userResourceResponse
	if err := json.Unmarshal([]byte(body), &resource); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resource.totalRemainingCredits(); got != 42 {
		t.Fatalf("totalRemainingCredits() = %v, want 42", got)
	}
}

func TestTotalRemainingCreditsSkipsOtherProducts(t *testing.T) {
	const body = `{"Response":{"Data":{"Accounts":[
		{"ProductCode":"p_other","CycleCapacityRemainPrecise":"9999"},
		{"ProductCode":"p_tcaca","CycleCapacityRemainPrecise":"10"}]}}}`
	var resource userResourceResponse
	if err := json.Unmarshal([]byte(body), &resource); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resource.totalRemainingCredits(); got != 10 {
		t.Fatalf("totalRemainingCredits() = %v, want 10 (other products excluded)", got)
	}
}

func TestTotalRemainingCreditsExhausted(t *testing.T) {
	const body = `{"Response":{"Data":{"Accounts":[
		{"ProductCode":"p_tcaca","CycleCapacityRemainPrecise":"0"},
		{"ProductCode":"p_tcaca","CycleCapacityRemainPrecise":"0"}]}}}`
	var resource userResourceResponse
	if err := json.Unmarshal([]byte(body), &resource); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resource.totalRemainingCredits(); got != 0 {
		t.Fatalf("totalRemainingCredits() = %v, want 0", got)
	}
}

func TestCreditsStateExhaustedWindow(t *testing.T) {
	now := time.Now()
	state := creditsState{exhaustedAt: now}
	if !state.exhausted(now.Add(quotaCooldown - time.Minute)) {
		t.Fatal("expected credential to still be cooled down inside the window")
	}
	if state.exhausted(now.Add(quotaCooldown + time.Minute)) {
		t.Fatal("expected cooldown to expire after quotaCooldown")
	}
	if (creditsState{}).exhausted(now) {
		t.Fatal("zero state must not report exhausted")
	}
}

func TestCreditsStateStale(t *testing.T) {
	now := time.Now()
	fresh := creditsState{known: true, checkedAt: now}
	if fresh.stale(now.Add(time.Minute)) {
		t.Fatal("a recent reading must not be stale")
	}
	if !fresh.stale(now.Add(quotaBalanceTTL + time.Second)) {
		t.Fatal("a reading older than quotaBalanceTTL must be stale")
	}
	if !(creditsState{}).stale(now) {
		t.Fatal("an unknown balance must always be stale")
	}
}

func TestStoreCreditsBalancePreservesExhaustionStart(t *testing.T) {
	const key = "test-preserve-exhaustion"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	start := time.Now()
	storeCreditsBalance(key, 0, start)
	first, ok := loadCreditsState(key)
	if !ok || first.exhaustedAt.IsZero() {
		t.Fatal("expected exhaustion to be recorded on a zero balance")
	}

	// A later probe that still reads zero must not extend the cooldown window.
	storeCreditsBalance(key, 0, start.Add(5*time.Minute))
	second, _ := loadCreditsState(key)
	if !second.exhaustedAt.Equal(first.exhaustedAt) {
		t.Fatalf("exhaustedAt moved from %v to %v; cooldown must not be extended by re-probing",
			first.exhaustedAt, second.exhaustedAt)
	}
}

func TestStoreCreditsBalanceClearsExhaustionOnTopUp(t *testing.T) {
	const key = "test-topup"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	now := time.Now()
	storeCreditsBalance(key, 0, now)
	storeCreditsBalance(key, 250, now.Add(time.Minute))

	state, ok := loadCreditsState(key)
	if !ok {
		t.Fatal("expected cached state")
	}
	if !state.exhaustedAt.IsZero() {
		t.Fatal("a positive balance must clear the exhaustion marker")
	}
	if state.exhausted(now.Add(time.Minute)) {
		t.Fatal("credential must be usable again after a top-up")
	}
}

func TestMarkCreditsExhaustedStartsCooldown(t *testing.T) {
	const key = "test-mark-exhausted"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	now := time.Now()
	markCreditsExhausted(key, now)
	state, ok := loadCreditsState(key)
	if !ok {
		t.Fatal("expected cached state")
	}
	if !state.exhausted(now) {
		t.Fatal("expected credential to be cooled down")
	}
	if state.remaining != 0 {
		t.Fatalf("remaining = %v, want 0", state.remaining)
	}
}

func TestIsQuotaExhaustedFailure(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		code    string
		message string
		want    bool
	}{
		{name: "429 is quota", status: http.StatusTooManyRequests, want: true},
		{name: "402 is quota", status: http.StatusPaymentRequired, want: true},
		{name: "english insufficient", status: 400, message: "insufficient credits", want: true},
		{name: "chinese balance", status: 400, message: "账户余额不足", want: true},
		{name: "chinese used up", status: 400, message: "额度已用完", want: true},
		{name: "unrelated 400", status: 400, message: "invalid request body", want: false},
		{name: "server error", status: 500, message: "internal error", want: false},
		{name: "empty", status: 400, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQuotaExhaustedFailure(tc.status, tc.code, tc.message); got != tc.want {
				t.Fatalf("isQuotaExhaustedFailure(%d, %q, %q) = %v, want %v",
					tc.status, tc.code, tc.message, got, tc.want)
			}
		})
	}
}

func TestClassifyUpstreamFailureMapsQuotaTo429(t *testing.T) {
	const key = "test-classify-quota"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	body := []byte(`{"code":11302,"msg":"账户额度不足"}`)
	// sa is nil so classifyUpstreamFailure skips the background re-probe.
	err := classifyUpstreamFailure(400, body, nil, key)
	if err.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 so the host applies quota cooldown", err.status)
	}
	if err.code != "credits_exhausted" {
		t.Fatalf("code = %q, want credits_exhausted", err.code)
	}
	if state, ok := loadCreditsState(key); !ok || !state.exhausted(time.Now()) {
		t.Fatal("a quota failure must start the local cooldown")
	}
}

func TestClassifyUpstreamFailurePreservesNonQuotaStatus(t *testing.T) {
	err := classifyUpstreamFailure(401, []byte(`{"code":11001,"msg":"token expired"}`), nil, "test-401")
	if err.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 preserved for the host refresh path", err.status)
	}
	if err.code != "11001" {
		t.Fatalf("code = %q, want the CodeBuddy business code", err.code)
	}
	if err.retryable {
		t.Fatal("401 must not be marked retryable")
	}
}

func TestClassifyUpstreamFailureMarksTransientRetryable(t *testing.T) {
	for _, status := range []int{408, 500, 502, 503, 504} {
		err := classifyUpstreamFailure(status, []byte("upstream boom"), nil, "test-transient")
		if err.status != status {
			t.Fatalf("status = %d, want %d", err.status, status)
		}
		if !err.retryable {
			t.Fatalf("status %d must be retryable", status)
		}
	}
}

func TestErrorEnvelopeForCarriesHTTPStatus(t *testing.T) {
	raw := errorEnvelopeFor(quotaExhaustedError(0))

	var decoded envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if decoded.OK {
		t.Fatal("expected a failure envelope")
	}
	if decoded.Error == nil {
		t.Fatal("expected an error payload")
	}
	// This is the field the host reads in decodeEnvelopeResult; without it the
	// 429 never reaches MarkResult and the credential is never cooled down.
	if decoded.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", decoded.Error.HTTPStatus)
	}
	if decoded.Error.Code != "credits_exhausted" {
		t.Fatalf("code = %q, want credits_exhausted", decoded.Error.Code)
	}
}

func TestErrorEnvelopeForPlainError(t *testing.T) {
	raw := errorEnvelopeFor(errPlain{})

	var decoded envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if decoded.Error == nil || decoded.Error.HTTPStatus != 0 {
		t.Fatalf("a plain error must not fabricate an HTTP status, got %+v", decoded.Error)
	}
	if decoded.Error.Code != "plugin_error" {
		t.Fatalf("code = %q, want plugin_error", decoded.Error.Code)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "boom" }

func TestQuotaGateBlocksExhaustedCredential(t *testing.T) {
	const key = "test-gate-blocked"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	markCreditsExhausted(key, time.Now())

	err := quotaGate(&storedAuth{}, key)
	if err == nil {
		t.Fatal("expected the gate to reject an exhausted credential")
	}
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("expected an *upstreamError, got %T", err)
	}
	if upstream.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", upstream.status)
	}
}

func TestQuotaGateAllowsFreshPositiveBalance(t *testing.T) {
	const key = "test-gate-allowed"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 120, time.Now())

	if err := quotaGate(&storedAuth{}, key); err != nil {
		t.Fatalf("expected the gate to allow a funded credential, got %v", err)
	}
}

func TestCreditsKeyPrefersAuthID(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-1"}}
	if got := creditsKey("auth-1", sa); got != "auth-1" {
		t.Fatalf("creditsKey() = %q, want auth-1", got)
	}
	if got := creditsKey("", sa); got != "uid:uid-1" {
		t.Fatalf("creditsKey() = %q, want uid:uid-1", got)
	}
	if got := creditsKey("", &storedAuth{}); got != "" {
		t.Fatalf("creditsKey() = %q, want empty", got)
	}
}

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
	if got := creditsNote(cooling, now.Add(quotaCooldown+time.Minute)); got != "剩余积分 0.00" {
		t.Fatalf("creditsNote() after cooldown = %q, want \"剩余积分 0.00\"", got)
	}
}

func TestStoredAuthNoteRoundTrips(t *testing.T) {
	// The host merges metadata back into the auth file, so a note written by the
	// plugin must survive a marshal/parse cycle to stay visible after a restart.
	original := &storedAuth{
		Auth: storedTokens{AccessToken: "token-1", RefreshToken: "refresh-1"},
		Note: "剩余积分 699.01",
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	if parsed.Note != "剩余积分 699.01" {
		t.Fatalf("Note = %q, want it preserved across the round trip", parsed.Note)
	}
}

func TestToAuthDataExposesNote(t *testing.T) {
	data := toAuthData(&storedAuth{
		Auth: storedTokens{AccessToken: "token-1"},
		Note: "剩余积分 500.00",
	})

	if got := data.Metadata["note"]; got != "剩余积分 500.00" {
		t.Fatalf("metadata[note] = %v, want the credits summary (this is what the card renders)", got)
	}
	if got := data.Metadata["type"]; got != providerName {
		t.Fatalf("metadata[type] = %v, want %q", got, providerName)
	}
}

func TestToAuthDataOmitsEmptyNote(t *testing.T) {
	data := toAuthData(&storedAuth{Auth: storedTokens{AccessToken: "token-1"}})
	if _, ok := data.Metadata["note"]; ok {
		t.Fatal("an empty note must not be published as metadata")
	}
}

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

func TestDebitCreditsReducesBalance(t *testing.T) {
	const key = "test-debit"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 100, time.Now())
	state, ok := debitCredits(key, 1.5)
	if !ok {
		t.Fatal("expected the debit to apply")
	}
	if state.remaining != 98.5 {
		t.Fatalf("remaining = %v, want 98.5", state.remaining)
	}
	if !state.estimated {
		t.Fatal("a locally debited balance must be marked estimated")
	}
}

func TestDebitCreditsClampsAtZeroWithoutCooldown(t *testing.T) {
	const key = "test-debit-clamp"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 1, time.Now())
	state, ok := debitCredits(key, 50)
	if !ok {
		t.Fatal("expected the debit to apply")
	}
	if state.remaining != 0 {
		t.Fatalf("remaining = %v, want 0 (clamped)", state.remaining)
	}
	// A local estimate must never strand a credential: only the billing API or an
	// upstream quota error may start a cooldown.
	if state.exhausted(time.Now()) {
		t.Fatal("a local debit reaching zero must not start a cooldown")
	}
}

func TestDebitCreditsRequiresBaseline(t *testing.T) {
	const key = "test-debit-no-baseline"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	if _, ok := debitCredits(key, 5); ok {
		t.Fatal("a debit without an authoritative baseline must be refused")
	}
	if _, ok := debitCredits("", 5); ok {
		t.Fatal("an empty key must be refused")
	}
	storeCreditsBalance(key, 10, time.Now())
	if _, ok := debitCredits(key, 0); ok {
		t.Fatal("a zero credit report must be ignored")
	}
}

func TestProbeResultClearsEstimatedMark(t *testing.T) {
	const key = "test-reconcile"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 100, time.Now())
	if _, ok := debitCredits(key, 10); !ok {
		t.Fatal("expected the debit to apply")
	}
	if state, _ := loadCreditsState(key); !state.estimated {
		t.Fatal("expected the balance to be marked estimated")
	}

	// A fresh authoritative reading reconciles the drift.
	storeCreditsBalance(key, 85, time.Now())
	state, _ := loadCreditsState(key)
	if state.estimated {
		t.Fatal("an authoritative reading must clear the estimated mark")
	}
	if state.remaining != 85 {
		t.Fatalf("remaining = %v, want the authoritative 85", state.remaining)
	}
}

func TestCreditsNoteMarksEstimatedBalance(t *testing.T) {
	now := time.Now()
	state := creditsState{known: true, remaining: 98.5, checkedAt: now, estimated: true}
	if got := creditsNote(state, now); got != "剩余积分 ~98.50" {
		t.Fatalf("creditsNote() = %q, want \"剩余积分 ~98.50\"", got)
	}
}

func TestDebitCreditsIsAtomicUnderConcurrency(t *testing.T) {
	const key = "test-debit-race"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 100, time.Now())

	// 50 concurrent debits of 1 credit each must all land: a read-modify-write
	// that is not serialized would lose some of them.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			debitCredits(key, 1)
		}()
	}
	wg.Wait()

	state, _ := loadCreditsState(key)
	if state.remaining != 50 {
		t.Fatalf("remaining = %v, want 50 (no lost deductions)", state.remaining)
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
