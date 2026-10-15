package chatcompletions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

func wantErr(t *testing.T, eErr *errclass.Error, class errclass.Class) {
	t.Helper()
	if eErr == nil || eErr.Class != class {
		t.Fatalf("want %s, got %+v", class, eErr)
	}
}

func TestConvertNonStreamStatusErrors(t *testing.T) {
	cases := map[int]errclass.Class{
		401: errclass.ClassAuth,
		402: errclass.ClassBilling,
		404: errclass.ClassInvalidModel,
		429: errclass.ClassRateLimit,
		503: errclass.ClassUpstream,
		400: errclass.ClassUnsupported,
	}
	for status, class := range cases {
		_, eErr := ConvertNonStreamResponse("claude", status, []byte(`{"error":"upstream exploded"}`))
		wantErr(t, eErr, class)
		if eErr.StatusCode != status {
			t.Fatalf("status %d not preserved: %+v", status, eErr)
		}
	}

	// Long bodies are bounded to a redacted snippet, never echoed whole.
	_, eErr := ConvertNonStreamResponse("claude", 500, []byte(`{"error":"`+strings.Repeat("y", 300)+`"}`))
	if eErr == nil || !strings.HasSuffix(eErr.Message, "...") || len(eErr.Message) > 259 {
		t.Fatalf("upstream body not bounded: %q", eErr.Message)
	}
}

func TestConvertNonStreamRedactsSecrets(t *testing.T) {
	_, eErr := ConvertNonStreamResponse("claude", 401,
		[]byte(`{"error":"bad key Bearer sk-secret-123 refused"}`))
	wantErr(t, eErr, errclass.ClassAuth)
	if strings.Contains(eErr.Message, "sk-secret-123") || !strings.Contains(eErr.Message, "[redacted]") {
		t.Fatalf("secret leaked: %q", eErr.Message)
	}
}

func TestConvertNonStreamUnsupportedFormat(t *testing.T) {
	_, eErr := ConvertNonStreamResponse("grpc", 200, []byte(`{}`))
	wantErr(t, eErr, errclass.ClassUnsupported)
}

func TestConvertNonStreamOpenAIPassthrough(t *testing.T) {
	body := []byte(`{"id":"r1","choices":[{"message":{"content":"hi"}}]}`)
	out, eErr := ConvertNonStreamResponse("openai", 200, body)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("passthrough changed body: %s", out)
	}
	if _, eErr := ConvertNonStreamResponse("openai", 200, []byte(`{`)); eErr == nil {
		t.Fatal("want translation error for malformed JSON")
	}
}

