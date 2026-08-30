package quota

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/hostrpc"
)

// creditsNote renders the credential note shown on the management panel's
// credential card. CodeBuddy meters prepaid credits, so the remaining balance
// plus the local cooldown state is the useful summary. The text is Chinese to
// match the CodeBuddy console and this plugin's user-facing docs.
func creditsNote(state creditsState, now time.Time) string {
	if !state.known {
		return ""
	}
	amount := fmt.Sprintf("%.2f", state.remaining)
	if state.estimated {
		// "~" marks a balance carried forward by local debits rather than one
		// read straight from the billing API.
		amount = "~" + amount
	}
	if state.exhausted(now) {
		return fmt.Sprintf("剩余积分 %s · 冷却中", amount)
	}
	return fmt.Sprintf("剩余积分 %s", amount)
}

// RefreshNote probes the balance and writes the resulting summary onto
// sa.Note. A probe failure leaves the previous note untouched: a stale balance
// is more useful on the card than an empty field.
func RefreshNote(sa *codebuddy.StoredAuth, authID string) {
	if sa == nil {
		return
	}
	key := Key(authID, sa)
	if key == "" {
		return
	}
	state, errProbe := probeCredits(sa, key)
	if errProbe != nil {
		hostrpc.Log("debug", "workbuddy: credits probe failed", map[string]any{"error": errProbe.Error()})
		return
	}
	if note := creditsNote(state, time.Now()); note != "" {
		sa.Note = note
	}
}

// persistNote writes the current credits summary straight to the auth
// file via host.auth.save, which also upserts the in-memory record so the
// credential card reflects it without waiting for the next refresh cycle.
//
// Best effort: the note is a display detail and must never fail a request.
func persistNote(sa *codebuddy.StoredAuth, key string) {
	if sa == nil || key == "" {
		return
	}
	state, ok := loadCreditsState(key)
	if !ok {
		return
	}
	note := creditsNote(state, time.Now())
	if note == "" || note == strings.TrimSpace(sa.Note) {
		return
	}
	snapshot := *sa
	snapshot.Note = note
	storage, errMarshal := json.Marshal(&snapshot)
	if errMarshal != nil {
		return
	}
	if errSave := hostrpc.AuthSave(codebuddy.AuthFileName, storage); errSave != nil {
		hostrpc.Log("debug", "workbuddy: persist credits note failed", map[string]any{"error": errSave.Error()})
		return
	}
	sa.Note = note
}
