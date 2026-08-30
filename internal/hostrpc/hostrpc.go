// Package hostrpc bridges plugin code to the cliproxy host's RPC surface.
//
// The transport itself cannot live here: reaching the host means calling through
// the C function-pointer table handed to the plugin at init, which is cgo
// territory and therefore confined to package main. So main injects its
// cgo-backed transport once at startup and everything else talks to the host
// through the typed helpers below, without knowing cgo exists.
package hostrpc

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Transport sends one host RPC and returns the raw response bytes.
type Transport func(method string, request []byte) ([]byte, error)

// transport is written once by cliproxy_plugin_init, before the host is able to
// issue any RPC, and only read afterwards — the same guarantee the raw host API
// pointer carried when it was a package-level global in main.
var transport Transport

// SetTransport installs the cgo-backed host transport. Called from
// cliproxy_plugin_init while the host table is still being negotiated.
func SetTransport(fn Transport) { transport = fn }

// Call invokes a host RPC method through the injected transport.
func Call(method string, request []byte) ([]byte, error) {
	if transport == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	return transport(method, request)
}

// StreamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func StreamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := Call(pluginabi.MethodHostStreamEmit, body)
	return err
}

func StreamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = StreamEmit(streamID, errJSON)
}

func StreamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = Call(pluginabi.MethodHostStreamClose, body)
}

// Log forwards a structured message to the host logger. Best effort: a logging
// failure must never affect request handling.
func Log(level, message string, fields map[string]any) {
	payload := map[string]any{"level": level, "message": message}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return
	}
	_, _ = Call(pluginabi.MethodHostLog, body)
}

// AuthSave writes a credential file through the host, which also upserts the
// in-memory record so the management panel reflects the change without waiting
// for the next refresh cycle.
func AuthSave(name string, storage json.RawMessage) error {
	body, errBody := json.Marshal(map[string]any{"name": name, "json": storage})
	if errBody != nil {
		return errBody
	}
	_, errSave := Call(pluginabi.MethodHostAuthSave, body)
	return errSave
}
