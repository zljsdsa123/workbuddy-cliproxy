package quota

import (
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
)

// Gate is the pre-flight credits check. It short-circuits a request when
// the credential is known to be exhausted, so CPA cools it down and rotates to
// another credential instead of burning a round trip upstream.
//
// A probe failure is never fatal: the request proceeds and the upstream response
// remains the authoritative quota signal.
func Gate(sa *codebuddy.StoredAuth, authID string) error {
	key := Key(authID, sa)
	if key == "" {
		return nil
	}
	now := time.Now()

	state, cached := loadCreditsState(key)
	if cached && state.exhausted(now) {
		return ExhaustedError(state.remaining)
	}
	if cached && !state.stale(now) {
		return nil
	}

	probed, errProbe := probeCredits(sa, key)
	if errProbe != nil {
		// Balance unknown: let the request through and rely on the upstream verdict.
		return nil
	}
	// The balance just moved from unknown/stale to fresh, so publish it.
	persistNote(sa, key)
	if probed.remaining <= 0 {
		return ExhaustedError(probed.remaining)
	}
	return nil
}

// debitCredits subtracts an observed credit consumption from the cached balance
// so the note reflects spend between billing probes.
//
// This is a local estimate layered on top of the last authoritative reading: it
// keeps the displayed balance moving in real time, and the next probe (on token
// refresh, or when the cache goes stale) reconciles it with the billing API. The
// balance is never pushed below zero, and reaching zero locally does NOT start a
// cooldown — only the billing API or an upstream quota error may do that, so a
// drifted estimate can't strand a credential that still has credits.
func debitCredits(key string, credit float64) (creditsState, bool) {
	if key == "" || credit <= 0 {
		return creditsState{}, false
	}
	// Serialize with the probe path: read-modify-write must be atomic or
	// concurrent requests debit from the same baseline and lose a deduction.
	mu := creditsProbeLock(key)
	mu.Lock()
	defer mu.Unlock()

	state, ok := loadCreditsState(key)
	if !ok || !state.known {
		// No authoritative baseline yet, so there is nothing to debit from.
		return creditsState{}, false
	}
	state.remaining -= credit
	if state.remaining < 0 {
		state.remaining = 0
	}
	state.estimated = true
	creditsByAuth.Store(key, state)
	return state, true
}

// TrackSpend records the credits a completed request consumed and refreshes
// the credential note. Best effort: never affects the request outcome.
func TrackSpend(sa *codebuddy.StoredAuth, authID string, credit float64) {
	if credit <= 0 {
		return
	}
	key := Key(authID, sa)
	if key == "" {
		return
	}
	if _, ok := debitCredits(key, credit); !ok {
		return
	}
	persistNote(sa, key)
}
