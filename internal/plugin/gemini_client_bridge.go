// Gemini native-client bridge (spec 04 §4/§5): a client speaking the native
// Gemini GenerateContent protocol is served through one of the three
// non-Gemini upstream routes by composing CPA's pinned SDK translators with
// this repo's already-verified adapters. Request bodies go
// gemini-><upstream>, upstream responses come back <upstream>->gemini, and
// stream frames are reassembled at SSE event boundaries and re-emitted as raw
// Gemini JSON chunks: CPA's handler owns data: framing, so data:-framed bytes
// here would double the SSE envelope.
//
// Composition per route:
//
//	chat-completions  direct SDK pair gemini<->openai
//	messages          direct SDK pair gemini<->claude for requests and streams;
//	                  the non-stream reply falls back to the two-stage path,
//	                  because the pinned claude->gemini non-stream converter
//	                  replays a data:-framed transcript instead of decoding a
//	                  Messages body
//	responses         no direct SDK pair exists, so both directions are
//	                  two-stage: gemini->openai then the repo's Responses
//	                  adapter builds/reads the upstream envelope
//
// A gemini-route upstream is served by the native gemini adapter, never here.
//
// The pinned SDK's chat->gemini stream translator consumes ONE signal per
// chunk (it returns on the first of role/reasoning/content/tool_calls/finish),
// so this bridge re-splits chat chunks: a chunk carrying content together with
// finish_reason or tool_calls would otherwise lose the later signals, and a
// usage trailer merged into the finish chunk would drop usage entirely.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	// The built-in registrations populate the SDK default registry this bridge
	// queries; without this import the DLL would build but every Gemini pair
	// would look unregistered at runtime.
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"

	"github.com/way326/opencode-go-omni/internal/adapter/messages"
	"github.com/way326/opencode-go-omni/internal/adapter/responses"
	"github.com/way326/opencode-go-omni/internal/adapter/shared"
	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// geminiClientUpstreamFormats maps a non-Gemini upstream route to the SDK
// format that route speaks. Every entry is a (client=gemini,
// upstream=<format>) pair: the registry keys request transforms as
// requests[from][to] and response transforms as responses[to][from], so one
// client-first pair feeds both the capability checks and the runtime calls.
var geminiClientUpstreamFormats = map[catalog.Route]sdktranslator.Format{
	catalog.RouteChatCompletions: sdktranslator.FormatOpenAI,
	catalog.RouteMessages:        sdktranslator.FormatClaude,
	catalog.RouteResponses:       sdktranslator.FormatOpenAIResponse,
}

// geminiClientFormat resolves the SDK format for one upstream route.
func geminiClientFormat(route catalog.Route) (sdktranslator.Format, bool) {
	format, ok := geminiClientUpstreamFormats[route]
	return format, ok
}

// geminiClientBridgeError reports a Gemini-client pair the pinned translators
// do not cover. It is ClassUnsupported (FR-009): the request is well formed but
// no conversion exists, so CPA surfaces it instead of forwarding an
// unconverted body.
func geminiClientBridgeError(route catalog.Route, format sdktranslator.Format, capability string) *errclass.Error {
	return &errclass.Error{
		Class: errclass.ClassUnsupported,
		Message: fmt.Sprintf(
			"native gemini client cannot reach the %s upstream route: no gemini<->%s %s is available",
			route, format, capability),
	}
}

