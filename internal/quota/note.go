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
// Only the "note" field is meant to change: everything else in the credential
// document (host-managed "type"/"disabled", tokens, account) must round-trip
// untouched. Re-marshaling StoredAuth alone would drop those host fields and
// orphan the file (its "type" wiped, the next rescan can rebind it to another
// provider) — so persistNote patches the original document instead.
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
	storage, errMarshal := storageWithNote(sa, note)
	if errMarshal != nil {
		return
	}
	if errSave := hostrpc.AuthSave(codebuddy.AuthFileName, storage); errSave != nil {
		hostrpc.Log("debug", "workbuddy: persist credits note failed", map[string]any{"error": errSave.Error()})
		return
	}
	sa.Note = note
}

// storageWithNote renders the credential bytes to persist with an updated note.
// When the original document is available (sa.RawDoc), only the "note" top-level
// field is replaced and every other field is preserved verbatim. If no raw
// document is at hand (e.g. a struct built by hand in tests) it falls back to a
// struct snapshot.
func storageWithNote(sa *codebuddy.StoredAuth, note string) (json.RawMessage, error) {
	if len(sa.RawDoc) > 0 {
		var doc map[string]any
		if err := json.Unmarshal(sa.RawDoc, &doc); err == nil {
			if doc == nil {
				doc = make(map[string]any)
			}
			doc["note"] = note
			return json.Marshal(doc)
		}
	}
	snapshot := *sa
	snapshot.Note = note
	return json.Marshal(&snapshot)
}
