// Offline tests for the Gemini native-client bridge: request conversion, reply
// conversion, and fragmented stream reassembly. Every case runs against the
// pinned SDK translators and this repo's adapters with no network and no host
// callbacks, so the assertions are deterministic byte-level checks of what the
// bridge hands to (and returns from) CPA.
package plugin

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/errclass"
)

const geminiClientUpstreamModel = "glm-5.3"

// geminiClientBody is a multi-turn native Gemini body with a tool call, its
// function response, a system instruction, and generation config, so one
// fixture exercises the content, tool, and config mappings of every route.
const geminiClientBody = `{
  "contents": [
    {"role": "user", "parts": [{"text": "hi"}]},
    {"role": "model", "parts": [{"text": "calling"}, {"functionCall": {"name": "lookup", "args": {"q": "x"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"name": "lookup", "response": {"result": "42"}}}]}
  ],
  "systemInstruction": {"parts": [{"text": "be brief"}]},
  "generationConfig": {"temperature": 0.25, "maxOutputTokens": 64},
  "tools": [{"functionDeclarations": [{"name": "lookup", "description": "look things up", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}]}]
}`

const chatUpstreamResponse = `{"id":"r1","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"thinking"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`

const claudeUpstreamResponse = `{"id":"msg_1","model":"minimax-m3","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`

