package gemini

import (
	"strings"
	"testing"
)

// This file covers tool and multi-turn round trips through the native Gemini
// route: a client's tool declaration and tool-call history must become Gemini
// functionDeclarations / functionCall / functionResponse, and Gemini's
// functionCall must come back in the client's own tool-call vocabulary. The
// conversions are CPA's, so these tests pin the end-to-end behaviour this
// adapter exposes rather than re-deriving the mapping.

// geminiToolResult is one Gemini result whose model turn calls a tool.
const geminiToolResult = `{"candidates":[{"content":{"parts":[` +
	`{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-CALL"}],` +
	`"role":"model"},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10},` +
	`"modelVersion":"gemini-3-pro-upstream","responseId":"resp-tool"}`

// TestToolCallRoundTripsIntoEveryClientFormat drives one request per client
// protocol (declaring a single tool plus its call/result history) through
// BuildRequest and translates the matching Gemini result back, asserting each
// client sees a tool call with its own vocabulary.
func TestToolCallRoundTripsIntoEveryClientFormat(t *testing.T) {
	requests := map[string]string{
		FormatOpenAI: `{"model":"` + clientModel + `","messages":[` +
			`{"role":"user","content":"look it up"},` +
			`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"}],` +
			`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`,
		FormatClaude: `{"model":"` + clientModel + `","max_tokens":256,"messages":[` +
			`{"role":"user","content":[{"type":"text","text":"look it up"}]},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"{\"ok\":true}"}]}]}],` +
			`"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`,
		FormatOpenAIResp: `{"model":"` + clientModel + `","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"look it up"}]},` +
			`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"},` +
			`{"type":"function_call_output","call_id":"call_1","output":"{\"ok\":true}"}],` +
			`"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}`,
		FormatGemini: `{"model":"` + clientModel + `","contents":[` +
			`{"role":"user","parts":[{"text":"look it up"}]},` +
			`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-CALL"}]},` +
			`{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}],` +
			`"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]}`,
	}

	for format, body := range requests {
		t.Run(format, func(t *testing.T) {
			upstreamReq, eErr := BuildRequest(upstreamModel, format, []byte(body), nil)
			if eErr != nil {
				t.Fatalf("BuildRequest failed: %v", eErr)
			}
			req := string(upstreamReq)
			// Native requests retain their declarations; translated requests
			// render the equivalent camelCase Gemini vocabulary.
			declField := `"functionDeclarations"`
			if !strings.Contains(req, declField) {
				t.Fatalf("tool declaration was not rendered as %s: %s", declField, req)
			}
			if !strings.Contains(req, "lookup") {
				t.Fatalf("tool name lost: %s", req)
			}
			// The history must survive as a functionCall/functionResponse pair,
			// so multi-turn tool continuity is preserved.
			if !strings.Contains(req, `"functionCall"`) {
				t.Fatalf("tool-call history was not rendered as functionCall: %s", req)
			}
			if !strings.Contains(req, `"functionResponse"`) {
				t.Fatalf("tool-result history was not rendered as functionResponse: %s", req)
			}

			out, eErr := ConvertNonStreamResponseWithContext(format, 200, []byte(geminiToolResult), upstreamModel, []byte(body), upstreamReq)
			if eErr != nil {
				t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
			}
			got := string(out)
			if !strings.Contains(got, "lookup") {
				t.Fatalf("tool call name lost on the way back: %s", got)
			}
			switch format {
			case FormatOpenAI:
				for _, want := range []string{`"tool_calls"`, `"function"`, `"arguments"`, `"tool_calls"`} {
					if !strings.Contains(got, want) {
						t.Fatalf("openai tool call missing %q: %s", want, got)
					}
				}
			case FormatClaude:
				for _, want := range []string{`"tool_use"`, `"name":"lookup"`, `"input"`} {
					if !strings.Contains(got, want) {
						t.Fatalf("claude tool_use missing %q: %s", want, got)
					}
				}
			case FormatOpenAIResp:
				for _, want := range []string{`"function_call"`, `"arguments"`, `"call_id"`} {
					if !strings.Contains(got, want) {
						t.Fatalf("responses function_call missing %q: %s", want, got)
					}
				}
			case FormatGemini:
				if !strings.Contains(got, "SIG-CALL") {
					t.Fatalf("native tool call lost its thoughtSignature: %s", got)
				}
				if strings.Contains(got, `"arguments"`) {
					t.Fatalf("native passthrough re-encoded the gemini body: %s", got)
				}
			}
		})
	}
}

// TestResponsesNamespaceToolRoundTrip proves the flattened namespace wire name
// is restored to the client's namespace/name pair on the way back, so a
// namespaced tool call survives the Gemini leg intact. The restoration is
// CPA's, derived from the client's original request body.
func TestResponsesNamespaceToolRoundTrip(t *testing.T) {
	body := `{"model":"` + clientModel + `","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}],` +
		`"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec",` +
		`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}]}`
	upstreamReq, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, []byte(body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	if !strings.Contains(string(upstreamReq), "functions__exec") {
		t.Fatalf("namespace tool was not qualified: %s", upstreamReq)
	}

	result := `{"candidates":[{"content":{"parts":[` +
		`{"functionCall":{"name":"functions__exec","args":{"cmd":"ls"}}}],"role":"model"},"finishReason":"STOP"}],` +
		`"modelVersion":"gemini-3-pro-upstream","responseId":"resp-ns"}`
	out, eErr := ConvertNonStreamResponseWithContext(FormatOpenAIResp, 200, []byte(result), upstreamModel, []byte(body), upstreamReq)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
	}
	got := string(out)
	if !strings.Contains(got, `"name":"exec"`) {
		t.Fatalf("local tool name was not restored: %s", got)
	}
	if !strings.Contains(got, `"namespace":"functions"`) {
		t.Fatalf("namespace was not restored: %s", got)
	}
	if strings.Contains(got, "functions__exec") {
		t.Fatalf("flattened wire name leaked to the client: %s", got)
	}
}

// TestStreamToolCallRoundTrip drives a streamed functionCall through the
// converter for the two protocols whose streams carry tool calls, proving the
// call is announced with its name and complete arguments.
func TestStreamToolCallRoundTrip(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-CALL"}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10},` +
		`"modelVersion":"gemini-3-pro-upstream","responseId":"resp-tool"}` + "\n\n"

	cases := []struct {
		format string
		want   []string
	}{
		{FormatOpenAI, []string{`"tool_calls"`, `"name":"lookup"`, `"arguments":"{\"q\":\"x\"}"`}},
		{FormatClaude, []string{`"type":"tool_use"`, `"name":"lookup"`, `"partial_json"`}},
		{FormatOpenAIResp, []string{`"function_call"`, `"name":"lookup"`, `"arguments"`}},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			sc := NewStreamConverterWithContext(tc.format, upstreamModel, nil, nil)
			events, _, eErr := sc.Feed([]byte(stream))
			if eErr != nil {
				t.Fatalf("Feed failed: %v", eErr)
			}
			tail, eErr := sc.Finish()
			if eErr != nil {
				t.Fatalf("Finish failed: %v", eErr)
			}
			all := joinFrames(events) + joinFrames(tail)
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Fatalf("streamed tool call lost %q: %s", want, all)
				}
			}
		})
	}
}
