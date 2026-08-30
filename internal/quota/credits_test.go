package quota

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
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
	if !state.exhausted(now.Add(cooldown - time.Minute)) {
		t.Fatal("expected credential to still be cooled down inside the window")
	}
	if state.exhausted(now.Add(cooldown + time.Minute)) {
		t.Fatal("expected the cooldown window to expire")
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
	if !fresh.stale(now.Add(balanceTTL + time.Second)) {
		t.Fatal("a reading older than balanceTTL must be stale")
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

func TestCreditsKeyPrefersAuthID(t *testing.T) {
	sa := &codebuddy.StoredAuth{Account: codebuddy.StoredAccount{UID: "uid-1"}}
	if got := Key("auth-1", sa); got != "auth-1" {
		t.Fatalf("Key() = %q, want auth-1", got)
	}
	if got := Key("", sa); got != "uid:uid-1" {
		t.Fatalf("Key() = %q, want uid:uid-1", got)
	}
	if got := Key("", &codebuddy.StoredAuth{}); got != "" {
		t.Fatalf("Key() = %q, want empty", got)
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
