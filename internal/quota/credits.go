// Package quota tracks CodeBuddy's prepaid credits per credential.
//
// CodeBuddy meters usage as prepaid credits packages rather than a rate limit and
// exposes no reset timestamp — packages are topped up manually — so this package
// maintains the cooldown locally: an authoritative zero (billing API, or an
// upstream quota error) puts a credential to sleep for cooldown, and the balance
// is re-probed on that cadence.
package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
)

const (
	// cooldown is how long a credential stays locally exhausted once its
	// credits run out. CodeBuddy exposes no reset timestamp (packages are
	// recharged manually), so we re-probe the balance on this cadence instead of
	// waiting for a provider-declared recovery time.
	cooldown = 30 * time.Minute
	// balanceTTL bounds how long a successful balance reading is trusted
	// before the next upstream call re-probes it.
	balanceTTL = 6 * time.Hour
	// probeTimeout bounds the billing API call so a slow balance probe
	// cannot stall the chat request that triggered it.
	probeTimeout = 10 * time.Second
	// exhaustedStatus is the HTTP status reported to CPA when credits are
	// exhausted. 429 is what drives quota cooldown + credential rotation in
	// the host's MarkResult handling.
	exhaustedStatus = http.StatusTooManyRequests
)

// creditsState is the cached credits balance for one credential.
//
// CodeBuddy meters usage as prepaid "credits" packages rather than a rate limit,
// and exposes no reset timestamp — packages are topped up manually. So instead of
// waiting for a provider-declared recovery time, an exhausted credential is held
// down locally for cooldown and then re-probed.
type creditsState struct {
	remaining   float64
	known       bool
	checkedAt   time.Time
	exhaustedAt time.Time // non-zero while the credential is locally cooled down
	// estimated marks a balance that has been locally debited from per-request
	// credit reports since the last authoritative billing reading.
	estimated bool
}

// exhausted reports whether the credential is still inside its local cooldown.
func (s creditsState) exhausted(now time.Time) bool {
	return !s.exhaustedAt.IsZero() && now.Sub(s.exhaustedAt) < cooldown
}

// stale reports whether the cached balance is too old to trust.
func (s creditsState) stale(now time.Time) bool {
	return !s.known || now.Sub(s.checkedAt) >= balanceTTL
}

var (
	creditsByAuth  sync.Map // auth key(string) -> creditsState
	creditsProbeMu sync.Map // auth key(string) -> *sync.Mutex, collapses concurrent probes
)

// Key identifies the credential a credits reading belongs to. The
// executor request's AuthID is authoritative; the account UID is the fallback
// for paths that do not carry one (a single credential per CodeBuddy account).
func Key(authID string, sa *codebuddy.StoredAuth) string {
	if key := strings.TrimSpace(authID); key != "" {
		return key
	}
	if sa != nil {
		if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
			return "uid:" + uid
		}
	}
	return ""
}

func loadCreditsState(key string) (creditsState, bool) {
	if key == "" {
		return creditsState{}, false
	}
	value, ok := creditsByAuth.Load(key)
	if !ok {
		return creditsState{}, false
	}
	state, ok := value.(creditsState)
	if !ok {
		creditsByAuth.Delete(key)
		return creditsState{}, false
	}
	return state, true
}

// storeCreditsBalance records a fresh balance reading, entering or leaving the
// local cooldown according to whether any credits remain.
func storeCreditsBalance(key string, remaining float64, now time.Time) {
	if key == "" {
		return
	}
	state := creditsState{remaining: remaining, known: true, checkedAt: now}
	if remaining <= 0 {
		// Preserve the original exhaustion timestamp so repeated probes during a
		// cooldown window do not keep extending it.
		if prev, ok := loadCreditsState(key); ok && !prev.exhaustedAt.IsZero() {
			state.exhaustedAt = prev.exhaustedAt
		} else {
			state.exhaustedAt = now
		}
	}
	creditsByAuth.Store(key, state)
}

