// WP-1 of PLUGIN_ONLY_499_REFACTOR_PLAN.md: the client gate and terminal
// buffer that defer the single successful Responses terminal to the CPA EOF
// flush, so a downstream close right after response.completed cannot race the
// usage record (the HTTP 499 / context canceled false negative).
//
// Scope is deliberately narrow: only the verified Codex HTTP SSE path whose
// upstream is Chat Completions. Every other client, source format, upstream
// protocol, and WebSocket upgrade keeps the existing framed terminal.
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/way326/opencode-go-omni/internal/catalog"
)

// responsesChatSourceFormat is the downstream format of a Codex Responses
// request ("Codex HTTP Responses"). The Chat Completions upstream is
// enforced by the caller passing catalog.RouteChatCompletions.
const responsesChatSourceFormat = "openai-response"

// codexSessionSources lists the only client identities accepted in the first
// release, keyed by normalizeCodexProduct: the Codex Desktop product observed
// live in CPA headers ("Originator: Codex Desktop") and the codex_cli_rs
// client. Every other client keeps the existing framed terminal. Matching
// normalizes case and internal whitespace and compares whole products;
// substring matching is deliberately avoided.
var codexSessionSources = map[string]struct{}{
	"codex desktop": {},
	"codex_cli_rs":  {},
}

// shouldDeferResponsesTerminal reports whether this executor request may use
// responsesTerminalBuffer. All of the following must hold:
//
//   - the upstream route is Chat Completions (the verified conversion path);
//   - the downstream format is Responses ("openai-response");
//   - the request is streaming;
//   - the transport is HTTP SSE, not a WebSocket upgrade;
//   - the client is a recognized Codex session source.
//
// Anything else returns false and keeps the existing terminal framing.
func shouldDeferResponsesTerminal(req executorRequest, route catalog.Route) bool {
	if route != catalog.RouteChatCompletions {
		return false
	}
	if !req.Stream {
		return false
	}
	// Require the inbound source format itself to be Responses. A request
	// whose SourceFormat and Format disagree (for example SourceFormat=openai
	// with Format=openai-response) is conservatively left untouched.
	if req.SourceFormat != responsesChatSourceFormat {
		return false
	}
	if format := strings.TrimSpace(req.Format); format != "" && format != responsesChatSourceFormat {
		return false
	}
	if responsesUpgradesToWebSocket(req.Headers) {
		return false
	}
	return codexResponsesClient(req.Headers)
}

func responsesUpgradesToWebSocket(header http.Header) bool {
	if header == nil {
		return false
	}
	return headerValuesContainToken(header.Values("Connection"), "upgrade") ||
		headerValuesContainToken(header.Values("Upgrade"), "websocket")
}

func headerValuesContainToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// codexResponsesClient recognizes Codex clients by any exact originator
// token, or by a User-Agent whose leading product token is a Codex session
// source. Identity signals only select the deferral; they are not an
// authentication boundary, so a recognized Codex originator and a different
// recognized Codex User-Agent (desktop app + CLI transport) are not treated
// as a conflict.
func codexResponsesClient(header http.Header) bool {
	if header == nil {
		return false
	}
	for _, value := range header.Values("Originator") {
		if codexSessionSource(strings.TrimSpace(value)) {
			return true
		}
	}
	_, ok := codexUserAgentSource(header.Get("User-Agent"))
	return ok
}

func codexSessionSource(token string) bool {
	_, ok := codexSessionSources[normalizeCodexProduct(token)]
	return ok
}

// normalizeCodexProduct lowercases and collapses internal whitespace so the
// desktop display product ("Codex Desktop") and the lowercase session_source
// tokens compare exactly, without admitting prefix or substring matches.
func normalizeCodexProduct(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

// codexUserAgentSource parses "<product>/<version>" where the product itself
// may contain spaces, and the version is the following non-space run. Real
// observed shapes are accepted:
//
//	Codex Desktop/0.162.0-alpha.17.2 (Windows 10.0.19045; x86_64) unknown (...)
//	codex_cli_rs/0.162.0-alpha.17.2 (Windows 11; x86_64)
//
// Trailing products and comments are ignored. The product must be a known
// Codex product and the version must be a non-empty single segment.
func codexUserAgentSource(userAgent string) (string, bool) {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return "", false
	}
	slash := strings.IndexByte(userAgent, '/')
	if slash <= 0 {
		return "", false
	}
	product := normalizeCodexProduct(userAgent[:slash])
	if !codexSessionSource(product) {
		return "", false
	}
	version := userAgent[slash+1:]
	if cut := strings.IndexAny(version, " \t"); cut >= 0 {
		version = version[:cut]
	}
	if version == "" || strings.ContainsRune(version, '/') {
		return "", false
	}
	return product, true
}

// responsesTerminalState tracks the single decision a buffered stream may
// make: store exactly one successful terminal, or latch a diagnostic failure.
type responsesTerminalState uint8

const (
	responsesTerminalPending responsesTerminalState = iota
	responsesTerminalStored
	responsesTerminalForwarded
	responsesTerminalPoisoned
)

