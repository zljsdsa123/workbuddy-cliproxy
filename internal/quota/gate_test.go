package quota

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

// billingStub answers the credits-probe endpoint with a fixed balance and counts
// how many times it was actually reached, which is what the gate's re-probe
// throttle is asserted on.
type billingStub struct {
	remaining string
	requests  int32
}

func (b *billingStub) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&b.requests, 1)
	body := fmt.Sprintf(
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[{"ProductCode":"p_tcaca","CycleCapacityRemainPrecise":%q}]}}}}`,
		b.remaining)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (b *billingStub) count() int32 { return atomic.LoadInt32(&b.requests) }

// stubBilling swaps the transport on the shared CodeBuddy client (a credential
// with no proxy_url probes through it) for the duration of one test.
func stubBilling(t *testing.T, remaining string) *billingStub {
	t.Helper()
	stub := &billingStub{remaining: remaining}
	shared := codebuddy.SharedHTTPClient()
	prev := shared.Transport
	t.Cleanup(func() { shared.Transport = prev })
	shared.Transport = stub
	return stub
}

func TestQuotaGateBlocksExhaustedCredential(t *testing.T) {
	const key = "test-gate-blocked"
	t.Cleanup(func() { creditsByAuth.Delete(key) })
	stubBilling(t, "100")

	markCreditsExhausted(key, time.Now())

	err := Gate(&codebuddy.StoredAuth{}, key)
	if err == nil {
		t.Fatal("expected the gate to reject an exhausted credential")
	}
	var upstream *wire.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("expected a *wire.UpstreamError, got %T", err)
	}
	if upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", upstream.Status)
	}
}

func TestQuotaGateAllowsFreshPositiveBalance(t *testing.T) {
	const key = "test-gate-allowed"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

	storeCreditsBalance(key, 120, time.Now())

	if err := Gate(&codebuddy.StoredAuth{}, key); err != nil {
		t.Fatalf("expected the gate to allow a funded credential, got %v", err)
	}
}

// TestQuotaGateRecoversWhenBalanceReturns is the regression for the "假 429"
// failure mode: the credential was cooled down while the account was empty, the
// balance came back (a top-up or the daily bonus package), and the gate kept
// re-asserting the cached zero from the stale snapshot — so a funded account
// stayed locked out for the whole cooldown window and, under client retries,
// indefinitely. The gate must consult the billing API and let the request pass.
func TestQuotaGateRecoversWhenBalanceReturns(t *testing.T) {
	const key = "test-gate-topup-recovery"
	t.Cleanup(func() { creditsByAuth.Delete(key) })
	stub := stubBilling(t, "1275")

	// ZoomCreditsExhausted stamps checkedAt = now, which is inside the re-probe
	// throttle; age it so the gate is allowed to ask the billing API again — the
	// state a retrying client reaches a minute after the cooldown started.
	markCreditsExhausted(key, time.Now().Add(-exhaustedReprobeInterval-time.Second))

	if err := Gate(&codebuddy.StoredAuth{}, key); err != nil {
		t.Fatalf("expected a topped-up credential to be released, got %v", err)
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("billing API hit %d times, want 1", got)
	}
	state, _ := loadCreditsState(key)
	if state.exhausted(time.Now()) {
		t.Fatal("a positive reading must clear the cooldown")
	}
	if state.remaining != 1275 {
		t.Fatalf("remaining = %v, want the authoritative 1275", state.remaining)
	}
}

// TestQuotaGateThrottlesReprobeWhileExhausted: the recovery probe must not turn
// every retried request into a billing call.
func TestQuotaGateThrottlesReprobeWhileExhausted(t *testing.T) {
	const key = "test-gate-reprobe-throttle"
	t.Cleanup(func() { creditsByAuth.Delete(key) })
	stub := stubBilling(t, "0")

	markCreditsExhausted(key, time.Now())

	for i := 0; i < 3; i++ {
		err := Gate(&codebuddy.StoredAuth{}, key)
		if err == nil {
			t.Fatal("expected the gate to keep blocking a credential with no credits")
		}
	}
	if got := stub.count(); got != 0 {
		t.Fatalf("billing API hit %d times inside the throttle window, want 0", got)
	}
}

// TestQuotaGateStaysBlockedWhenReprobeStillZero: the re-probe must not become a
// way for an empty account to reach the upstream.
func TestQuotaGateStaysBlockedWhenReprobeStillZero(t *testing.T) {
	const key = "test-gate-reprobe-zero"
	t.Cleanup(func() { creditsByAuth.Delete(key) })
	stub := stubBilling(t, "0")

	markCreditsExhausted(key, time.Now().Add(-exhaustedReprobeInterval-time.Second))

	err := Gate(&codebuddy.StoredAuth{}, key)
	if err == nil {
		t.Fatal("expected the gate to keep blocking an empty account")
	}
	var upstream *wire.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 so the host keeps rotating, got %v", err)
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("billing API hit %d times, want 1", got)
	}
	// Re-reading zero must not push the cooldown window out again.
	state, _ := loadCreditsState(key)
	if !state.exhausted(time.Now()) {
		t.Fatal("an empty account must stay cooled down")
	}
}
