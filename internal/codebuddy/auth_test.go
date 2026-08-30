package codebuddy

import (
	"encoding/json"
	"testing"
)

func TestStoredAuthNoteRoundTrips(t *testing.T) {
	// The host merges metadata back into the auth file, so a note written by the
	// plugin must survive a marshal/parse cycle to stay visible after a restart.
	original := &StoredAuth{
		Auth: StoredTokens{AccessToken: "token-1", RefreshToken: "refresh-1"},
		Note: "剩余积分 699.01",
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseStored(raw)
	if err != nil {
		t.Fatalf("ParseStored: %v", err)
	}
	if parsed.Note != "剩余积分 699.01" {
		t.Fatalf("Note = %q, want it preserved across the round trip", parsed.Note)
	}
}