// responsesTerminalBuffer defers the one successful Responses terminal of a
// Codex Chat->Responses stream so the caller can emit it as a data-only
// chunk at stream EOF. Contract:
//
//   - accept(event) returns (event, nil) for every ordinary chunk, which
//     must be forwarded unchanged;
//   - accept returns (nil, nil) exactly once, for the clean successful
//     response.completed terminal (status=completed, no error), which is
//     held back for the EOF commit;
//   - a legal non-success terminal is forwarded byte-identical in its
//     original framing: response.completed with response.status=incomplete
//     (the Chat finish_reason:length truncation) as well as response.failed
//     and response.incomplete. That terminal is the last chunk; any later
//     chunk is an error;
//   - any later chunk, duplicate terminal, missing or unrecognized status,
//     non-null error on a completed terminal, malformed terminal claim, or
//     non-canonical chunk returns (nil, error) and latches the failure;
//   - terminal() returns "data: <exact JSON>" with no event line and no
//     trailing newline while a clean success is held, (nil, nil) when a
//     non-success terminal was already forwarded in its original framing,
//     and an error for every other state.
//
// accept and terminal are single-goroutine; the pump calls them from one
// goroutine only.
type responsesTerminalBuffer struct {
	state   responsesTerminalState
	payload []byte
	reason  string
}

// accept classifies one converted SSE chunk. See the type contract.
func (b *responsesTerminalBuffer) accept(event []byte) ([]byte, error) {
	switch b.state {
	case responsesTerminalStored:
		b.poison("responses terminal already buffered; trailing chunk received")
		return nil, b.failureError()
	case responsesTerminalForwarded:
		b.poison("chunk received after a forwarded non-success terminal")
		return nil, b.failureError()
	case responsesTerminalPoisoned:
		return nil, b.failureError()
	}
	chunk := splitResponsesSSEChunk(event)
	if !chunk.wellFormed {
		// The selected path only carries canonical single-frame chunks.
		// Anything else (mixed frames, comments, CRLF, multi-line data, empty
		// chunks) is refused instead of forwarded, so a terminal embedded in
		// a mixed chunk cannot escape ahead of the EOF commit.
		if claim := scanResponsesTerminalClaim(event); claim != "" {
			b.poison(fmt.Sprintf("non-canonical Responses chunk carrying %q", claim))
			return nil, b.failureError()
		}
		b.poison("unexpected non-canonical Responses chunk")
		return nil, b.failureError()
	}
	if !chunk.hasData {
		if isResponsesTerminalType(chunk.name) {
			b.poison(fmt.Sprintf("Responses terminal %q without a data payload", chunk.name))
			return nil, b.failureError()
		}
		return event, nil
	}
	probe, ok := decodeResponsesTerminalProbe([]byte(chunk.data))
	if !ok {
		if chunk.claimsResponsesTerminal() {
			b.poison(fmt.Sprintf("Responses terminal %q with undecodable payload", chunk.terminalName()))
			return nil, b.failureError()
		}
		return event, nil
	}
	if isResponsesTerminalType(chunk.name) || isResponsesTerminalType(probe.Type) {
		if chunk.name != "" && chunk.name != probe.Type {
			b.poison(fmt.Sprintf("Responses terminal framing mismatch (event=%q type=%q)", chunk.name, probe.Type))
			return nil, b.failureError()
		}
	}
	switch probe.Type {
	case "response.completed":
		switch probe.Response.Status {
		case "completed":
			if problem := responsesCompletedErrorProblem(probe); problem != "" {
				b.poison("invalid successful terminal: " + problem)
				return nil, b.failureError()
			}
			b.state = responsesTerminalStored
			b.payload = append([]byte(nil), chunk.data...)
			return nil, nil
		case "incomplete":
			// Legal truncation (Chat finish_reason:length). Forward the
			// original framed terminal unchanged and never claim success.
			b.state = responsesTerminalForwarded
			b.reason = "non-success terminal was forwarded in its original framing"
			return event, nil
		default:
			if probe.Response.Status == "" {
				b.poison("invalid terminal: response.status is missing")
			} else {
				b.poison(fmt.Sprintf("invalid terminal: response.status=%q", probe.Response.Status))
			}
			return nil, b.failureError()
		}
	case "response.failed", "response.incomplete":
		// Keep the genuine failure terminal byte-identical; only the clean
		// successful terminal is deferred.
		b.state = responsesTerminalForwarded
		b.reason = fmt.Sprintf("%s terminal was forwarded in its original framing", probe.Type)
		return event, nil
	default:
		return event, nil
	}
}

// terminal returns the deferred data-only terminal, (nil, nil) when a
// non-success terminal was already forwarded unchanged, or the latched error.
func (b *responsesTerminalBuffer) terminal() ([]byte, error) {
	switch b.state {
	case responsesTerminalStored:
		out := make([]byte, 0, len("data: ")+len(b.payload))
		out = append(out, "data: "...)
		out = append(out, b.payload...)
		return out, nil
	case responsesTerminalForwarded:
		return nil, nil
	default:
		return nil, b.failureError()
	}
}

