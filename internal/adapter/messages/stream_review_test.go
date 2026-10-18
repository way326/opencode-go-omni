package messages

import (
	"strings"
	"testing"
)

func TestReviewResponsesFinishRejectsMissingCompletionEvidence(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	events, done, eErr := sc.Feed([]byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cut\",\"model\":\"m\"}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
	))
	if eErr != nil || done {
		t.Fatalf("Feed done=%v err=%v", done, eErr)
	}
	finished, eErr := sc.Finish()
	if eErr == nil || !strings.Contains(eErr.Message, "truncated upstream stream") {
		t.Fatalf("Finish error = %v, want truncated upstream stream", eErr)
	}
	if len(finished) != 0 || strings.Contains(string(joinReviewEvents(events)), "response.completed") {
		t.Fatalf("truncated stream produced a terminal: feed=%q finish=%q", joinReviewEvents(events), joinReviewEvents(finished))
	}
}

func TestReviewResponsesFinishAcceptsStopReasonAtEOF(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	var events [][]byte
	for _, frame := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_eof\",\"model\":\"m\",\"usage\":{\"input_tokens\":4}}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
	} {
		fed, done, eErr := sc.Feed([]byte(frame))
		events = append(events, fed...)
		if eErr != nil || done {
			t.Fatalf("Feed done=%v err=%v", done, eErr)
		}
	}
	finished, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish: %v", eErr)
	}
	events = append(events, finished...)
	byName := namedEvents(t, events)
	if got := len(byName["response.completed"]); got != 1 {
		t.Fatalf("response.completed count = %d", got)
	}
	resp := byName["response.completed"][0]["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("response status = %v", resp["status"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestReviewResponsesFinishPreservesMaxTokensToolUsageAtEOF(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	var events [][]byte
	for _, frame := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_tool\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":2}}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":7,\"cache_creation_input_tokens\":1}}\n\n",
	} {
		fed, done, eErr := sc.Feed([]byte(frame))
		events = append(events, fed...)
		if eErr != nil || done {
			t.Fatalf("Feed done=%v err=%v", done, eErr)
		}
	}
	finished, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish: %v", eErr)
	}
	events = append(events, finished...)
	byName := namedEvents(t, events)
	if got := len(byName["response.completed"]); got != 1 {
		t.Fatalf("response.completed count = %d", got)
	}
	resp := byName["response.completed"][0]["response"].(map[string]any)
	if resp["status"] != "incomplete" {
		t.Fatalf("response status = %v", resp["status"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(8) || usage["output_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}
	output := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" ||
		call["arguments"] != "{\"q\":\"x\"}" {
		t.Fatalf("function call = %v", call)
	}
}

func TestReviewResponsesFinishAcceptsMessageStopWithoutStopReason(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	events, done, eErr := sc.Feed([]byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stop\",\"model\":\"m\"}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"complete\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))
	if eErr != nil || !done {
		t.Fatalf("Feed done=%v err=%v", done, eErr)
	}
	terminal, eErr := sc.Finish()
	if eErr != nil || len(namedEvents(t, append(events, terminal...))["response.completed"]) != 1 {
		t.Fatalf("explicit message_stop terminal missing: err=%v events=%q", eErr, joinReviewEvents(append(events, terminal...)))
	}
}

func TestReviewResponsesFinishRejectsHalfJSONFrame(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	_, done, eErr := sc.Feed([]byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_half\",\"model\":\"m\"}}\n\n" +
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}",
	))
	if eErr != nil || done {
		t.Fatalf("Feed done=%v err=%v", done, eErr)
	}
	events, eErr := sc.Finish()
	if eErr == nil {
		t.Fatal("Finish accepted a half-written JSON frame")
	}
	if len(events) != 0 || strings.Contains(string(joinReviewEvents(events)), "response.completed") {
		t.Fatalf("half-frame produced output: %q", joinReviewEvents(events))
	}
}

func TestReviewNativeMessagesEOFDoesNotSynthesizeTerminal(t *testing.T) {
	sc := NewStreamConverter("claude")
	input := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native\",\"model\":\"m\"}}\n\n"
	events, done, eErr := sc.Feed([]byte(input))
	if eErr != nil || done {
		t.Fatalf("Feed done=%v err=%v", done, eErr)
	}
	finished, eErr := sc.Finish()
	if eErr != nil || len(finished) != 0 {
		t.Fatalf("native Finish events=%q err=%v", joinReviewEvents(finished), eErr)
	}
	if string(joinReviewEvents(events)) != input {
		t.Fatalf("native events changed: %q", joinReviewEvents(events))
	}
}

func joinReviewEvents(events [][]byte) []byte {
	var out []byte
	for _, event := range events {
		out = append(out, event...)
	}
	return out
}
