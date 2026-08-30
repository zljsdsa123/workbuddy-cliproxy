package hostrpc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// stubTransport installs a recording transport for the duration of one test and
// restores the previous one, since transport is package state.
func stubTransport(t *testing.T, fn Transport) *[]string {
	t.Helper()
	var seen []string
	prev := transport
	t.Cleanup(func() { transport = prev })
	transport = func(method string, request []byte) ([]byte, error) {
		seen = append(seen, method+" "+string(request))
		return fn(method, request)
	}
	return &seen
}

func TestCallWithoutTransportFails(t *testing.T) {
	prev := transport
	t.Cleanup(func() { transport = prev })
	transport = nil

	// The host table is only installed at cliproxy_plugin_init; anything running
	// before that must get an error rather than dereference a nil function.
	if _, err := Call("host.log", []byte("{}")); err == nil {
		t.Fatal("expected an error when no transport is installed")
	}
}

func TestSetTransportRoutesCall(t *testing.T) {
	seen := stubTransport(t, func(method string, request []byte) ([]byte, error) {
		return []byte(`{"ok":true}`), nil
	})

	out, err := Call("host.log", []byte(`{"level":"debug"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("Call returned %q, want the transport's response", out)
	}
	if len(*seen) != 1 || (*seen)[0] != `host.log {"level":"debug"}` {
		t.Fatalf("transport saw %v, want the method and request passed through verbatim", *seen)
	}
}

func TestStreamEmitRequiresStreamID(t *testing.T) {
	seen := stubTransport(t, func(method string, request []byte) ([]byte, error) {
		return nil, nil
	})

	if err := StreamEmit("", []byte("chunk")); err == nil {
		t.Fatal("expected an error for an empty stream id")
	}
	// StreamEmitError/StreamClose are best effort and must stay silent rather
	// than emit a malformed host call when there is no stream to talk to.
	StreamEmitError("", "boom")
	StreamClose("")
	if len(*seen) != 0 {
		t.Fatalf("transport saw %v, want no host call without a stream id", *seen)
	}
}

func TestStreamEmitPropagatesHostRejection(t *testing.T) {
	stubTransport(t, func(method string, request []byte) ([]byte, error) {
		return nil, fmt.Errorf("stream closed")
	})

	// The pump relies on this error to stop reading an upstream whose client has
	// already disconnected.
	if err := StreamEmit("s-1", []byte("chunk")); err == nil {
		t.Fatal("expected the host rejection to reach the caller")
	}
}

func TestAuthSaveSendsRawJSON(t *testing.T) {
	var body []byte
	stubTransport(t, func(method string, request []byte) ([]byte, error) {
		body = request
		return nil, nil
	})

	if err := AuthSave("workbuddy.json", json.RawMessage(`{"note":"剩余积分 12.00"}`)); err != nil {
		t.Fatalf("AuthSave: %v", err)
	}

	// The credential must be embedded as raw JSON. A plain []byte field would be
	// base64-encoded here and the host would persist an unparsable auth file.
	if !strings.Contains(string(body), `"json":{"note":"剩余积分 12.00"}`) {
		t.Fatalf("AuthSave body = %s, want the credential inlined as raw JSON", body)
	}
	var decoded struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if decoded.Name != "workbuddy.json" {
		t.Fatalf("name = %q, want workbuddy.json", decoded.Name)
	}
}

func TestAuthSavePropagatesFailure(t *testing.T) {
	stubTransport(t, func(method string, request []byte) ([]byte, error) {
		return nil, fmt.Errorf("host refused")
	})

	// quota's persistNote logs on failure and keeps the old note, so the error has
	// to make it back out.
	if err := AuthSave("workbuddy.json", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the host failure to reach the caller")
	}
}
