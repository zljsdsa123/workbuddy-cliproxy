// Package codebuddy is the Tencent CodeBuddy upstream: its endpoints, the
// {code,msg,data} envelope every API answers with, the request headers it
// expects, and the on-disk shape of a credential.
//
// Nothing in here knows about cliproxy — it is the provider-facing half of the
// plugin, imported by the quota, auth and executor packages.
package codebuddy

const (
	// ProviderName is the id this plugin registers under inside cliproxy.
	ProviderName = "workbuddy"
	// AuthFileName is the credential file the host persists for this provider.
	AuthFileName = "workbuddy.json"

	upstreamBase  = "https://copilot.tencent.com"
	clientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer = "https://www.codebuddy.cn"

	EndpointAuthState    = upstreamBase + "/v2/plugin/auth/state?platform=CLI"
	EndpointLoginAcct    = upstreamBase + "/v2/plugin/login/account?state="
	EndpointAuthToken    = upstreamBase + "/v2/plugin/auth/token?state="
	EndpointTokenRefresh = upstreamBase + "/v2/plugin/auth/token/refresh"
	EndpointChat         = upstreamBase + "/v2/chat/completions"
	EndpointUserResource = upstreamBase + "/v2/billing/meter/get-user-resource"

	// QuotaProductCode scopes the billing query to CodeBuddy resource packages.
	QuotaProductCode = "p_tcaca"
)