const toolCallResponse = `{"id":"r1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls",
	"message":{"role":"assistant","content":"partial",
	"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"k\":1}"}}]}}],
	"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func TestConvertNonStreamClaude(t *testing.T) {
	out, eErr := ConvertNonStreamResponse("claude", 200, []byte(toolCallResponse))
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	m := decodeOut(t, out, nil)
	if m["id"] != "r1" || m["type"] != "message" || m["role"] != "assistant" || m["model"] != "m" {
		t.Fatalf("envelope wrong: %v", m)
	}
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason wrong: %v", m)
	}
	// Canonical kernel shape: stop_sequence is omitted entirely (never
	// null), matching the Responses route and the stream message_delta.
	if strings.Contains(string(out), `"stop_sequence"`) {
		t.Fatalf("canonical shape must omit stop_sequence: %s", out)
	}
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage wrong: %v", usage)
	}
	content := m["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want text + tool_use blocks, got %v", content)
	}
	if content[0].(map[string]any)["text"] != "partial" {
		t.Fatalf("text block wrong: %v", content[0])
	}
	tu := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "c1" || tu["name"] != "f" ||
		fmt.Sprint(tu["input"]) != "map[k:1]" {
		t.Fatalf("tool_use block wrong: %v", tu)
	}
}

func TestConvertNonStreamClaudeStopReasons(t *testing.T) {
	cases := map[string]string{
		"tool_calls":     "tool_use",
		"length":         "max_tokens",
		"content_filter": "refusal",
		"stop":           "end_turn",
		"":               "end_turn",
		"semaphore":      "end_turn",
	}
	for finish, want := range cases {
		body := `{"choices":[{"finish_reason":"` + finish + `","message":{"role":"assistant","content":"x"}}]}`
		m := mustConvert(t, "claude", body)
		if m["stop_reason"] != want {
			t.Fatalf("finish %q: stop_reason=%v want %q", finish, m["stop_reason"], want)
		}
	}
}

func mustConvert(t *testing.T, format, body string) map[string]any {
	t.Helper()
	out, eErr := ConvertNonStreamResponse(format, 200, []byte(body))
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	return decodeOut(t, out, nil)
}

func TestConvertNonStreamClaudeVariants(t *testing.T) {
	t.Run("no usage zeroes counters", func(t *testing.T) {
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`)
		usage := m["usage"].(map[string]any)
		if usage["input_tokens"] != float64(0) || usage["output_tokens"] != float64(0) {
			t.Fatalf("usage not zeroed: %v", usage)
		}
	})
	t.Run("empty arguments become empty object", func(t *testing.T) {
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":""}}]}}]}`)
		tu := m["content"].([]any)[0].(map[string]any)
		if fmt.Sprint(tu["input"]) != "map[]" {
			t.Fatalf("empty args not defaulted: %v", tu)
		}
	})
	t.Run("image parts convert back", func(t *testing.T) {
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},{"type":"image_url","image_url":{"url":"https://x/i.png"}},{"type":"image_url","image_url":{"url":"data:,QUJD"}}]}}]}`)
		content := m["content"].([]any)
		img := content[1].(map[string]any)["source"].(map[string]any)
		if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "QUJD" {
			t.Fatalf("data URL not decoded: %v", img)
		}
		if content[2].(map[string]any)["source"].(map[string]any)["url"] != "https://x/i.png" {
			t.Fatalf("plain url lost: %v", content[2])
		}
		nonB64 := content[3].(map[string]any)["source"].(map[string]any)
		if nonB64["type"] != "url" || nonB64["url"] != "data:,QUJD" {
			t.Fatalf("non-base64 data URL not passed through verbatim: %v", nonB64)
		}
	})
	t.Run("responses part aliases behave identically", func(t *testing.T) {
		// F5 parity: Responses-flavored part names in upstream CC bodies
		// convert exactly like their Chat Completions names.
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"},{"type":"input_image","image_url":{"url":"https://x/i.png"}}]}}]}`)
		content := m["content"].([]any)
		if len(content) != 3 {
			t.Fatalf("alias parts wrong: %v", content)
		}
		if content[0].(map[string]any)["text"] != "a" || content[1].(map[string]any)["text"] != "b" {
			t.Fatalf("alias text blocks wrong: %v", content)
		}
		if src := content[2].(map[string]any)["source"].(map[string]any); src["type"] != "url" || src["url"] != "https://x/i.png" {
			t.Fatalf("alias image block wrong: %v", content[2])
		}
	})
	t.Run("empty string content yields no blocks", func(t *testing.T) {
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`)
		if blocks := m["content"].([]any); len(blocks) != 0 {
			t.Fatalf("expected empty content: %v", blocks)
		}
	})
	t.Run("null and empty content yield no blocks", func(t *testing.T) {
		m := mustConvert(t, "claude",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant"}}]}`)
		if blocks := m["content"].([]any); len(blocks) != 0 {
			t.Fatalf("expected empty content: %v", blocks)
		}
	})
}

func TestConvertNonStreamClaudeErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		class errclass.Class
	}{
		{"malformed json", `{`, errclass.ClassTranslation},
		{"no choices", `{"choices":[]}`, errclass.ClassTranslation},
		{"malformed args", `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{bad"}}]}}]}`, errclass.ClassTranslation},
		{"malformed content object", `{"choices":[{"message":{"content":{"bad":1}}}]}`, errclass.ClassTranslation},
		{"image missing url", `{"choices":[{"message":{"content":[{"type":"image_url"}]}}]}`, errclass.ClassTranslation},
		{"malformed data URL", `{"choices":[{"message":{"content":[{"type":"image_url","image_url":{"url":"data:x"}}]}}]}`, errclass.ClassTranslation},
		// Routed through shared.DecodeStringOrParts like every other
		// route: unknown part types are unsupported_protocol_or_parameter
		// naming the endpoint (FR-009), not translation.
		{"unsupported part type", `{"choices":[{"message":{"content":[{"type":"audio"}]}}]}`, errclass.ClassUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := ConvertNonStreamResponse("claude", 200, []byte(tc.body))
			wantErr(t, eErr, tc.class)
			if tc.class == errclass.ClassUnsupported && eErr != nil &&
				!strings.Contains(eErr.Message, "audio") {
				t.Fatalf("must name the part type: %+v", eErr)
			}
		})
	}
}

