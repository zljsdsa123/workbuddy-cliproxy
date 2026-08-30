// Package executor forwards chat completion requests to CodeBuddy.
//
// Every path streams upstream — CodeBuddy rejects non-stream requests with code
// 11101 — so the non-streaming entry point folds the SSE stream back into a
// single chat.completion object.
package executor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/lovingfish/workbuddy-cliproxy/internal/codebuddy"
	"github.com/lovingfish/workbuddy-cliproxy/internal/hostrpc"
	"github.com/lovingfish/workbuddy-cliproxy/internal/quota"
	"github.com/lovingfish/workbuddy-cliproxy/internal/wire"
)

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func Execute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := codebuddy.ParseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if errQuota := quota.Gate(sa, req.AuthID); errQuota != nil {
		return nil, errQuota
	}
	// CodeBuddy rejects non-stream requests (code 11101), so always stream
	// upstream and fold the chunks into a single chat.completion object.
	body := rewriteSystemForUpstream(forceStreamBody(req.Payload, req.OriginalRequest))
	httpReq, err := http.NewRequest(http.MethodPost, codebuddy.EndpointChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	codebuddy.BackendHeaders(httpReq, sa)
	resp, err := codebuddy.SharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		return nil, quota.Classify(resp.StatusCode, payload, sa, req.AuthID)
	}
	completion, spent, err := aggregateCompletion(resp.Body, req.Model)
	if err != nil {
		return nil, err
	}
	quota.TrackSpend(sa, req.AuthID, spent)
	return wire.OKEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// streamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type streamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func ExecuteStream(raw []byte) ([]byte, error) {
	var req streamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := codebuddy.ParseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if errQuota := quota.Gate(sa, req.AuthID); errQuota != nil {
		return nil, errQuota
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	body = rewriteSystemForUpstream(body)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		chunks, errCollect := collectUpstreamStream(body, sa, sseFramed, req.AuthID)
		if errCollect != nil {
			return nil, errCollect
		}
		return wire.OKEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async streaming still opens the upstream connection synchronously so an
	// error status can be returned through the RPC envelope. Emitting it as a
	// stream chunk instead would hide the failure from the host: by the time a
	// chunk is emitted this call has already returned OK, so MarkResult would
	// record the request as a success and never cool the credential down.
	httpReq, err := http.NewRequest(http.MethodPost, codebuddy.EndpointChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	codebuddy.BackendHeaders(httpReq, sa)
	resp, errDo := codebuddy.SharedHTTPClient().Do(httpReq)
	if errDo != nil {
		return nil, fmt.Errorf("http_error: %w", errDo)
	}
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			return nil, fmt.Errorf("workbuddy: close upstream error body: %w", errClose)
		}
		return nil, quota.Classify(resp.StatusCode, errPayload, sa, req.AuthID)
	}

	// Status is good: hand the open body to the pump, which emits each chunk via
	// host.stream.emit so the client sees true streaming.
	go pumpUpstreamStream(resp, req.StreamID, sseFramed, sa, req.AuthID)
	return wire.OKEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads an already-open upstream SSE response in the
// background and emits each cleaned chunk to the host stream. It closes the
// stream when done. An emit failure (client disconnected → host closed the
// stream) aborts the pump so we stop reading a dead upstream.
//
// The caller has already validated the response status, so any failure here is
// mid-stream and can only be surfaced as a stream error.
func pumpUpstreamStream(resp *http.Response, streamID string, sseFramed bool, sa *codebuddy.StoredAuth, authID string) {
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			hostrpc.StreamEmitError(streamID, fmt.Sprintf("close upstream body: %v", errClose))
		}
	}()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	spent := 0.0
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned, credit, hasCredit := cleanChunkJSON(content)
		if hasCredit {
			// CodeBuddy reports a running total per chunk, so keep the largest
			// value rather than summing partial reports.
			if credit > spent {
				spent = credit
			}
		}
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := hostrpc.StreamEmit(streamID, []byte(cleaned)); err != nil {
			break
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		hostrpc.StreamEmitError(streamID, fmt.Sprintf("read upstream stream: %v", errScan))
	}
	quota.TrackSpend(sa, authID, spent)
	hostrpc.StreamClose(streamID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice.
func collectUpstreamStream(body []byte, sa *codebuddy.StoredAuth, sseFramed bool, authID string) ([]pluginapi.ExecutorStreamChunk, error) {
	httpReq, err := http.NewRequest(http.MethodPost, codebuddy.EndpointChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	codebuddy.BackendHeaders(httpReq, sa)
	resp, err := codebuddy.SharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			hostrpc.Log("warn", "workbuddy: close upstream body failed", map[string]any{"error": errClose.Error()})
		}
	}()
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		return nil, quota.Classify(resp.StatusCode, errPayload, sa, authID)
	}
	chunks, spent := aggregateSSE(resp.Body, sseFramed)
	quota.TrackSpend(sa, authID, spent)
	return chunks, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}