const responsesUpstreamResponse = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`

func decodeJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, raw)
	}
	return out
}

// dig walks a decoded JSON value, addressing array elements by decimal index,
// and fails the test on a missing path instead of panicking.
func dig(t *testing.T, value any, path ...string) any {
	t.Helper()
	current := value
	for i, key := range path {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				t.Fatalf("dig %v: key %q missing", path, key)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("dig %v: index %q invalid for a %d-element array", path, key, len(node))
			}
			current = node[index]
		default:
			t.Fatalf("dig %v: %q is not an object or array: %#v", path, strings.Join(path[:i], "."), current)
		}
	}
	return current
}

func requireClass(t *testing.T, eErr *errclass.Error, want errclass.Class) {
	t.Helper()
	if eErr == nil {
		t.Fatalf("expected %s error, got none", want)
	}
	if eErr.Class != want {
		t.Fatalf("error class = %s (%s), want %s", eErr.Class, eErr.Message, want)
	}
}

// geminiParts returns every part map of candidate 0, in order.
func geminiParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := dig(t, body, "candidates", "0", "content", "parts").([]any)
	if !ok {
		t.Fatalf("candidates[0].content.parts is not an array")
	}
	parts := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if obj, ok := entry.(map[string]any); ok {
			parts = append(parts, obj)
		}
	}
	return parts
}

// geminiTexts returns the text of every part, and hasFunctionCall reports
// whether any part is a functionCall with the given name.
func geminiTexts(parts []map[string]any) []string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func hasFunctionCall(parts []map[string]any, name string) bool {
	for _, part := range parts {
		call, ok := part["functionCall"].(map[string]any)
		if ok && call["name"] == name {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func joined(events [][]byte) string {
	return string(bytes.Join(events, nil))
} // ---------------------------------------------------------------------------
// request leg
// ---------------------------------------------------------------------------

// TestGeminiClientBridgeChatRouteRequest covers the gemini->chat-completions
// request leg, including the upstream model rewrite the executor relies on.
func TestGeminiClientBridgeChatRouteRequest(t *testing.T) {
	out, eErr := buildGeminiClientRequest(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte(geminiClientBody), true)
	if eErr != nil {
		t.Fatalf("buildGeminiClientRequest: %v", eErr)
	}
	if bytes.Contains(out, []byte(`"contents"`)) {
		t.Fatalf("native gemini field leaked into the upstream body: %s", out)
	}
	body := decodeJSONObject(t, out)
	if got := dig(t, body, "model"); got != geminiClientUpstreamModel {
		t.Fatalf("model = %#v, want %q", got, geminiClientUpstreamModel)
	}
	if got := dig(t, body, "stream"); got != true {
		t.Fatalf("stream = %#v, want true", got)
	}
	// systemInstruction becomes a system turn and the first client turn stays a
	// user turn, so upstream sees the same conversation shape.
	if got := dig(t, body, "messages", "0", "role"); got != "system" {
		t.Fatalf("messages[0].role = %#v, want system: %s", got, out)
	}
	if got := dig(t, body, "messages", "1", "content"); got != "hi" {
		t.Fatalf("messages[1].content = %#v, want hi: %s", got, out)
	}
}

// TestGeminiClientBridgeChatRouteKeepsMultiTurnAndTools pins the native
// capabilities the bridge must not degrade: multi-turn history, tool calls,
// tool results, the system instruction, and generation config all survive.
func TestGeminiClientBridgeChatRouteKeepsMultiTurnAndTools(t *testing.T) {
	out, eErr := buildGeminiClientRequest(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte(geminiClientBody), false)
	if eErr != nil {
		t.Fatalf("buildGeminiClientRequest: %v", eErr)
	}
	body := decodeJSONObject(t, out)
	if messages := dig(t, body, "messages").([]any); len(messages) < 5 {
		t.Fatalf("messages = %d, want system, user, model and tool turns: %s", len(messages), out)
	}
	for _, marker := range []string{`"tool_calls"`, `"tool_call_id"`, "be brief", `"tools"`, `"max_tokens"`} {
		if !bytes.Contains(out, []byte(marker)) {
			t.Fatalf("%s lost: %s", marker, out)
		}
	}
}

// TestGeminiClientBridgeMessagesRouteRequest covers the gemini->messages leg.
func TestGeminiClientBridgeMessagesRouteRequest(t *testing.T) {
	out, eErr := buildGeminiClientRequest(catalog.RouteMessages, geminiClientUpstreamModel, []byte(geminiClientBody), true)
	if eErr != nil {
		t.Fatalf("buildGeminiClientRequest: %v", eErr)
	}
	body := decodeJSONObject(t, out)
	if got := dig(t, body, "model"); got != geminiClientUpstreamModel {
		t.Fatalf("model = %#v, want %q", got, geminiClientUpstreamModel)
	}
	if got := dig(t, body, "stream"); got != true {
		t.Fatalf("stream = %#v, want true", got)
	}
	// Anthropic requires max_tokens; the SDK translator defaults it, and a
	// dropped default would make every bridged Messages call fail upstream.
	if got := dig(t, body, "max_tokens"); got == nil {
		t.Fatalf("max_tokens missing: %s", out)
	}
	for _, marker := range []string{`"tool_use"`, `"tool_result"`, "be brief"} {
		if !bytes.Contains(out, []byte(marker)) {
			t.Fatalf("%s lost: %s", marker, out)
		}
	}
}

// TestGeminiClientBridgeResponsesRouteRequest covers the layered
// gemini->chat-completions->Responses request leg: there is no direct
// gemini->openai-response SDK pair, so the repo's Responses builder must produce
// the upstream envelope.
func TestGeminiClientBridgeResponsesRouteRequest(t *testing.T) {
	if sdktranslator.HasRequestTransformer(sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse) {
		t.Fatal("a direct gemini->openai-response request pair now exists: the layered path can be simplified")
	}
	out, eErr := buildGeminiClientRequest(catalog.RouteResponses, geminiClientUpstreamModel, []byte(geminiClientBody), false)
	if eErr != nil {
		t.Fatalf("buildGeminiClientRequest: %v", eErr)
	}
	t.Logf("responses request body = %s", out)
	if bytes.Contains(out, []byte(`"contents"`)) {
		t.Fatalf("unconverted gemini body would reach upstream: %s", out)
	}
	body := decodeJSONObject(t, out)
	if got := dig(t, body, "model"); got != geminiClientUpstreamModel {
		t.Fatalf("model = %#v, want %q", got, geminiClientUpstreamModel)
	}
	if !bytes.Contains(out, []byte("hi")) {
		t.Fatalf("client text lost: %s", out)
	}
}

// TestGeminiClientBridgeRejectsUnroutableInput pins the explicit-failure paths:
// a gemini-route upstream has no bridge, and a malformed client body must never
// be forwarded.
func TestGeminiClientBridgeRejectsUnroutableInput(t *testing.T) {
	if out, eErr := buildGeminiClientRequest(catalog.RouteGemini, geminiClientUpstreamModel, []byte(geminiClientBody), false); out != nil {
		t.Fatalf("gemini-route request produced %s (%v)", out, eErr)
	} else {
		requireClass(t, eErr, errclass.ClassUnsupported)
	}
	if out, eErr := buildGeminiClientRequest(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte("{"), false); out != nil {
		t.Fatalf("malformed request produced %s (%v)", out, eErr)
	} else {
		requireClass(t, eErr, errclass.ClassTranslation)
	}
}

// ---------------------------------------------------------------------------
// reply leg
// ---------------------------------------------------------------------------

// TestGeminiClientBridgeChatRouteResponse covers the direct upstream->gemini leg
// for a Chat Completions upstream, including the reasoning part the translator
// places before the visible text.
func TestGeminiClientBridgeChatRouteResponse(t *testing.T) {
	out, eErr := convertGeminiClientResponse(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte(geminiClientBody), []byte(`{"model":"glm-5.3"}`), []byte(chatUpstreamResponse))
	if eErr != nil {
		t.Fatalf("convertGeminiClientResponse: %v", eErr)
	}
	body := decodeJSONObject(t, out)
	texts := geminiTexts(geminiParts(t, body))
	if !containsString(texts, "hello") || !containsString(texts, "thinking") {
		t.Fatalf("parts lost: %#v (%s)", texts, out)
	}
	if got := dig(t, body, "candidates", "0", "finishReason"); got != "STOP" {
		t.Fatalf("finishReason = %#v (%s)", got, out)
	}
	if bytes.Contains(out, []byte(`"choices"`)) {
		t.Fatalf("upstream envelope leaked to the gemini client: %s", out)
	}
}

// TestGeminiClientBridgeMessagesRouteResponse proves the Messages reply leg the
// reviewer required: text, tool call, and usage all reach a Gemini client. The
// pinned claude->gemini non-stream converter cannot decode a Messages body, so
// the bridge must compose the repo's Messages->Chat adapter with the SDK's
// chat->gemini translator.
func TestGeminiClientBridgeMessagesRouteResponse(t *testing.T) {
	out, eErr := convertGeminiClientResponse(catalog.RouteMessages, "minimax-m3", []byte(geminiClientBody), []byte(`{"model":"minimax-m3"}`), []byte(claudeUpstreamResponse))
	if eErr != nil {
		t.Fatalf("convertGeminiClientResponse: %v", eErr)
	}
	t.Logf("claude->gemini reply = %s", out)
	body := decodeJSONObject(t, out)
	parts := geminiParts(t, body)
	if !containsString(geminiTexts(parts), "hello") {
		t.Fatalf("text lost: %s", out)
	}
	if !hasFunctionCall(parts, "lookup") {
		t.Fatalf("tool call lost: %s", out)
	}
	if got := dig(t, body, "candidates", "0", "finishReason"); got != "STOP" {
		t.Fatalf("finishReason = %#v (%s)", got, out)
	}
	if got := dig(t, body, "usageMetadata", "promptTokenCount"); got != float64(2) {
		t.Fatalf("promptTokenCount = %#v (%s)", got, out)
	}
	if got := dig(t, body, "usageMetadata", "candidatesTokenCount"); got != float64(3) {
		t.Fatalf("candidatesTokenCount = %#v (%s)", got, out)
	}
}

// TestGeminiClientBridgeResponsesRouteResponse covers the layered
// Responses->chat->gemini reply leg.
func TestGeminiClientBridgeResponsesRouteResponse(t *testing.T) {
	out, eErr := convertGeminiClientResponse(catalog.RouteResponses, geminiClientUpstreamModel, []byte(geminiClientBody), []byte(`{"model":"glm-5.3"}`), []byte(responsesUpstreamResponse))
	if eErr != nil {
		t.Fatalf("convertGeminiClientResponse: %v", eErr)
	}
	t.Logf("responses->gemini reply = %s", out)
	body := decodeJSONObject(t, out)
	if !containsString(geminiTexts(geminiParts(t, body)), "hello") {
		t.Fatalf("text lost: %s", out)
	}
	if bytes.Contains(out, []byte(`"output"`)) {
		t.Fatalf("upstream envelope leaked to the gemini client: %s", out)
	}
} // TestGeminiClientBridgeResponseGuards covers the silent-loss guard and the
// malformed-body / unroutable-route refusals.
func TestGeminiClientBridgeResponseGuards(t *testing.T) {
	if !geminiClientOutputEmpty([]byte(`{"candidates":[{"content":{"parts":[],"role":"model"}}]}`)) {
		t.Fatal("empty candidate not detected")
	}
	if geminiClientOutputEmpty([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)) {
		t.Fatal("content-bearing candidate reported empty")
	}
	for _, test := range []struct {
		name   string
		format sdktranslator.Format
		body   string
		want   bool
	}{
		{name: "chat text", format: sdktranslator.FormatOpenAI, body: chatUpstreamResponse, want: true},
		{name: "chat tool only", format: sdktranslator.FormatOpenAI, body: `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1"}]}}]}`, want: true},
		{name: "chat empty", format: sdktranslator.FormatOpenAI, body: `{"choices":[{"message":{"role":"assistant","content":""}}]}`, want: false},
		{name: "claude text", format: sdktranslator.FormatClaude, body: claudeUpstreamResponse, want: true},
		{name: "claude empty", format: sdktranslator.FormatClaude, body: `{"content":[],"stop_reason":"end_turn"}`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := geminiClientAssistantOutputPresent(test.format, []byte(test.body)); got != test.want {
				t.Fatalf("assistant output present = %t, want %t", got, test.want)
			}
		})
	}
	if out, eErr := convertGeminiClientResponse(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte(geminiClientBody), nil, []byte("not json")); out != nil {
		t.Fatalf("malformed upstream body produced %s (%v)", out, eErr)
	} else {
		requireClass(t, eErr, errclass.ClassTranslation)
	}
	if out, eErr := convertGeminiClientResponse(catalog.RouteGemini, geminiClientUpstreamModel, nil, nil, []byte(chatUpstreamResponse)); out != nil {
		t.Fatalf("gemini-route response produced %s (%v)", out, eErr)
	} else {
		requireClass(t, eErr, errclass.ClassUnsupported)
	}
}