func TestConvertNonStreamResponses(t *testing.T) {
	out, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(toolCallResponse))
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	m := decodeOut(t, out, nil)
	if m["id"] != "r1" || m["object"] != "response" || m["model"] != "m" || m["status"] != "completed" {
		t.Fatalf("envelope wrong: %v", m)
	}
	output := m["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want message + function_call items, got %v", output)
	}
	msg := output[0].(map[string]any)
	if msg["type"] != "message" || msg["id"] != "r1" || msg["role"] != "assistant" {
		t.Fatalf("message item wrong: %v", msg)
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "partial" {
		t.Fatalf("output_text part wrong: %v", part)
	}
	fc := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "c1" ||
		fc["name"] != "f" || fc["arguments"] != `{"k":1}` {
		t.Fatalf("function_call item wrong: %v", fc)
	}
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(15) {
		t.Fatalf("usage wrong: %v", usage)
	}
}

func TestConvertNonStreamResponsesDeepSeekReasoning(t *testing.T) {
	body := `{"id":"resp_1","model":"deepseek-v4.1-flash","choices":[{"finish_reason":"tool_calls","message":{"reasoning_content":"先调用天气工具。","tool_calls":[{"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{}"}}]}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":2}}}`
	m := mustConvert(t, "openai-response", body)
	output := m["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want reasoning and function_call output items: %v", output)
	}
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_resp_1" {
		t.Fatalf("reasoning item identity/type wrong: %v", reasoning)
	}
	summary := reasoning["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("reasoning summary shape wrong: %v", reasoning)
	}
	part := summary[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "先调用天气工具。" {
		t.Fatalf("reasoning summary text lost: %v", part)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_weather" {
		t.Fatalf("tool call item lost: %v", call)
	}
	usage := m["usage"].(map[string]any)
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning token usage changed: %v", usage)
	}

	nonDeepSeek := mustConvert(t, "openai-response",
		`{"model":"glm-5.2","choices":[{"message":{"reasoning_content":"provider extension"}}]}`)
	if output := nonDeepSeek["output"].([]any); len(output) != 0 {
		t.Fatalf("non-DeepSeek reasoning_content should be ignored: %v", output)
	}
}

func TestDeepSeekReasoningToolRoundTripAcrossResponsesTurns(t *testing.T) {
	firstTurn := `{"input":"上海天气如何？","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`
	firstChat, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(firstTurn), nil)
	if eErr != nil {
		t.Fatalf("first Responses-to-Chat request failed: %v", eErr)
	}
	firstReq := decodeOut(t, firstChat, nil)
	if firstReq["messages"].([]any)[0].(map[string]any)["content"] != "上海天气如何？" {
		t.Fatalf("initial user input missing: %v", firstReq["messages"])
	}

	upstream := `{"id":"resp_tool","model":"deepseek-v4.1-flash","choices":[{"finish_reason":"tool_calls","message":{"reasoning_content":"先查询上海天气。","tool_calls":[{"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"上海\"}"}}]}}]}`
	responsesBytes, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(upstream))
	if eErr != nil {
		t.Fatalf("Chat-to-Responses conversion failed: %v", eErr)
	}
	var responses map[string]any
	if err := json.Unmarshal(responsesBytes, &responses); err != nil {
		t.Fatalf("decode synthesized Responses: %v", err)
	}
	items := responses["output"].([]any)
	items = append(items, map[string]any{
		"type": "function_call_output", "call_id": "call_weather", "output": "上海晴，22°C",
	})
	secondTurn, err := json.Marshal(map[string]any{"input": items})
	if err != nil {
		t.Fatal(err)
	}
	secondChat, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", secondTurn, nil)
	if eErr != nil {
		t.Fatalf("second Responses-to-Chat request failed: %v", eErr)
	}
	secondReq := decodeOut(t, secondChat, nil)
	messages := secondReq["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("second turn should contain assistant call and tool result: %v", messages)
	}
	assistant := messages[0].(map[string]any)
	if assistant["role"] != "assistant" || assistant["reasoning_content"] != "先查询上海天气。" {
		t.Fatalf("assistant reasoning was not returned to the upstream: %v", assistant)
	}
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_weather" {
		t.Fatalf("assistant tool call was not retained: %v", calls)
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_weather" || tool["content"] != "上海晴，22°C" {
		t.Fatalf("function_call_output did not pair with call: %v", tool)
	}
}

