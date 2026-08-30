// Package wire holds the JSON shapes that cross the plugin↔host boundary: the
// response envelope every plugin RPC returns, and the error type that carries an
// upstream HTTP status through it.
package wire

import (
	"encoding/json"
	"errors"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError mirrors the host's pluginabi.Error. HTTPStatus is the only
// channel the host uses to recover an upstream status code from a plugin
// failure (see decodeEnvelopeResult in internal/pluginhost/rpc_client.go), and
// that status is what drives quota cooldown and credential rotation.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func OKEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: result})
}

func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message}})
	return raw
}

// StatusEnvelope renders a plugin error that carries an upstream HTTP status so
// the host can classify it (429 → quota cooldown, 401 → refresh, ...).
func StatusEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{
		Code:       code,
		Message:    message,
		Retryable:  retryable,
		HTTPStatus: status,
	}})
	return raw
}

// ErrorEnvelopeFor renders err as a plugin error envelope, preserving the
// upstream status when err carries one.
func ErrorEnvelopeFor(err error) []byte {
	var upstream *UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		return StatusEnvelope(upstream.Code, upstream.Error(), upstream.Status, upstream.Retryable)
	}
	return ErrorEnvelope("plugin_error", err.Error())
}
