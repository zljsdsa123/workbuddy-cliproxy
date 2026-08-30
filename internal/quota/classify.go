package quota

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

func ExhaustedError(remaining float64) *wire.UpstreamError {
	return wire.NewUpstreamError(
		exhaustedStatus,
		"credits_exhausted",
		fmt.Sprintf("workbuddy: CodeBuddy credits exhausted (remaining=%.2f); credential cooled down for %s", remaining, cooldown),
	)
}

// Classify maps an upstream chat failure onto a status-carrying
// error, recognising the quota-exhaustion cases so they drive credential
// cooldown rather than looking like generic faults.
func Classify(status int, body []byte, sa *codebuddy.StoredAuth, authID string) *wire.UpstreamError {
	message := truncate(strings.TrimSpace(string(body)), 400)
	code := ""
	if len(body) > 0 {
		var env codebuddy.APIEnvelope
		if json.Unmarshal(body, &env) == nil && env.Code != 0 {
			code = strconv.Itoa(env.Code)
			if msg := strings.TrimSpace(env.Msg); msg != "" {
				message = msg
			}
		}
	}

	if isQuotaExhaustedFailure(status, code, message) {
		key := Key(authID, sa)
		markCreditsExhausted(key, time.Now())
		// Re-probe in the background so the cached balance reflects reality once
		// the user tops the account up, then publish the result to the card.
		if key != "" && sa != nil {
			go func(snapshot codebuddy.StoredAuth, probeKey string) {
				_, _ = probeCredits(&snapshot, probeKey)
				persistNote(&snapshot, probeKey)
			}(*sa, key)
		}
		detail := message
		if detail == "" {
			detail = fmt.Sprintf("upstream %d", status)
		}
		return wire.NewUpstreamError(
			exhaustedStatus,
			"credits_exhausted",
			fmt.Sprintf("workbuddy: CodeBuddy credits exhausted (%s); credential cooled down for %s", detail, cooldown),
		)
	}

	detail := message
	if detail == "" {
		detail = http.StatusText(status)
	}
	errUpstream := wire.NewUpstreamError(status, code, fmt.Sprintf("workbuddy: upstream %d: %s", status, detail))
	// 5xx and 408 are transient upstream faults; the host retries those.
	switch status {
	case http.StatusRequestTimeout,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		errUpstream = errUpstream.WithRetryable(true)
	}
	return errUpstream
}

// quotaFailureKeywords are the CodeBuddy phrasings that indicate the account has
// run out of credits rather than hit a transient rate limit.
var quotaFailureKeywords = []string{
	"insufficient",
	"quota",
	"credits",
	"额度",
	"余额",
	"积分",
	"用完",
	"耗尽",
	"不足",
	"超出限制",
}

func isQuotaExhaustedFailure(status int, code, message string) bool {
	// 402 Payment Required and 429 Too Many Requests are unambiguous.
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return true
	}
	lower := strings.ToLower(message)
	for _, keyword := range quotaFailureKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
