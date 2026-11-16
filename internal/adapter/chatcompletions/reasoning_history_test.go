package chatcompletions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// assistantHistoryMessages returns every assistant message in out, decoded
// with encoding/json.RawMessage fields so the tests can tell an explicit JSON
// value apart from an omitted member.
func assistantHistoryMessages(t *testing.T, out []byte) []map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("outbound request is not JSON: %v", err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(m["messages"], &raw); err != nil {
		t.Fatalf("messages array is not JSON: %v", err)
	}
	msgs := make([]map[string]json.RawMessage, 0, len(raw))
	for _, fields := range raw {
		var role string
		if err := json.Unmarshal(fields["role"], &role); err != nil || role != "assistant" {
			continue
		}
		msgs = append(msgs, fields)
	}
	return msgs
}

func rawText(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

// TestResponsesReasoningHistoryKeepsContentDefined pins the Chat Completions
// contract for assistant messages that carry reasoning but neither text nor
// tool_calls: content is required unless tool_calls is present, so it must be
// serialized as an explicit empty string. A null or omitted content is what
// upstream rejected with "content or tool_calls must be set".
func TestResponsesReasoningHistoryKeepsContentDefined(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]}
	]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("reasoning-only history must keep its single assistant message: %s", out)
	}
	last := msgs[len(msgs)-1]
	if _, present := last["tool_calls"]; present {
		t.Fatalf("no tool call may be fabricated: %s", out)
	}
	if got := rawText(last["content"]); got != `""` {
		t.Fatalf("reasoning-only assistant content must be an explicit empty string, got %s: %s", got, out)
	}
	if got := rawText(last["reasoning_content"]); got != `"think"` {
		t.Fatalf("reasoning text must survive verbatim, got %s: %s", got, out)
	}
}

// TestResponsesReasoningKeepsPerTurnBoundaries drives a full tool-calling
// turn plus a trailing reasoning item, then checks that reasoning stays on
// the turn that produced it and that the final reasoning-only message keeps
// an explicit empty content.
func TestResponsesReasoningKeepsPerTurnBoundaries(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"user","content":"check"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"first"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"r"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"tail"}]}
	]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("want the tool-call turn plus the trailing reasoning-only turn, got %d: %s", len(msgs), out)
	}
	caller, trailing := msgs[0], msgs[1]
	if _, present := caller["tool_calls"]; !present {
		t.Fatalf("tool calls must be preserved on the first assistant message: %s", out)
	}
	if got := rawText(caller["reasoning_content"]); got != `"first"` {
		t.Fatalf("first-turn reasoning must stay on its own turn, got %s: %s", got, out)
	}
	if _, present := trailing["tool_calls"]; present {
		t.Fatalf("trailing reasoning must not be merged into the tool-call turn: %s", out)
	}
	if got := rawText(trailing["content"]); got != `""` {
		t.Fatalf("trailing reasoning-only content must be an explicit empty string, got %s: %s", got, out)
	}
	if got := rawText(trailing["reasoning_content"]); got != `"tail"` {
		t.Fatalf("trailing reasoning must not be dropped, got %s: %s", got, out)
	}
}

// TestResponsesNullContentAssistantKeepsReasoning covers an assistant input
// item whose content is JSON null: the pending reasoning still has to reach a
// message that satisfies the same content-or-tool_calls rule.
func TestResponsesNullContentAssistantKeepsReasoning(t *testing.T) {
	body := `{"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"assistant","content":null},
		{"type":"message","role":"user","content":"next"}
	]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want exactly one assistant message, got %d: %s", len(msgs), out)
	}
	asst := msgs[0]
	if _, present := asst["tool_calls"]; present {
		t.Fatalf("no tool call may be fabricated: %s", out)
	}
	if got := rawText(asst["content"]); got != `""` {
		t.Fatalf("null-content assistant must carry an explicit empty string, got %s: %s", got, out)
	}
	if got := rawText(asst["reasoning_content"]); got != `"think"` {
		t.Fatalf("reasoning must not be dropped, got %s: %s", got, out)
	}
}

// TestResponsesReasoningToolCallKeepsNullContent confirms that the tool-call
// leg is untouched: an assistant message that carries tool_calls still omits
// content rather than emitting an empty string, and a non-DeepSeek target
// receives no reasoning_content at all.
func TestResponsesReasoningToolCallKeepsNullContent(t *testing.T) {
	body := `{"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want one assistant tool-call message: %s", out)
	}
	if _, present := msgs[0]["tool_calls"]; !present {
		t.Fatalf("tool calls must be preserved: %s", out)
	}
	if got := rawText(msgs[0]["content"]); got != "null" {
		t.Fatalf("tool-call messages must keep the existing nil-content shape, got %s: %s", got, out)
	}

	nonDeepSeek, eErr := BuildRequest("glm-5.2", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	for _, msg := range assistantHistoryMessages(t, nonDeepSeek) {
		if _, present := msg["reasoning_content"]; present {
			t.Fatalf("reasoning_content must stay DeepSeek-only: %s", nonDeepSeek)
		}
	}
}

func TestDeepSeekToolReasoningFallbackFillsMissingHistory(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"user","content":"start"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"one"},
		{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":"two"},
		{"type":"function_call","call_id":"c3","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c3","output":"three"}
	],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("want three assistant tool-call messages, got %d: %s", len(msgs), out)
	}
	for i, msg := range msgs {
		if _, present := msg["tool_calls"]; !present {
			t.Fatalf("assistant message %d lost tool_calls: %s", i, out)
		}
		if got := rawText(msg["reasoning_content"]); got != `"[reasoning unavailable]"` {
			t.Fatalf("assistant message %d fallback reasoning = %s: %s", i, got, out)
		}
	}
}

func TestDeepSeekToolReasoningFallbackScope(t *testing.T) {
	base := `{"input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	]%s}`
	tools := `,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]`
	for _, tc := range []struct {
		name          string
		model         string
		tools         string
		wantReasoning bool
	}{
		{name: "deepseek with tools", model: "deepseek-v4.1-flash", tools: tools, wantReasoning: true},
		{name: "deepseek without tools", model: "deepseek-v4.1-flash"},
		{name: "non-deepseek with tools", model: "glm-5.2", tools: tools},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(base, tc.tools)
			out, eErr := BuildRequest(tc.model, "openai-response", []byte(body), nil)
			if eErr != nil {
				t.Fatalf("unexpected error: %v", eErr)
			}
			msgs := assistantHistoryMessages(t, out)
			if len(msgs) != 1 {
				t.Fatalf("want one assistant message, got %d: %s", len(msgs), out)
			}
			_, present := msgs[0]["reasoning_content"]
			if present != tc.wantReasoning {
				t.Fatalf("reasoning_content present=%v want %v: %s", present, tc.wantReasoning, out)
			}
		})
	}
}

func TestDeepSeekToolReasoningPreservesExistingText(t *testing.T) {
	body := `{"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if got := rawText(msgs[0]["reasoning_content"]); got != `"think"` {
		t.Fatalf("existing reasoning was replaced by fallback: %s", out)
	}
}

func TestDeepSeekEncryptedReasoningWithToolCallUsesFallback(t *testing.T) {
	body := `{"input":[
		{"type":"reasoning","encrypted_content":"opaque-secret"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	out, eErr := BuildRequest("opencode-go/deepseek-v4.1-flash", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := assistantHistoryMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want one assistant message, got %d: %s", len(msgs), out)
	}
	if got := rawText(msgs[0]["reasoning_content"]); got != `"[reasoning unavailable]"` {
		t.Fatalf("encrypted-only reasoning fallback = %s: %s", got, out)
	}
}
