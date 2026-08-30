package auth

import (
	"testing"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
)

func TestToAuthDataExposesNote(t *testing.T) {
	data := ToAuthData(&codebuddy.StoredAuth{
		Auth: codebuddy.StoredTokens{AccessToken: "token-1"},
		Note: "剩余积分 500.00",
	})

	if got := data.Metadata["note"]; got != "剩余积分 500.00" {
		t.Fatalf("metadata[note] = %v, want the credits summary (this is what the card renders)", got)
	}
	if got := data.Metadata["type"]; got != codebuddy.ProviderName {
		t.Fatalf("metadata[type] = %v, want %q", got, codebuddy.ProviderName)
	}
}

func TestToAuthDataOmitsEmptyNote(t *testing.T) {
	data := ToAuthData(&codebuddy.StoredAuth{Auth: codebuddy.StoredTokens{AccessToken: "token-1"}})
	if _, ok := data.Metadata["note"]; ok {
		t.Fatal("an empty note must not be published as metadata")
	}
}