// ---------------------------------------------------------------------------
// stream leg
// ---------------------------------------------------------------------------

// finishable is the executor's close-without-terminal seam: pumpStream reaches
// it by type assertion, never through the Feed-only converter interface, so the
// tests must assert it the same way.
type finishable interface {
	Finish() ([][]byte, *errclass.Error)
}

// finishStream calls the Finish seam exactly as the executor's pump does.
func finishStream(t *testing.T, conv streamConverter) ([][]byte, *errclass.Error) {
	t.Helper()
	finisher, ok := conv.(finishable)
	if !ok {
		t.Fatal("stream converter has no Finish seam")
	}
	return finisher.Finish()
}

// feedFragmented hands the converter one byte at a time, which is the strongest
// offline stand-in for arbitrary HTTP chunk boundaries.
func feedFragmented(t *testing.T, conv streamConverter, stream string) (events [][]byte, done bool) {
	t.Helper()
	for i := 0; i < len(stream); i++ {
		ev, isDone, eErr := conv.Feed([]byte(stream[i : i+1]))
		if eErr != nil {
			t.Fatalf("Feed at byte %d: %v", i, eErr)
		}
		events = append(events, ev...)
		if isDone {
			if rest := strings.Trim(stream[i+1:], "\r\n"); rest != "" {
				t.Fatalf("stream reported done at byte %d with trailing payload %q", i, rest)
			}
			return events, true
		}
	}
	return events, false
}

