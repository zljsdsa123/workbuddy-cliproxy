// Package auth implements the workbuddy auth provider: parsing a stored
// credential, the CodeBuddy browser login flow, and token refresh.
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/quota"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

const loginTTL = 5 * time.Minute

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	client  *http.Client
	expires time.Time
}

var loginStates sync.Map // state(string) -> *loginCtx

func ParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := codebuddy.ParseStored(req.RawJSON)
	if err != nil {
		// Not a workbuddy credential; let the host try other providers.
		return wire.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return wire.OKEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    ToAuthData(sa),
	})
}

func ToAuthData(sa *codebuddy.StoredAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	metadata := map[string]any{"type": codebuddy.ProviderName}
	if note := strings.TrimSpace(sa.Note); note != "" {
		metadata["note"] = note
	}
	return pluginapi.AuthData{
		Provider:    codebuddy.ProviderName,
		ID:          codebuddy.ProviderName,
		FileName:    codebuddy.AuthFileName,
		Label:       "WorkBuddy",
		StorageJSON: storage,
		Metadata:    metadata,
	}
}

func StartLogin(raw []byte) ([]byte, error) {
	client := codebuddy.NewLoginClient()
	data, _, err := codebuddy.DoJSON(client, http.MethodPost, codebuddy.EndpointAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st codebuddy.AuthStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	loginStates.Store(st.State, &loginCtx{client: client, expires: time.Now().Add(loginTTL)})
	return wire.OKEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  codebuddy.ProviderName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
	})
}

func PollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns code 11217 ("login ing") while pending, and code 0 with the
	// token bundle once complete. login/account sits behind the openresty gateway
	// and is rejected (401) until login finishes, so probe token first and only
	// fetch account once we hold a bearer.
	tokRaw, _, errTok := codebuddy.DoJSON(lc.client, http.MethodGet, codebuddy.EndpointAuthToken+state, nil, nil)
	if errTok != nil {
		return wire.OKEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok codebuddy.TokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return wire.OKEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	var acct codebuddy.AccountData
	acctHeaders := func(r *http.Request) {
		codebuddy.CommonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := codebuddy.DoJSON(lc.client, http.MethodGet, codebuddy.EndpointLoginAcct+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	sa := &codebuddy.StoredAuth{
		Auth: codebuddy.StoredTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       tok.Domain,
		},
		Account: codebuddy.StoredAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	loginStates.Delete(state)
	// Seed the credits note so the credential card shows a balance right away
	// instead of staying blank until the first refresh cycle.
	quota.RefreshNote(sa, "")
	return wire.OKEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   ToAuthData(sa),
	})
}

func RefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := codebuddy.ParseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	headers := func(r *http.Request) {
		codebuddy.CommonHeaders(r)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", codebuddy.ProviderName)
	}
	data, status, err := codebuddy.DoJSON(codebuddy.SharedHTTPClient(), http.MethodPost, codebuddy.EndpointTokenRefresh, headers, nil)
	if err != nil {
		if status >= 400 {
			return nil, wire.NewUpstreamError(status, "", fmt.Sprintf("refresh rejected (HTTP %d)", status))
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	var tok codebuddy.TokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	// A token refresh is the natural place to re-read the balance: the refreshed
	// credential is persisted anyway, so the note rides along at no extra cost.
	quota.RefreshNote(sa, req.AuthID)
	return wire.OKEnvelope(pluginapi.AuthRefreshResponse{Auth: ToAuthData(sa)})
}
