package quota

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

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
	// sa is nil so Classify skips the background re-probe.
	err := Classify(400, body, nil, key)
	if err.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 so the host applies quota cooldown", err.Status)
	}
	if err.Code != "credits_exhausted" {
		t.Fatalf("code = %q, want credits_exhausted", err.Code)
	}
	if state, ok := loadCreditsState(key); !ok || !state.exhausted(time.Now()) {
		t.Fatal("a quota failure must start the local cooldown")
	}
}

func TestClassifyUpstreamFailurePreservesNonQuotaStatus(t *testing.T) {
	err := Classify(401, []byte(`{"code":11001,"msg":"token expired"}`), nil, "test-401")
	if err.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 preserved for the host refresh path", err.Status)
	}
	if err.Code != "11001" {
		t.Fatalf("code = %q, want the CodeBuddy business code", err.Code)
	}
	if err.Retryable {
		t.Fatal("401 must not be marked retryable")
	}
}

func TestClassifyUpstreamFailureMarksTransientRetryable(t *testing.T) {
	for _, status := range []int{408, 500, 502, 503, 504} {
		err := Classify(status, []byte("upstream boom"), nil, "test-transient")
		if err.Status != status {
			t.Fatalf("status = %d, want %d", err.Status, status)
		}
		if !err.Retryable {
			t.Fatalf("status %d must be retryable", status)
		}
	}
}

// This test lives with quota rather than with wire because the chain it guards is
// quota's: an exhausted-credits error has to reach the host still carrying 429.
func TestErrorEnvelopeForCarriesHTTPStatus(t *testing.T) {
	raw := wire.ErrorEnvelopeFor(ExhaustedError(0))

	var decoded wire.Envelope
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