// requireRawGeminiChunks asserts every emitted event is a bare Gemini JSON
// chunk: CPA's handler adds the data: framing, so a frame here would double it.
func requireRawGeminiChunks(t *testing.T, events [][]byte) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	for _, event := range events {
		if !json.Valid(event) {
			t.Fatalf("event is not raw JSON: %q", event)
		}
		trimmed := bytes.TrimSpace(event)
		if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
			t.Fatalf("event carries SSE framing: %q", event)
		}
	}
}

// TestGeminiClientBridgeChatRouteStreamFragmentReassembly drives a Chat
// Completions stream one byte at a time through the realistic upstream shape:
// text deltas, tool-call fragments, a finish_reason chunk, the separate usage
// trailer, then [DONE]. The finish_reason chunk must NOT end the stream.
func TestGeminiClientBridgeChatRouteStreamFragmentReassembly(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]}}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"x\"}"}}]}}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.3","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
		"",
	}, "\n")

	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, []byte(geminiClientBody), []byte(`{"model":"glm-5.3"}`))
	events, done := feedFragmented(t, conv, stream)
	if !done {
		t.Fatal("stream never reported its [DONE] sentinel")
	}
	requireRawGeminiChunks(t, events)
	blob := joined(events)
	t.Logf("chat stream chunks = %s", blob)
	if !strings.Contains(blob, `"hel"`) || !strings.Contains(blob, `"lo"`) {
		t.Fatalf("text deltas lost: %s", blob)
	}
	if !strings.Contains(blob, `"finishReason":"TOOL_CALLS"`) && !strings.Contains(blob, `"finishReason":"STOP"`) {
		t.Fatalf("terminal finishReason lost: %s", blob)
	}
	// The usage trailer arrives after finish_reason, so the converter must not
	// stop at the finish chunk; the SDK needs it as its own chunk too.
	if !strings.Contains(blob, `"usageMetadata"`) || !strings.Contains(blob, `"candidatesTokenCount":3`) {
		t.Fatalf("include_usage trailer lost: %s", blob)
	}
	if !strings.Contains(blob, `"functionCall"`) || !strings.Contains(blob, `"name":"lookup"`) {
		t.Fatalf("tool call fragments lost: %s", blob)
	}
	if tail, eErr := finishStream(t, conv); eErr != nil || len(tail) != 0 {
		t.Fatalf("Finish after [DONE] = %d events, %v; want none", len(tail), eErr)
	}
}