// markCreditsExhausted starts a local cooldown after the upstream reported a
// quota failure, even when the balance API has not been consulted yet.
func markCreditsExhausted(key string, now time.Time) {
	if key == "" {
		return
	}
	state, _ := loadCreditsState(key)
	state.remaining = 0
	state.known = true
	state.checkedAt = now
	// The upstream itself reported exhaustion, so zero is authoritative here.
	state.estimated = false
	if state.exhaustedAt.IsZero() {
		state.exhaustedAt = now
	}
	creditsByAuth.Store(key, state)
}

func creditsProbeLock(key string) *sync.Mutex {
	value, _ := creditsProbeMu.LoadOrStore(key, &sync.Mutex{})
	mu, ok := value.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
		creditsProbeMu.Store(key, mu)
	}
	return mu
}

// userResourceResponse models the get-user-resource billing payload. Only the
// per-cycle remaining credits are needed; CycleCapacityRemainPrecise is the
// authoritative field and is serialized as a string.
type userResourceResponse struct {
	Response struct {
		Data struct {
			Accounts []struct {
				ProductCode                string  `json:"ProductCode"`
				Status                     int     `json:"Status"`
				CycleCapacityRemainPrecise string  `json:"CycleCapacityRemainPrecise"`
				CycleCapacityRemain        float64 `json:"CycleCapacityRemain"`
			} `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// totalRemainingCredits sums the usable per-cycle balance across a user's
// CodeBuddy resource packages. Multiple packages stack (a base package plus
// bonus packages), so the credential is only exhausted once all of them are.
func (r userResourceResponse) totalRemainingCredits() float64 {
	total := 0.0
	for _, account := range r.Response.Data.Accounts {
		if code := strings.TrimSpace(account.ProductCode); code != "" && code != codebuddy.QuotaProductCode {
			continue
		}
		remaining := account.CycleCapacityRemain
		if precise := strings.TrimSpace(account.CycleCapacityRemainPrecise); precise != "" {
			if parsed, errParse := strconv.ParseFloat(precise, 64); errParse == nil {
				remaining = parsed
			}
		}
		if remaining > 0 {
			total += remaining
		}
	}
	return total
}

// probeCredits queries the billing API for the credential's remaining credits
// and caches the result. Concurrent callers for the same credential collapse
// onto one upstream call.
func probeCredits(sa *codebuddy.StoredAuth, key string) (creditsState, error) {
	if key == "" {
		return creditsState{}, fmt.Errorf("credits probe: missing auth key")
	}
	mu := creditsProbeLock(key)
	mu.Lock()
	defer mu.Unlock()

	// Another caller may have refreshed the balance while we waited on the lock.
	if state, ok := loadCreditsState(key); ok && !state.stale(time.Now()) {
		return state, nil
	}

	reqBody, errMarshal := json.Marshal(map[string]any{
		"PageNumber":      1,
		"PageSize":        100,
		"ProductCode":     codebuddy.QuotaProductCode,
		"Status":          []int{0, 3},
		"OnlyValidPeriod": true,
	})
	if errMarshal != nil {
		return creditsState{}, fmt.Errorf("credits probe: marshal request: %w", errMarshal)
	}

	// 按该凭据的生效代理出网：走与 JSONClient 同一份连接池传输（短超时另配）。
	// 生效代理为空 → codebuddy.Transport 返回共享默认传输，语义与改造前一致。
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: codebuddy.Transport(codebuddy.EffectiveProxy(nil, sa)),
	}
	data, status, errCall := codebuddy.DoJSON(client, http.MethodPost, codebuddy.EndpointUserResource, func(r *http.Request) {
		codebuddy.BackendHeaders(r, sa)
	}, bytes.NewReader(reqBody))
	if errCall != nil {
		if status > 0 {
			return creditsState{}, fmt.Errorf("credits probe: upstream %d: %w", status, errCall)
		}
		return creditsState{}, fmt.Errorf("credits probe: %w", errCall)
	}

	var resource userResourceResponse
	if errUnmarshal := json.Unmarshal(data, &resource); errUnmarshal != nil {
		return creditsState{}, fmt.Errorf("credits probe: parse response: %w", errUnmarshal)
	}

	now := time.Now()
	remaining := resource.totalRemainingCredits()
	storeCreditsBalance(key, remaining, now)
	state, _ := loadCreditsState(key)
	return state, nil
}
