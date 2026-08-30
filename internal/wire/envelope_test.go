package wire

import (
	"encoding/json"
	"testing"
)

func TestErrorEnvelopeForPlainError(t *testing.T) {
	raw := ErrorEnvelopeFor(errPlain{})

	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if decoded.Error == nil || decoded.Error.HTTPStatus != 0 {
		t.Fatalf("a plain error must not fabricate an HTTP status, got %+v", decoded.Error)
	}
	if decoded.Error.Code != "plugin_error" {
		t.Fatalf("code = %q, want plugin_error", decoded.Error.Code)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "boom" }