// TestGeminiClientBridgeChatRouteStreamFinishDoesNotTerminate pins the defect the
// reviewer found: a finish_reason frame is followed by the usage trailer and
// [DONE] on the same stream, so the converter must keep reading past it.
func TestGeminiClientBridgeChatRouteStreamFinishDoesNotTerminate(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	prefix := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	events, done, eErr := conv.Feed([]byte(prefix))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	if done {
		t.Fatalf("finish_reason ended the stream before its usage trailer and [DONE]: %s", joined(events))
	}
	if blob := joined(events); !strings.Contains(blob, `"text":"hi"`) {
		t.Fatalf("text delta lost: %s", blob)
	}
	// The deferred finish_reason must not leak out before the stream ends.
	if blob := joined(events); strings.Contains(blob, `"finishReason"`) {
		t.Fatalf("finishReason emitted before the stream terminal: %s", blob)
	}
	tail, done, eErr := conv.Feed([]byte("data: {\"id\":\"c1\",\"model\":\"glm-5.3\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"))
	if eErr != nil || !done {
		t.Fatalf("Feed tail: done=%t err=%v", done, eErr)
	}
	blob := joined(tail)
	if !strings.Contains(blob, `"finishReason":"STOP"`) || !strings.Contains(blob, `"usageMetadata"`) {
		t.Fatalf("finish/usage tail lost: %s", blob)
	}
} // TestGeminiClientBridgeChatRouteStreamMergedFinishUsage covers an upstream that
// merges finish_reason and usage into one chunk: both signals must still reach
// the client, which requires splitting them into separate SDK chunks.
func TestGeminiClientBridgeChatRouteStreamMergedFinishUsage(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	stream := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"
	events, _, eErr := conv.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	if blob := joined(events); !strings.Contains(blob, `"text":"hi"`) {
		t.Fatalf("content lost when merged with finish_reason: %s", blob)
	}
	tail, eErr := finishStream(t, conv)
	if eErr != nil {
		t.Fatalf("Finish: %v", eErr)
	}
	blob := joined(append(events, tail...))
	if !strings.Contains(blob, `"finishReason":"STOP"`) || !strings.Contains(blob, `"usageMetadata"`) {
		t.Fatalf("merged finish/usage lost: %s", blob)
	}
	if !strings.Contains(blob, `"candidatesTokenCount":2`) {
		t.Fatalf("usage values lost: %s", blob)
	}
}