func TestConvertNonStreamResponsesVariants(t *testing.T) {
	t.Run("length maps to incomplete", func(t *testing.T) {
		m := mustConvert(t, "openai-response",
			`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"cut"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
		if m["status"] != "incomplete" {
			t.Fatalf("status wrong: %v", m)
		}
	})
	t.Run("empty result yields empty output array", func(t *testing.T) {
		m := mustConvert(t, "openai-response",
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant"}}]}`)
		if output := m["output"].([]any); len(output) != 0 {
			t.Fatalf("expected empty output: %v", output)
		}
	})
	t.Run("image content rejected explicitly", func(t *testing.T) {
		_, eErr := ConvertNonStreamResponse("openai-response", 200,
			[]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://x"}}]}}]}`))
		wantErr(t, eErr, errclass.ClassTranslation)
	})
}

func TestConvertNonStreamResponsesErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"no choices", `{"choices":[]}`},
		{"malformed content", `{"choices":[{"message":{"content":7}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(tc.body))
			wantErr(t, eErr, errclass.ClassTranslation)
		})
	}
}

func TestConvertNonStreamClaudeLengthWithToolsKeepsToolUse(t *testing.T) {
	// Observed tool calls outrank the status-derived reason: a terminal
	// length cannot downgrade stop_reason to max_tokens (FR-006).
	m := mustConvert(t, "claude",
		`{"id":"r1","model":"m","choices":[{"finish_reason":"length","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", m["stop_reason"])
	}
}

func TestConvertNonStreamResponsesLengthWithToolsIncomplete(t *testing.T) {
	// Responses-status exemption: status derives from finish alone
	// (length→incomplete) regardless of tool calls, matching the stream
	// sibling (F-R2).
	m := mustConvert(t, "openai-response",
		`{"id":"r1","model":"m","choices":[{"finish_reason":"length","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	if m["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", m["status"])
	}
}

func TestConvertNonStreamResponsesNamespaceRestored(t *testing.T) {
	rt := shared.NewResponseTools()
	reqBody := `{"input":"hi","tools":[{"type":"namespace","name":"subagents","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}]}`
	if _, eErr := BuildRequest("m", "openai-response", []byte(reqBody), nil, rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	upstream := `{"id":"r1","model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"subagents__spawn_agent","arguments":"{}"}}]}}]}`
	out, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(upstream), rt)
	if eErr != nil {
		t.Fatalf("Convert: %v", eErr)
	}
	m := decodeOut(t, out, nil)
	output := m["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("want 1 output item, got %v", output)
	}
	fc := output[0].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "spawn_agent" || fc["namespace"] != "subagents" {
		t.Fatalf("identity not restored: %v", fc)
	}
}

func TestConvertChatUsageDetails(t *testing.T) {
	body := []byte(`{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":{"content":"x"}}],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":3}}}`)
	for _, format := range []string{"claude", "openai-response"} {
		out, eErr := ConvertNonStreamResponse(format, 200, body)
		if eErr != nil {
			t.Fatalf("%s: %v", format, eErr)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		u := m["usage"].(map[string]any)
		if format == "claude" {
			if u["input_tokens"] != float64(2) || u["cache_read_input_tokens"] != float64(8) {
				t.Fatalf("claude usage = %v", u)
			}
		} else if u["input_tokens"] != float64(10) || u["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(8) || u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(3) {
			t.Fatalf("responses usage = %v", u)
		}
	}
}
