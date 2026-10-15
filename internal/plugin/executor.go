// Non-stream and stream execution paths (FR-005/FR-006/FR-007, arch §4/§5
// steps 5-8): resolve the public model ID against the catalog snapshot,
// translate the inbound payload to the record's upstream protocol, call
// upstream through the host HTTP callbacks, and translate back to the
// client protocol. Request authentication is selected by CPA.

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/gemini"
	"opencode-go-cliproxyapi/internal/adapter/messages"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/key resolution succeeded.
type resolvedExecution struct {
	cfg         config.Config
	rec         catalog.ModelRecord
	key         string
	tools       shared.ResponseTools
	original    []byte
	upstream    []byte
	model       string
	clientModel string
	callbackID  string
}

// Payload contains CPA's effective request, including request rewrites. The
// original body is retained separately by CPA for response/session metadata.
func executorBody(req executorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

func executorResponseFormat(req executorRequest) string {
	if req.Format != "" {
		return req.Format
	}
	return req.SourceFormat
}

func prepareUpstreamRequest(res *resolvedExecution, req executorRequest) ([]byte, *errclass.Error) {
	body := executorBody(req)
	res.original, res.model = req.OriginalRequest, res.rec.UpstreamID
	if len(res.original) == 0 {
		res.original = body
	}
	if res.rec.Protocol == catalog.RouteGemini && res.rec.EndpointPath != "" {
		// A model/endpoint override controls the actual upstream model. Keep
		// the body model and translation metadata aligned with that path.
		res.model = strings.TrimPrefix(res.rec.EndpointPath, "/v1/models/")
		res.model = strings.TrimSuffix(strings.TrimSuffix(res.model, ":generateContent"), ":streamGenerateContent")
	}
	var out []byte
	var e *errclass.Error
	if res.rec.Protocol == catalog.RouteGemini {
		out, e = gemini.BuildRequestWithContext(context.Background(), res.model, req.SourceFormat, body, req.Stream, res.rec.Thinking, &res.tools)
	} else if req.SourceFormat == "gemini" {
		out, e = buildGeminiClientRequest(res.rec.Protocol, res.rec.UpstreamID, body, req.Stream, res.rec.Thinking)
	} else {
		out, e = buildUpstreamRequest(res.rec.Protocol, res.model, req.SourceFormat, body, res.rec.Thinking, &res.tools)
	}
	res.upstream = out
	return out, e
}

func executionURL(res *resolvedExecution, stream bool) (string, *errclass.Error) {
	u, err := url.Parse(catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath))
	if err != nil {
		return "", errclass.Translation("invalid upstream URL")
	}
	if res.rec.Protocol == catalog.RouteGemini {
		for _, suffix := range []string{":generateContent", ":streamGenerateContent"} {
			u.Path = strings.TrimSuffix(u.Path, suffix)
		}
		if stream {
			u.Path += ":streamGenerateContent"
			q := u.Query()
			q.Set("alt", "sse")
			u.RawQuery = q.Encode()
		} else {
			u.Path += ":generateContent"
		}
	}
	return u.String(), nil
}

func convertExecutionResponse(res *resolvedExecution, format string, status int, body []byte) ([]byte, *errclass.Error) {
	if res.rec.Protocol == catalog.RouteGemini {
		return gemini.ConvertNonStreamResponseWithContext(format, status, body, res.model, res.original, res.upstream)
	}
	if format == "gemini" {
		return convertGeminiClientResponse(res.rec.Protocol, res.model, res.original, res.upstream, body)
	}
	return convertNonStream(res.rec.Protocol, format, status, body, &res.tools)
}

func executionStreamConverter(res *resolvedExecution, format string) streamConverter {
	if res.rec.Protocol == catalog.RouteGemini {
		return gemini.NewStreamConverterWithContext(format, res.model, res.original, res.upstream)
	}
	if format == "gemini" {
		return newGeminiClientStreamConverter(res.rec.Protocol, res.model, res.original, res.upstream)
	}
	return newStreamConverter(res.rec.Protocol, format, &res.tools)
}

// resolveExecution resolves the requested model against the snapshot and
// extracts the key selected by CPA. A non-nil second return is a ready-made
// failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not opencode-go"})
	}
	key, source, keyErr := selectedCredentialAPIKey(req)
	if keyErr != nil {
		message := keyErr.Error()
		if errors.Is(keyErr, errSelectedCredentialNoAPIKey) {
			message = "selected auth has no api key"
		}
		debugTrace("executor auth model=%s auth_id=%s provider=%s key_source=%s key_error=%v", req.Model, req.AuthID, req.AuthProvider, source, keyErr)
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: message})
	}
	debugTrace("executor auth model=%s auth_id=%s provider=%s key_source=%s attr_count=%d storage_json_bytes=%d", req.Model, req.AuthID, req.AuthProvider, source, len(req.AuthAttributes), len(req.StorageJSON))
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}
	return &resolvedExecution{cfg: cfg, rec: rec, key: key, tools: *shared.NewResponseTools(), clientModel: req.Model, callbackID: req.HostCallbackID}, nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "non-stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := prepareUpstreamRequest(res, req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url, eErr := executionURL(res, false)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s key_count=%d", req.Model, res.rec.UpstreamID, res.rec.Protocol, url, len(res.cfg.APIKeys))
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.DoWithCallback(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	}, res.callbackID)
	if err != nil {
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, resp.Body)), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertExecutionResponse(res, executorResponseFormat(req), resp.StatusCode, resp.Body)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: clientResponseModel(res, executorResponseFormat(req), converted), Headers: resp.Headers}), nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	case catalog.RouteGemini:
		return gemini.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts, tools...)
	}
	return nil, errclass.Translation("unsupported route")
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	h.Set("x-opencode-session", sessionID)
	h.Set("Content-Type", "application/json")
	return h
}

const emptyOpenCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// An explicit OpenCode session must beat CPA's derived identity: otherwise
// independent conversations with identical first turns can collapse together.
func resolveOpenCodeSessionID(req executorRequest) (string, *errclass.Error) {
	if sid := req.Headers.Get("X-Opencode-Session"); sid != "" {
		return sid, nil
	}
	if sid, ok := req.Metadata["canonical_session_id"].(string); ok && sid != "" {
		return sid, nil
	}
	for _, name := range []string{
		"X-Session-Affinity",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
	} {
		if sid := req.Headers.Get(name); sid != "" {
			return sid, nil
		}
	}
	return deriveOpenCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// deriveOpenCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveOpenCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					text, eErr := shared.ToolResultText(block.Result, "tool messages carry text only")
					if eErr != nil {
						return "", eErr
					}
					content.WriteString(text)
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	case "gemini":
		var body struct {
			Contents []struct {
				Role  string                       `json:"role"`
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(originalRequest, &body); err != nil {
			return "", errclass.Translation("malformed gemini request JSON")
		}
		started := false
		for _, turn := range body.Contents {
			if turn.Role != "user" && turn.Role != "" {
				if started {
					break
				}
				continue
			}
			started = true
			for _, part := range turn.Parts {
				if raw, ok := part["text"]; ok {
					var value string
					if json.Unmarshal(raw, &value) != nil {
						return "", errclass.Translation("invalid gemini text part")
					}
					content.WriteString(value)
				}
				for _, key := range []string{"inlineData", "fileData"} {
					if raw, ok := part[key]; ok {
						content.Write(raw)
					}
				}
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "OpenCode Go session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	case catalog.RouteGemini:
		return gemini.ConvertNonStreamResponse(sourceFormat, status, body, tools...)
	}
	return nil, errclass.Translation("unsupported route")
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamConverter is the common shape of the three adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string, tools ...*shared.ResponseTools) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat, tools...)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat, tools...)
	}
	return chatcompletions.NewStreamConverter(sourceFormat, tools...)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := prepareUpstreamRequest(res, req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url, eErr := executionURL(res, true)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	st, _, id, err := m.bridge.DoStreamWithCallback(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	}, res.callbackID)
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		var body []byte
		if id != "" {
			watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
				_ = m.bridge.StreamClose(id)
			})
			for int64(len(body)) < res.cfg.MaxResponseBytes {
				chunk, readError, closed, readErr := m.bridge.StreamRead(id)
				remaining := res.cfg.MaxResponseBytes - int64(len(body))
				if int64(len(chunk)) > remaining {
					chunk = chunk[:int(remaining)]
				}
				body = append(body, chunk...)
				if closed || readErr != nil || readError != "" || json.Valid(body) {
					break
				}
			}
			watchdog.Stop()
			_ = m.bridge.StreamClose(id)
		}
		return classEnvelope(shared.UpstreamStatusError(st, body)), nil
	}

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(downID, id, res, executorResponseFormat(req))
	}()
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution, sourceFormat string) {
	var closeOnce sync.Once
	closeStreams := func(downErrMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			_ = m.bridge.StreamCloseDownstream(downID, downErrMsg)
		})
	}
	defer closeStreams("")

	var aborted atomic.Bool
	watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				_ = m.bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		aborted.Store(true)
		_ = m.bridge.StreamClose(upstreamID)
	})
	defer watchdog.Stop()

	conv := executionStreamConverter(res, sourceFormat)
	emit := func(events [][]byte) error {
		for i, event := range events {
			events[i] = clientResponseModel(res, sourceFormat, event)
		}
		return m.emitAll(downID, events)
	}
	var (
		total          int64
		upstreamClosed bool
		convDone       bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		if aborted.Load() {
			closeStreams(errclass.Redact("stream exceeded request-timeout"))
			return
		}
		if err != nil {
			closeStreams(errclass.Redact(err.Error()))
			return
		}
		if readErrMsg != "" {
			closeStreams(errclass.Redact(readErrMsg))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams(errclass.Redact("stream exceeded max-response-bytes"))
			return
		}
		events, done, convErr := conv.Feed(payload)
		if convErr != nil {
			closeStreams(errclass.Redact(convErr.Message))
			return
		}
		if emitErr := emit(events); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
		convDone = done
		if convDone || upstreamClosed {
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t", convDone, upstreamClosed)
	// Finalize even when Feed saw a sentinel: Gemini's finisher validates
	// completion and guards duplicate terminal events itself.
	if finisher, ok := conv.(interface {
		Finish() ([][]byte, *errclass.Error)
	}); ok {
		flushed, finishErr := finisher.Finish()
		if finishErr != nil {
			closeStreams(errclass.Redact(finishErr.Message))
			return
		}
		if emitErr := emit(flushed); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
	} else if !convDone && upstreamClosed {
		if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
			flushed := flusher.Flush()
			if emitErr := emit(flushed); emitErr != nil {
				closeStreams(errclass.Redact(emitErr.Error()))
				return
			}
		}
	}
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
