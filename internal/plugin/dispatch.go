// Package plugin is the cliproxy RPC surface: it declares what the plugin can
// do and routes every host method to its handler.
//
// Adding a capability means adding a case here plus a flag in newRegistration's
// Capabilities.
package plugin

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/lovingfish/workbuddy-cliproxy/internal/auth"
	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/executor"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return wire.OKEnvelope(newRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return wire.OKEnvelope(pluginapi.ModelResponse{Provider: codebuddy.ProviderName, Models: staticModels()})
	case pluginabi.MethodAuthIdentifier:
		return wire.OKEnvelope(identifierResponse{Identifier: codebuddy.ProviderName})
	case pluginabi.MethodAuthParse:
		return auth.ParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return auth.StartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return auth.PollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return auth.RefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return wire.OKEnvelope(identifierResponse{Identifier: codebuddy.ProviderName})
	case pluginabi.MethodExecutorExecute:
		return executor.Execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executor.ExecuteStream(request)
	default:
		return wire.ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