// TestGeminiClientBridgeChatRouteStreamCleanCloseWithoutDone covers a clean EOF
// after the finish_reason chunk: Finish must flush the held terminal instead of
// failing, because the usage-less finish chunk may legally be the last frame.
func TestGeminiClientBridgeChatRouteStreamCleanCloseWithoutDone(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	stream := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}"
	_, done, eErr := conv.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	if done {
		t.Fatal("a clean EOF is not a sentinel; done must be false")
	}
	flushed, eErr := finishStream(t, conv)
	if eErr != nil {
		t.Fatalf("Finish on a finished-but-unsentineled stream: %v", eErr)
	}
	requireRawGeminiChunks(t, flushed)
	blob := joined(flushed)
	if !strings.Contains(blob, `"finishReason":"STOP"`) || !strings.Contains(blob, `"usageMetadata"`) {
		t.Fatalf("Finish dropped the flushed terminal: %s", blob)
	}
	if again, eErr := finishStream(t, conv); eErr != nil || len(again) != 0 {
		t.Fatalf("Finish is not idempotent: %d events, %v", len(again), eErr)
	}
}

// TestGeminiClientBridgeChatRouteStreamTruncated covers a stream that dies
// before any completion indicator: closing it as if it had completed would
// report a truncated reply as success, so Finish must fail with a 502.
func TestGeminiClientBridgeChatRouteStreamTruncated(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	events, done, eErr := conv.Feed([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n"))
	if eErr != nil || done || len(events) == 0 {
		t.Fatalf("Feed: events=%d done=%t err=%v", len(events), done, eErr)
	}
	flushed, eErr := finishStream(t, conv)
	if eErr == nil {
		t.Fatalf("truncated stream closed cleanly: %q", joined(flushed))
	}
	if eErr.Class != errclass.ClassUpstream || eErr.StatusCode != 502 {
		t.Fatalf("truncation classified as %s/%d (%s)", eErr.Class, eErr.StatusCode, eErr.Message)
	}
	if !strings.Contains(eErr.Message, "truncated") {
		t.Fatalf("truncation message = %s", eErr.Message)
	}
	if eErr.Retryable {
		t.Fatal("a post-first-byte truncation must not be retryable")
	}
	if len(flushed) != 0 {
		t.Fatalf("truncated stream still emitted %q", joined(flushed))
	}
	if again, eErr := finishStream(t, conv); eErr == nil || len(again) != 0 {
		t.Fatalf("Finish replayed %d events, err=%v", len(again), eErr)
	}
}

// TestGeminiClientBridgeMessagesRouteStreamFragments drives a Messages stream
// with CRLF line endings, event: names, and fragmented payloads, and pins that
// only message_stop terminates it.
func TestGeminiClientBridgeMessagesRouteStreamFragments(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"minimax-m3","usage":{"input_tokens":2}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
		``,
	}, "\r\n")

	conv := newGeminiClientStreamConverter(catalog.RouteMessages, "minimax-m3", []byte(geminiClientBody), []byte(`{"model":"minimax-m3"}`))
	events, done := feedFragmented(t, conv, stream)
	if !done {
		t.Fatal("message_stop did not terminate the stream")
	}
	requireRawGeminiChunks(t, events)
	blob := joined(events)
	t.Logf("claude stream chunks = %s", blob)
	if !strings.Contains(blob, `"hel"`) || !strings.Contains(blob, `"lo"`) {
		t.Fatalf("text deltas lost: %s", blob)
	}
	if !strings.Contains(blob, `"name":"lookup"`) {
		t.Fatalf("tool call lost: %s", blob)
	}
	if !strings.Contains(blob, `"promptTokenCount":2`) || !strings.Contains(blob, `"candidatesTokenCount":3`) {
		t.Fatalf("usage mapping lost: %s", blob)
	}
}