// buildGeminiClientRequest converts a native Gemini request body into the
// upstream route's request format (FR-005). model is the resolved upstream
// model ID, which the converters write into the target body so a client-facing
// prefix never leaks upstream.
//
// Failures are explicit: an unroutable route, a missing converter, a malformed
// client body, or a converter-reported error all return a classified error, so
// the executor never sends an unconverted body upstream.
func buildGeminiClientRequest(route catalog.Route, model string, body []byte, stream bool, support ...*pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	format, ok := geminiClientFormat(route)
	if !ok {
		return nil, geminiClientBridgeError(route, sdktranslator.FormatGemini, "request translator")
	}
	var requestObject map[string]json.RawMessage
	if json.Unmarshal(body, &requestObject) != nil || requestObject == nil {
		return nil, errclass.Translation("malformed gemini request JSON")
	}
	// The Responses route has no direct SDK request pair, so its intermediate pair
	// is gemini->openai and the repo's Responses builder supplies the envelope.
	// HasRequestTransformer is keyed client-first: the pair is gemini-><pair>.
	pair := format
	if route == catalog.RouteResponses {
		pair = sdktranslator.FormatOpenAI
	}
	if !sdktranslator.HasRequestTransformer(sdktranslator.FormatGemini, pair) {
		return nil, geminiClientBridgeError(route, pair, "request translator")
	}
	translated := sdktranslator.TranslateRequestEnvelope(
		context.Background(),
		sdktranslator.FormatGemini,
		pair,
		sdktranslator.RequestEnvelope{
			Format: sdktranslator.FormatGemini,
			Model:  model,
			Stream: stream,
			Body:   body,
		},
	)
	// A request-scoped refusal is never ignorable: without this check the
	// executor would forward the untranslated Gemini body.
	if translated.Err != nil {
		return nil, errclass.Translation(
			fmt.Sprintf("gemini->%s request translation failed: %s", format, translated.Err.Error()))
	}
	out := translated.Body
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, errclass.Translation(
			fmt.Sprintf("gemini->%s request translation produced an empty body", format))
	}
	// The Responses route has no direct request pair, so the Chat Completions
	// body the SDK produced feeds the repo's verified Chat->Responses builder.
	if route == catalog.RouteResponses {
		var ts *pluginapi.ThinkingSupport
		if len(support) > 0 {
			ts = support[0]
		}
		built, eErr := responses.BuildRequest(model, "openai", out, ts)
		if eErr != nil {
			return nil, eErr
		}
		out = built
	}
	if !json.Valid(out) {
		return nil, errclass.Translation(
			fmt.Sprintf("gemini->%s request translation produced invalid JSON", format))
	}
	return out, nil
}

// looksLikeSSETranscript reports whether an upstream body is a data:-framed
// transcript rather than a single protocol reply. The pinned claude->gemini
// non-stream converter only understands the former.
func looksLikeSSETranscript(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "data:") || strings.Contains(string(body), "\ndata:")
}

// convertGeminiClientResponse converts one non-streaming upstream response body
// into a Gemini GenerateContent response (FR-006). original is the client's
// original Gemini body and translated the upstream request body; both are
// forwarded because the SDK response translators may consult them.
func convertGeminiClientResponse(route catalog.Route, model string, original, translated, body []byte) ([]byte, *errclass.Error) {
	format, ok := geminiClientFormat(route)
	if !ok {
		return nil, geminiClientBridgeError(route, sdktranslator.FormatGemini, "response translator")
	}
	var responseObject map[string]json.RawMessage
	if json.Unmarshal(body, &responseObject) != nil || responseObject == nil {
		return nil, errclass.Translation(fmt.Sprintf("malformed %s response JSON", format))
	}

	out, eErr := geminiClientNonStream(route, format, model, original, translated, body)
	if eErr != nil {
		return nil, eErr
	}
	// Silent-loss guard: a reply that carried assistant output upstream must not
	// reach the client as an empty candidate. Keyed on "output present upstream,
	// nothing expressed downstream", so it stops firing by itself once a
	// converter gap is fixed.
	if geminiClientOutputEmpty(out) && geminiClientAssistantOutputPresent(format, body) {
		return nil, errclass.Translation(fmt.Sprintf(
			"%s->gemini response translation dropped the assistant output", format))
	}
	return out, nil
}

