package codebuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// StoredAuth is the on-disk shape of a workbuddy credential.
//
// Note is the human-readable credits summary surfaced on the management panel's
// credential card. The host merges auth metadata back into this file, so the
// field round-trips: it is written from metadata on save and read back here on
// parse, which keeps the balance visible across restarts until the next probe.
//
// CPA merges its host-managed metadata as top-level keys of this same document,
// so a credential routed through its own proxy carries "proxy_url" here; it is
// captured on parse so every outbound leg for the credential goes through the
// same proxy the management panel shows.
type StoredAuth struct {
	Auth     StoredTokens  `json:"auth"`
	Account  StoredAccount `json:"account"`
	Note     string        `json:"note,omitempty"`
	ProxyURL string        `json:"proxy_url,omitempty"`

	// RawDoc is the exact credential document the host handed to this process.
	// StoredAuth is only a projection of it: host-managed top-level fields
	// ("type", "disabled", "priority", …) live in the document but not in this
	// struct. Keeping the original lets persistNote rewrite just the "note"
	// field and leave everything else intact, instead of re-marshaling a bare
	// struct and wiping fields it does not model.
	RawDoc json.RawMessage `json:"-"`
}

type StoredTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type StoredAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// TokenData is the auth/token and token/refresh payload.
type TokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

// AccountData is the login/account payload.
type AccountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// AuthStateData is the auth/state payload that seeds a browser login.
type AuthStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func ParseStored(raw []byte) (*StoredAuth, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa StoredAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	sa.RawDoc = append(json.RawMessage(nil), raw...)
	return &sa, nil
}