// TestGeminiClientBridgeMessagesRouteStreamErrorEvent checks that a Messages
// error event reaches the client as a Gemini error chunk instead of a silently
// truncated stream.
func TestGeminiClientBridgeMessagesRouteStreamErrorEvent(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteMessages, "minimax-m3", nil, nil)
	events, _, eErr := conv.Feed([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	requireRawGeminiChunks(t, events)
	if blob := joined(events); !strings.Contains(blob, `"error"`) || !strings.Contains(blob, "overloaded") {
		t.Fatalf("upstream error event lost: %s", blob)
	}
}

// TestGeminiClientBridgeResponsesRouteStream covers the layered
// Responses->Chat->gemini stream leg, including its converter-reported terminal
// and the usage that arrives with it.
func TestGeminiClientBridgeResponsesRouteStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"hel"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"lo"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
		``,
		``,
	}, "\n")

	conv := newGeminiClientStreamConverter(catalog.RouteResponses, geminiClientUpstreamModel, []byte(geminiClientBody), []byte(`{"model":"glm-5.3"}`))
	events, done := feedFragmented(t, conv, stream)
	if !done {
		t.Fatal("response.completed did not terminate the stream")
	}
	requireRawGeminiChunks(t, events)
	blob := joined(events)
	t.Logf("responses stream chunks = %s", blob)
	if !strings.Contains(blob, `"hel"`) || !strings.Contains(blob, `"lo"`) {
		t.Fatalf("text deltas lost: %s", blob)
	}
	if !strings.Contains(blob, `"usageMetadata"`) {
		t.Fatalf("usage lost: %s", blob)
	}
	if !strings.Contains(blob, `"finishReason"`) {
		t.Fatalf("finishReason lost: %s", blob)
	}
}

// TestGeminiClientBridgeResponsesRouteStreamTruncated pins that a Responses
// stream which never completes is reported as truncated.
func TestGeminiClientBridgeResponsesRouteStreamTruncated(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteResponses, geminiClientUpstreamModel, nil, nil)
	events, done, eErr := conv.Feed([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"delta\":\"par\"}\n\n"))
	if eErr != nil || done {
		t.Fatalf("Feed: done=%t err=%v", done, eErr)
	}
	if len(events) == 0 {
		t.Fatal("no events emitted for the text delta")
	}
	if _, eErr := finishStream(t, conv); eErr == nil || eErr.StatusCode != 502 {
		t.Fatalf("truncated responses stream closed with %v", eErr)
	}
} // TestGeminiClientBridgeChatRouteStreamErrorFrame checks the failure path for
// upstreams whose error frame the translator cannot express: the stream must fail
// explicitly rather than close as if it had completed.
func TestGeminiClientBridgeChatRouteStreamErrorFrame(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	events, done, eErr := conv.Feed([]byte("data: {\"error\":{\"message\":\"upstream exploded\",\"type\":\"server_error\"}}\n\n"))
	if eErr == nil {
		t.Fatalf("error frame accepted as a clean chunk: %d events, done=%t", len(events), done)
	}
	if eErr.Class != errclass.ClassUpstream || !strings.Contains(eErr.Message, "upstream exploded") {
		t.Fatalf("error classification = %s (%s)", eErr.Class, eErr.Message)
	}
	if len(events) != 0 {
		t.Fatalf("events emitted with the failure: %q", events)
	}
}

// TestGeminiClientBridgeStreamMalformedFrame pins the malformed-frame guard.
func TestGeminiClientBridgeStreamMalformedFrame(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	events, _, eErr := conv.Feed([]byte("data: {not json}\n\n"))
	if eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("malformed frame error = %v (events=%q)", eErr, events)
	}
	if !strings.Contains(eErr.Message, "malformed") {
		t.Fatalf("malformed frame message = %s", eErr.Message)
	}
}

// TestGeminiClientBridgeStreamUnroutableRoute pins the gemini-route guard on both
// converter entry points.
func TestGeminiClientBridgeStreamUnroutableRoute(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteGemini, geminiClientUpstreamModel, nil, nil)
	if _, _, eErr := conv.Feed([]byte("data: {\"candidates\":[]}\n\n")); eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("gemini-route Feed error = %v", eErr)
	}
	if flushed, eErr := finishStream(t, conv); eErr == nil || eErr.Class != errclass.ClassUnsupported || len(flushed) != 0 {
		t.Fatalf("gemini-route Finish = %d events, %v", len(flushed), eErr)
	}
}

// TestGeminiClientBridgeStreamMultiLineDataFrame pins spec-correct frame
// reassembly: a payload split across data lines of ONE event decodes once.
func TestGeminiClientBridgeStreamMultiLineDataFrame(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	chunk := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\n" +
		"data: \"split\"}}],\"model\":\"glm-5.3\"}\n\n"
	events, _, eErr := conv.Feed([]byte(chunk))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	requireRawGeminiChunks(t, events)
	if blob := joined(events); !strings.Contains(blob, "split") {
		t.Fatalf("multi-line frame lost: %s", blob)
	}
}