// geminiClientNonStream runs the route's converter chain for one reply body.
func geminiClientNonStream(route catalog.Route, format sdktranslator.Format, model string, original, translated, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteResponses:
		// No direct gemini<-openai-response pair: normalize the Responses
		// envelope to Chat Completions through the repo adapter, then translate.
		chatBody, eErr := responses.ConvertNonStreamResponse("openai", 200, body)
		if eErr != nil {
			return nil, eErr
		}
		return geminiClientTranslateChatNonStream(model, original, translated, chatBody)

	case catalog.RouteMessages:
		if !looksLikeSSETranscript(body) {
			// The pinned claude->gemini non-stream converter replays a
			// transcript; a real Messages body would convert to an empty
			// candidate, so normalize it to Chat Completions first.
			chatBody, eErr := messages.ConvertNonStreamResponse("openai", 200, body)
			if eErr != nil {
				return nil, eErr
			}
			return geminiClientTranslateChatNonStream(model, original, translated, chatBody)
		}
	}

	if !sdktranslator.HasResponseTransformer(sdktranslator.FormatGemini, format) {
		return nil, geminiClientBridgeError(route, format, "response translator")
	}
	if !sdktranslator.HasNonStreamResponseTransformer(sdktranslator.FormatGemini, format) {
		return nil, geminiClientBridgeError(route, format, "non-streaming response translator")
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		context.Background(),
		format,
		sdktranslator.FormatGemini,
		model,
		original,
		translated,
		body,
		&param,
	)
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, errclass.Translation(
			fmt.Sprintf("%s->gemini response translation produced an empty body", format))
	}
	return out, nil
}

// geminiClientTranslateChatNonStream runs the second stage of a two-stage
// non-stream reply: a Chat Completions envelope into Gemini.
func geminiClientTranslateChatNonStream(model string, original, translated, chatBody []byte) ([]byte, *errclass.Error) {
	if !json.Valid(chatBody) {
		return nil, errclass.Translation("intermediate chat-completions response is not valid JSON")
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		context.Background(),
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatGemini,
		model,
		original,
		translated,
		chatBody,
		&param,
	)
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, errclass.Translation("openai->gemini response translation produced an empty body")
	}
	return out, nil
}

