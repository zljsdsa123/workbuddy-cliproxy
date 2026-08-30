package quota

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

func TestQuotaGateBlocksExhaustedCredential(t *testing.T) {
	const key = "test-gate-blocked"
	t.Cleanup(func() { creditsByAuth.Delete(key) })

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