func (b *responsesTerminalBuffer) poison(reason string) {
	b.state = responsesTerminalPoisoned
	b.payload = nil
	b.reason = reason
}

func (b *responsesTerminalBuffer) failureError() error {
	reason := b.reason
	if reason == "" {
		reason = "no successful response.completed terminal was buffered for EOF delivery"
	}
	return fmt.Errorf("responses terminal: %s", reason)
}

// responsesTerminalProbe is the minimal projection used to classify a
// terminal; the payload itself is never rebuilt.
type responsesTerminalProbe struct {
	Type     string          `json:"type"`
	Error    json.RawMessage `json:"error"`
	Response struct {
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	} `json:"response"`
}

func decodeResponsesTerminalProbe(data []byte) (responsesTerminalProbe, bool) {
	var probe responsesTerminalProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return responsesTerminalProbe{}, false
	}
	return probe, true
}

// responsesCompletedErrorProblem returns "" only when a status=completed
// terminal carries no error; an attached error disqualifies the frame.
func responsesCompletedErrorProblem(probe responsesTerminalProbe) string {
	if !jsonNullOrAbsent(probe.Error) {
		return "top-level error field is set"
	}
	if !jsonNullOrAbsent(probe.Response.Error) {
		return "response.error field is set"
	}
	return ""
}

func jsonNullOrAbsent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

func isResponsesTerminalType(name string) bool {
	switch name {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

// responsesSSEChunk is one parsed SSE frame. wellFormed is true only for the
// exact shape this plugin emits: an optional single "event:" line, an
// optional single "data:" line, and one trailing blank line.
type responsesSSEChunk struct {
	name       string
	nameSeen   bool
	data       string
	hasData    bool
	dataLines  int
	wellFormed bool
}

func (c responsesSSEChunk) terminalName() string {
	if c.name != "" {
		return c.name
	}
	if probe, ok := decodeResponsesTerminalProbe([]byte(c.data)); ok {
		return probe.Type
	}
	return ""
}

// claimsResponsesTerminal reports whether a frame that is not in the exact
// expected shape still names a Responses terminal, by event line or payload
// type. Such frames are refused rather than forwarded.
func (c responsesSSEChunk) claimsResponsesTerminal() bool {
	if isResponsesTerminalType(c.name) {
		return true
	}
	if !c.hasData {
		return false
	}
	if probe, ok := decodeResponsesTerminalProbe([]byte(c.data)); ok {
		return isResponsesTerminalType(probe.Type)
	}
	return false
}

func splitResponsesSSEChunk(raw []byte) responsesSSEChunk {
	chunk := responsesSSEChunk{wellFormed: true}
	if len(raw) == 0 || !bytes.HasSuffix(raw, []byte("\n\n")) {
		chunk.wellFormed = false
		if len(raw) == 0 {
			return chunk
		}
	}
	body := bytes.TrimSuffix(raw, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\n"))
	if len(bytes.TrimSpace(body)) == 0 {
		chunk.wellFormed = false
		return chunk
	}
	seenBlank := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasSuffix(line, "\r") {
			chunk.wellFormed = false
			line = strings.TrimSuffix(line, "\r")
		}
		if line == "" {
			seenBlank = true
			continue
		}
		if seenBlank {
			chunk.wellFormed = false
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			if chunk.nameSeen {
				chunk.wellFormed = false
				continue
			}
			chunk.nameSeen = true
			chunk.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if chunk.name == "" {
				chunk.wellFormed = false
			}
		case strings.HasPrefix(line, "data:"):
			chunk.dataLines++
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if chunk.dataLines == 1 {
				chunk.data = value
			} else {
				chunk.data += "\n" + value
			}
		default:
			chunk.wellFormed = false
		}
	}
	if chunk.dataLines > 1 {
		chunk.wellFormed = false
	}
	chunk.hasData = chunk.dataLines > 0
	return chunk
}

// scanResponsesTerminalClaim scans a chunk that is not in the canonical
// single-frame shape for any frame naming a Responses terminal, so the
// diagnostic can say what was withheld. The chunk itself is never forwarded.
func scanResponsesTerminalClaim(raw []byte) string {
	for _, frame := range strings.Split(string(raw), "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		name, data := lenientResponsesFrameFields(frame)
		if isResponsesTerminalType(name) {
			return name
		}
		if data == "" {
			continue
		}
		if probe, ok := decodeResponsesTerminalProbe([]byte(data)); ok && isResponsesTerminalType(probe.Type) {
			return probe.Type
		}
	}
	return ""
}

// lenientResponsesFrameFields extracts the first event name and the
// SSE-joined data payload from one frame without enforcing framing rules.
func lenientResponsesFrameFields(frame string) (string, string) {
	name := ""
	var dataLines []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			if name == "" {
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			dataLines = append(dataLines, value)
		}
	}
	return name, strings.Join(dataLines, "\n")
}