// geminiClientOutputEmpty reports whether a converted Gemini response carries no
// candidate content at all.
func geminiClientOutputEmpty(out []byte) bool {
	var probe struct {
		Candidates []struct {
			Content struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return false
	}
	for _, candidate := range probe.Candidates {
		if len(candidate.Content.Parts) > 0 {
			return false
		}
	}
	return true
}

// geminiClientAssistantOutputPresent reports whether an upstream response body
// carries assistant output (text, reasoning, or tool calls) a client must see,
// covering the Chat Completions and Messages envelope shapes.
func geminiClientAssistantOutputPresent(format sdktranslator.Format, body []byte) bool {
	if format == sdktranslator.FormatOpenAIResponse {
		// Responses bodies use output items, not choices; the two-stage path
		// normalizes them to Chat Completions before dispatch.
		return false
	}
	var probe struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				Reasoning json.RawMessage `json:"reasoning_content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for _, choice := range probe.Choices {
		if geminiClientJSONCarries(choice.Message.Content) ||
			geminiClientJSONCarries(choice.Message.Reasoning) ||
			geminiClientJSONCarries(choice.Message.ToolCalls) {
			return true
		}
	}
	return geminiClientJSONCarries(probe.Content)
}

// geminiClientJSONCarries reports whether a decoded JSON value holds real
// content: absent, null, empty string, empty array, and empty object do not
// count, so a legitimately empty assistant turn is not treated as dropped.
func geminiClientJSONCarries(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "[]", "{}":
		return false
	}
	return len(bytes.TrimSpace(raw)) > 0
}

// newGeminiClientStreamConverter returns the stream converter for one
// Gemini-client execution against a non-Gemini upstream route. Capability
// failures are deferred to Feed/Finish, because the executor's only signal for
// an unusable converter is a returned error, and reporting it before the first
// emission keeps the failure pre-first-byte, where a retry is still safe.
func newGeminiClientStreamConverter(route catalog.Route, model string, original, translated []byte) streamConverter {
	sc := &geminiClientStreamConverter{
		route:      route,
		model:      model,
		original:   original,
		translated: translated,
		framer:     shared.NewSSEFramer(false),
	}
	format, ok := geminiClientFormat(route)
	if !ok {
		sc.setupErr = geminiClientBridgeError(route, sdktranslator.FormatGemini, "stream response translator")
		return sc
	}
	sc.upstreamFormat = format
	switch route {
	case catalog.RouteResponses:
		// No direct gemini<-openai-response stream pair: the repo's verified
		// Responses->Chat converter produces the intermediate Chat SSE, which
		// the shared chat pipeline (below) then translates to Gemini.
		sc.chatStage = responses.NewStreamConverter("openai")
	case catalog.RouteMessages:
		// Direct gemini<-claude stream pair; nothing to compose.
	default:
		if !sdktranslator.HasStreamResponseTransformer(sdktranslator.FormatGemini, format) {
			sc.setupErr = geminiClientBridgeError(route, format, "streaming response translator")
		}
	}
	return sc
}

// geminiClientStreamConverter turns one upstream SSE stream into raw Gemini JSON
// chunks. Frames are reassembled at SSE event boundaries (data lines of one
// event join with "\n" per the spec), so any split of the HTTP byte stream and
// any legal multi-data-line frame converts exactly once.
//
// The upstream terminal is recorded, never assumed: a Chat Completions
// finish_reason chunk is followed by a usage trailer and [DONE] on the same
// stream, so terminating there would cut the tail. The stream ends at its
// explicit sentinel ([DONE] / message_stop / the Responses converter's
// done), and Finish flushes what a clean EOF left pending. A sentinel that
// arrives with no completion indicator ends the stream without a synthesized
// terminal: this bridge never invents a finishReason the upstream did not
// report.
//
// A converter is safe for SEQUENTIAL Feed calls only: use one converter per
// upstream stream.
type geminiClientStreamConverter struct {
	route          catalog.Route
	upstreamFormat sdktranslator.Format
	model          string
	original       []byte
	translated     []byte

	// setupErr, when non-nil, fails Feed and Finish before any emission.
	setupErr *errclass.Error

	// chatStage converts Responses SSE into the intermediate Chat SSE. Non-nil
	// only on the Responses route.
	chatStage streamConverter

	framer *shared.SSEFramer

	// Chat-pipeline state: signals split out of the forwarded chunks so the
	// SDK translator sees one signal per chunk.
	chunkBase     map[string]json.RawMessage // last chunk's identity fields
	pendingFinish string                     // finish_reason awaiting its own chunk
	pendingUsage  json.RawMessage            // usage awaiting its own chunk

	sawFinish   bool // upstream completion indicator seen (finish_reason / stop_reason)
	sawTerminal bool // explicit stream sentinel seen ([DONE] / message_stop / converter done)
	emitted     bool // at least one event reached the client
	finished    bool // Finish already ran
	finishErr   *errclass.Error
	param       any // SDK translator state carried across frames
}

// geminiClaudeStreamSignal classifies one Claude protocol payload. The
// Responses route needs no equivalent: its converter reports terminals and
// failures through its own done flag and error return.
type geminiClaudeStreamSignal struct {
	terminal bool   // message_stop ends the stream
	finish   bool   // message_delta carries the upstream completion indicator
	errText  string // upstream-reported failure reason
}

// geminiClaudeStreamSignalFor decodes the terminal/error markers of one Claude
// SSE payload. Unknown event types are not signals.
func geminiClaudeStreamSignalFor(data string) geminiClaudeStreamSignal {
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return geminiClaudeStreamSignal{}
	}
	switch probe.Type {
	case "message_stop":
		return geminiClaudeStreamSignal{terminal: true, finish: true}
	case "message_delta":
		return geminiClaudeStreamSignal{finish: true}
	case "error":
		return geminiClaudeStreamSignal{errText: geminiClientStreamErrorText(probe.Error)}
	}
	return geminiClaudeStreamSignal{}
}

// Feed consumes one upstream HTTP chunk (any split of the byte stream) and
// returns the Gemini chunks completed by it plus whether the stream reached its
// sentinel. Errors carry a redacted snippet, never a full frame.
func (sc *geminiClientStreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	if sc.setupErr != nil {
		return nil, false, sc.setupErr
	}
	if sc.sawTerminal {
		return nil, true, nil
	}
	sc.framer.Push(chunk)
	events, eErr = sc.drain()
	if eErr != nil {
		return events, false, eErr
	}
	// A sentinel ends the stream, so the deferred finish/usage trailers must be
	// emitted here: the executor does not call Finish after a done Feed.
	if sc.sawTerminal {
		tail, eErr := sc.flushChatTail()
		if eErr != nil {
			return events, false, eErr
		}
		events = append(events, tail...)
	}
	if len(events) > 0 {
		sc.emitted = true
	}
	return events, sc.sawTerminal, nil
}

// Finish drains a stream whose upstream closed without a sentinel: the closing
// blank line is appended so a dangling final frame still converts, whatever a
// clean EOF left pending is flushed, and a stream that never reported any
// completion indicator is failed as truncated rather than closed as if it had
// completed. Finish is idempotent: a repeated call replays the recorded outcome.
func (sc *geminiClientStreamConverter) Finish() ([][]byte, *errclass.Error) {
	if sc.finished {
		return nil, sc.finishErr
	}
	sc.finished = true
	if sc.setupErr != nil {
		sc.finishErr = sc.setupErr
		return nil, sc.finishErr
	}
	if sc.sawTerminal {
		return nil, nil
	}
	sc.framer.Push([]byte("\n\n"))
	events, eErr := sc.drain()
	if eErr != nil {
		sc.finishErr = eErr
		return nil, eErr
	}
	if !sc.sawFinish {
		sc.finishErr = &errclass.Error{
			Class:      errclass.ClassUpstream,
			Message:    fmt.Sprintf("%s stream ended before any terminal frame (%s->gemini bridge, truncated upstream stream)", sc.upstreamFormat, sc.route),
			StatusCode: 502,
			Retryable:  !sc.emitted,
		}
		return nil, sc.finishErr
	}
	// A finish_reason chunk may legally be the last frame before a clean close,
	// so the deferred trailers are the stream's only terminal evidence.
	tail, eErr := sc.flushChatTail()
	if eErr != nil {
		sc.finishErr = eErr
		return nil, eErr
	}
	events = append(events, tail...)
	if len(events) > 0 {
		sc.emitted = true
	}
	return events, nil
}

// drain pulls every complete SSE frame the framer holds.
func (sc *geminiClientStreamConverter) drain() ([][]byte, *errclass.Error) {
	var events [][]byte
	for {
		eventType, data, _, ok := sc.framer.Next()
		if !ok {
			return events, nil
		}
		if shared.IsSSEDone(data) {
			sc.sawTerminal = true
			return events, nil
		}
		if data == "" {
			continue
		}
		frames, eErr := sc.dispatch(eventType, data)
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
		if sc.sawTerminal {
			return events, nil
		}
	}
}

// dispatch routes one complete upstream payload through its route's converter
// chain: the Claude payload goes straight to the SDK pair, while Chat payloads
// (native, or produced by the Responses stage) go through the signal splitter.
func (sc *geminiClientStreamConverter) dispatch(eventType, data string) ([][]byte, *errclass.Error) {
	if !json.Valid([]byte(data)) {
		return nil, errclass.Translation(fmt.Sprintf(
			"malformed %s stream frame: %s", sc.upstreamFormat, shared.RedactedSnippet(data)))
	}
	if sc.route == catalog.RouteMessages {
		return sc.translateClaudeFrame(data)
	}

	chunks := [][]byte{[]byte(data)}
	if sc.chatStage != nil {
		// The Responses converter needs its own SSE framing, including the
		// event: name that routes the payload to its typed decoder.
		stageEvents, done, eErr := sc.chatStage.Feed([]byte(sseFrame(eventType, data)))
		if eErr != nil {
			return nil, eErr
		}
		if done {
			sc.sawTerminal = true
		}
		chunks = stageEvents
	}
	var events [][]byte
	for _, chunk := range chunks {
		frames, eErr := sc.splitChatChunk(chunk)
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
	}
	return events, nil
}

// sseFrame re-renders one reassembled payload as an SSE frame, preserving the
// event name the downstream converter dispatches on.
func sseFrame(eventType, data string) string {
	if eventType == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

// translateClaudeFrame converts one Claude payload through the direct SDK pair.
func (sc *geminiClientStreamConverter) translateClaudeFrame(data string) ([][]byte, *errclass.Error) {
	signal := geminiClaudeStreamSignalFor(data)
	if signal.finish {
		sc.sawFinish = true
	}
	events := sdktranslator.TranslateStream(
		context.Background(),
		sdktranslator.FormatClaude,
		sdktranslator.FormatGemini,
		sc.model,
		sc.original,
		sc.translated,
		[]byte("data: "+data),
		&sc.param,
	)
	if signal.errText != "" && len(events) == 0 {
		// The translator could not express the upstream failure (a Messages
		// error event does convert into a Gemini error chunk), so fail loudly
		// instead of closing the stream as if it had completed.
		return nil, &errclass.Error{
			Class:   errclass.ClassUpstream,
			Message: errclass.Redact("upstream claude stream error: " + signal.errText),
		}
	}
	if signal.terminal {
		sc.sawTerminal = true
	}
	return events, nil
}

// splitChatChunk normalizes one Chat Completions chunk and translates it. The
// SDK translator consumes a single signal per chunk, so finish_reason and usage
// are held for their own chunks and a delta carrying both content and tool_calls
// is split, which keeps every signal visible in the Gemini output.
func (sc *geminiClientStreamConverter) splitChatChunk(chunk []byte) ([][]byte, *errclass.Error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(chunk, &doc); err != nil || doc == nil {
		return nil, errclass.Translation(fmt.Sprintf(
			"malformed intermediate chat chunk: %s", shared.RedactedSnippet(string(chunk))))
	}
	base := make(map[string]json.RawMessage, 4)
	for _, key := range []string{"id", "object", "created", "model"} {
		if value, ok := doc[key]; ok {
			base[key] = value
		}
	}
	if len(base) > 0 {
		sc.chunkBase = base
	}
	if usage, ok := doc["usage"]; ok && geminiClientJSONCarries(usage) {
		sc.pendingUsage = usage
	}

	// An error frame the translator cannot express (no choices, no usage) must
	// fail the stream: otherwise the close would look like a clean completion.
	if errRaw, ok := doc["error"]; ok && geminiClientJSONCarries(errRaw) &&
		!geminiClientJSONCarries(doc["choices"]) && !geminiClientJSONCarries(doc["usage"]) {
		return nil, &errclass.Error{
			Class:   errclass.ClassUpstream,
			Message: errclass.Redact(fmt.Sprintf("upstream %s stream error: %s", sc.upstreamFormat, geminiClientStreamErrorText(errRaw))),
		}
	}

	var events [][]byte
	choices := doc["choices"]
	if !geminiClientJSONCarries(choices) {
		return nil, nil
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(choices, &list); err != nil || len(list) == 0 {
		return nil, nil
	}
	if finish, ok := list[0]["finish_reason"]; ok && geminiClientJSONCarries(finish) {
		var reason string
		if json.Unmarshal(finish, &reason) == nil && reason != "" {
			sc.pendingFinish = reason
			sc.sawFinish = true
		}
	}
	delta := list[0]["delta"]
	if !geminiClientJSONCarries(delta) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(delta, &fields); err != nil {
		return nil, errclass.Translation(fmt.Sprintf(
			"malformed intermediate chat delta: %s", shared.RedactedSnippet(string(delta))))
	}
	if content := chatDeltaPart(fields, "content", "reasoning_content"); content != nil {
		frames, eErr := sc.translateChatChunk(sc.chatChunkJSON(content, "", nil))
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
	}
	if calls, ok := fields["tool_calls"]; ok && geminiClientJSONCarries(calls) {
		frames, eErr := sc.translateChatChunk(sc.chatChunkJSON(map[string]json.RawMessage{"tool_calls": calls}, "", nil))
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
	}
	return events, nil
}

// chatDeltaPart collects the named delta members that are present and carry
// content, so an empty delta renders nothing.
func chatDeltaPart(fields map[string]json.RawMessage, names ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, name := range names {
		if value, ok := fields[name]; ok && geminiClientJSONCarries(value) {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// flushChatTail emits the deferred finish_reason and usage chunks. One-shot: the
// held values are cleared, so a second call (or the sentinel path followed by
// Finish) emits nothing.
func (sc *geminiClientStreamConverter) flushChatTail() ([][]byte, *errclass.Error) {
	var events [][]byte
	if sc.pendingFinish != "" {
		finish := sc.pendingFinish
		sc.pendingFinish = ""
		frames, eErr := sc.translateChatChunk(sc.chatChunkJSON(nil, finish, nil))
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
	}
	if len(sc.pendingUsage) > 0 {
		usage := sc.pendingUsage
		sc.pendingUsage = nil
		frames, eErr := sc.translateChatChunk(sc.chatChunkJSON(nil, "", usage))
		if eErr != nil {
			return events, eErr
		}
		events = append(events, frames...)
	}
	return events, nil
}

// translateChatChunk runs one synthesized Chat chunk through the second stage of
// the Gemini translation.
func (sc *geminiClientStreamConverter) translateChatChunk(chunk []byte) ([][]byte, *errclass.Error) {
	events := sdktranslator.TranslateStream(
		context.Background(),
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatGemini,
		sc.model,
		sc.original,
		sc.translated,
		chunk,
		&sc.param,
	)
	return events, nil
}

// chatChunkJSON renders one Chat Completions stream chunk carrying a single
// signal: a usage trailer (empty choices, usage only), a delta, or the terminal
// finish_reason. Identity fields are copied from the last upstream chunk so the
// synthesized frames stay consistent with the stream they extend.
func (sc *geminiClientStreamConverter) chatChunkJSON(delta map[string]json.RawMessage, finishReason string, usage json.RawMessage) []byte {
	frame := make(map[string]any, len(sc.chunkBase)+2)
	for key, value := range sc.chunkBase {
		frame[key] = value
	}
	frame["object"] = "chat.completion.chunk"
	if usage != nil {
		frame["choices"] = []any{}
		frame["usage"] = usage
	} else {
		if delta == nil {
			delta = map[string]json.RawMessage{}
		}
		var finish any
		if finishReason != "" {
			finish = finishReason
		}
		frame["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil
	}
	return raw
}

// geminiClientStreamErrorText extracts a readable reason from an upstream error
// value, tolerating both the string and object shapes the protocols use.
func geminiClientStreamErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return text
	}
	var obj struct {
		Message string `json:"message"`
		Status  string `json:"status"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, candidate := range []string{obj.Message, obj.Status, obj.Type, obj.Code} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}