// TestGeminiClientBridgeStreamRespectsEventBoundary pins the reviewer's point
// that data lines are joined per SSE event instead of being dispatched as soon as
// each one happens to be valid JSON: two individually-valid payloads in one event
// must not be emitted as two translated chunks.
func TestGeminiClientBridgeStreamRespectsEventBoundary(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	open := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"two\"}}]}\n"
	events, _, eErr := conv.Feed([]byte(open))
	if eErr != nil {
		t.Fatalf("Feed before the event boundary: %v", eErr)
	}
	if len(events) != 0 {
		t.Fatalf("payloads dispatched before the blank-line event boundary: %s", joined(events))
	}
	// Closing the event yields the joined payload, which is not one JSON
	// document: the frame is reported malformed rather than silently split.
	_, _, eErr = conv.Feed([]byte("\n"))
	if eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("joined multi-payload event error = %v", eErr)
	}
}

// TestGeminiClientBridgeStreamIgnoresNoise keeps heartbeats, comments, and
// event-only frames from producing output or ending the stream.
func TestGeminiClientBridgeStreamIgnoresNoise(t *testing.T) {
	conv := newGeminiClientStreamConverter(catalog.RouteChatCompletions, geminiClientUpstreamModel, nil, nil)
	events, done, eErr := conv.Feed([]byte(": keepalive\n\nevent: ping\n\nretry: 100\n\n"))
	if eErr != nil || done {
		t.Fatalf("noise: done=%t err=%v", done, eErr)
	}
	if len(events) != 0 {
		t.Fatalf("noise produced events: %s", joined(events))
	}
}

// TestGeminiClaudeStreamSignalTable covers the Claude terminal/error classifier
// that decides when the direct Messages stream ends.
func TestGeminiClaudeStreamSignalTable(t *testing.T) {
	tests := []struct {
		name         string
		data         string
		wantTerminal bool
		wantFinish   bool
		wantErr      string
	}{
		{name: "message_stop", data: `{"type":"message_stop"}`, wantTerminal: true, wantFinish: true},
		{name: "message_delta", data: `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, wantFinish: true},
		{name: "text delta", data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`},
		{name: "error object", data: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, wantErr: "overloaded"},
		{name: "error string", data: `{"type":"error","error":"boom"}`, wantErr: "boom"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			signal := geminiClaudeStreamSignalFor(test.data)
			if signal.terminal != test.wantTerminal || signal.finish != test.wantFinish || signal.errText != test.wantErr {
				t.Fatalf("signal = %+v, want terminal=%t finish=%t err=%q", signal, test.wantTerminal, test.wantFinish, test.wantErr)
			}
		})
	}
}

// TestGeminiClientStreamErrorTextShapes covers the fallback ordering of the
// upstream error decoder.
func TestGeminiClientStreamErrorTextShapes(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: ``, want: ""},
		{raw: `null`, want: ""},
		{raw: `"plain"`, want: "plain"},
		{raw: `{"message":"m","status":"s","type":"t"}`, want: "m"},
		{raw: `{"status":"s","type":"t"}`, want: "s"},
		{raw: `{"type":"t"}`, want: "t"},
		{raw: `{"code":"c"}`, want: "c"},
	} {
		if got := geminiClientStreamErrorText([]byte(test.raw)); got != test.want {
			t.Fatalf("error text for %s = %q, want %q", test.raw, got, test.want)
		}
	}
}

// TestGeminiClientBridgeLooksLikeSSETranscript pins the discriminator that picks
// the direct converter (transcript bodies) from the two-stage path.
func TestGeminiClientBridgeLooksLikeSSETranscript(t *testing.T) {
	if !looksLikeSSETranscript([]byte("data: {\"type\":\"message_delta\"}\n\n")) {
		t.Fatal("leading data: line not detected")
	}
	if !looksLikeSSETranscript([]byte("event: x\ndata: {}\n")) {
		t.Fatal("embedded data: line not detected")
	}
	if looksLikeSSETranscript([]byte(claudeUpstreamResponse)) {
		t.Fatal("plain JSON reply reported as a transcript")
	}
}
