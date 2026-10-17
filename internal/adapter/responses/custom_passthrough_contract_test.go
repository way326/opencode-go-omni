package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
)

// ---- native passthrough contract (non-GPT upstream, FR-006) -------------
//
// The upstream sees custom tools declared as functions with a required
// string input, so it streams the input wrapped in {"input":"..."}. The
// passthrough must hand the client the custom shape it declared: item
// events restore to custom_tool_call, argument fragments become
// custom_tool_call_input.delta carrying unwrapped input only, and names
// flattened from a namespace tool regain their original name and
// namespace. Non-custom items stay byte-identical.

// passthroughRegistry registers the supplied declarations the same way
// BuildRequest does, so IsCustom/RestoreWire answer exactly as they would
// in one production request.
func passthroughRegistry(t *testing.T, tools ...shared.RespTool) *shared.ResponseTools {
	t.Helper()
	rt := shared.NewResponseTools()
	r := &shared.ResponsesRequest{Tools: tools}
	if _, eErr := rt.Normalize(r, EndpointPath); eErr != nil {
		t.Fatalf("Normalize: %+v", eErr)
	}
	return rt
}

func namespaceRegistry(t *testing.T) *shared.ResponseTools {
	t.Helper()
	return passthroughRegistry(t,
		shared.RespTool{Type: "namespace", Name: "subagents", Tools: []shared.RespTool{
			{Type: "custom", Name: "custom_sub"},
			{Type: "function", Name: "spawn_agent", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		shared.RespTool{Type: "function", Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)},
	)
}

// perByte splits a raw SSE body into single-byte network chunks so every
// framer boundary, JSON envelope split and UTF-8 sequence is exercised.
func perByte(s string) []string {
	out := make([]string, 0, len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i:i+1])
	}
	return out
}

func encodeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func outboundFunctionCall(id, callID, name, args string) map[string]any {
	item := map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": args}
	if id != "" {
		item["id"] = id
	}
	return item
}

func itemAddedPayload(index int, item map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
	return string(b)
}

func itemDonePayload(index int, item map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	return string(b)
}

func argsDeltaPayload(itemID string, index, sequence int, delta string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": itemID, "output_index": index, "sequence_number": sequence, "delta": delta,
	})
	return string(b)
}

func argsDonePayload(itemID string, index int, name, args string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "response.function_call_arguments.done",
		"item_id": itemID, "output_index": index, "name": name, "arguments": args,
	})
	return string(b)
}

func completedPayload() string {
	return `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`
}

func eventPayload(t *testing.T, event []byte) map[string]any {
	t.Helper()
	_, data := parseSSE(t, event)
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("payload not JSON: %v (%q)", err, data)
	}
	return m
}

func TestPassthroughCustomArgumentDeltasUseInputNamespace(t *testing.T) {
	rt := passthroughRegistry(t, shared.RespTool{Type: "custom", Name: "exec"})
	code := "console.log('hi')"
	wrapped := shared.WrapCustomToolInput(code)
	raw := frame("response.output_item.added", itemAddedPayload(0, outboundFunctionCall("fc_1", "call_1", "exec", ""))) +
		frame("response.function_call_arguments.delta", argsDeltaPayload("fc_1", 0, 7, wrapped[:12])) +
		frame("response.function_call_arguments.delta", argsDeltaPayload("fc_1", 0, 8, wrapped[12:])) +
		frame("response.function_call_arguments.done", argsDonePayload("fc_1", 0, "exec", wrapped)) +
		frame("response.output_item.done", itemDonePayload(0, outboundFunctionCall("fc_1", "call_1", "exec", wrapped))) +
		frame("response.completed", completedPayload())

	events, done, eErr := feedChunks(t, NewStreamConverter("openai-response", rt), perByte(raw))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if !done {
		t.Fatalf("stream not terminal")
	}
	var deltas []string
	var sawFunctionDelta, sawInputDone, sawItemDone bool
	var inputDone, itemDone map[string]any
	for _, ev := range events {
		event, _ := parseSSE(t, ev)
		switch event {
		case "response.function_call_arguments.delta":
			sawFunctionDelta = true
		case "response.custom_tool_call_input.delta":
			payload := eventPayload(t, ev)
			if payload["item_id"] != "fc_1" || payload["output_index"] != float64(0) {
				t.Fatalf("input delta references wrong item: %v", payload)
			}
			delta, _ := payload["delta"].(string)
			deltas = append(deltas, delta)
		case "response.custom_tool_call_input.done":
			sawInputDone = true
			inputDone = eventPayload(t, ev)
		case "response.output_item.done":
			sawItemDone = true
			itemDone = eventPayload(t, ev)
		}
	}
	if sawFunctionDelta {
		t.Fatalf("custom call leaked function_call_arguments.delta: %s", events)
	}
	if got := strings.Join(deltas, ""); got != code {
		t.Fatalf("input deltas = %q, want %q", got, code)
	}
	for _, d := range deltas {
		if strings.Contains(d, "input") || strings.Contains(d, "\"") {
			t.Fatalf("wrapper token leaked into input delta %q", d)
		}
	}
	if !sawInputDone || inputDone["input"] != code {
		t.Fatalf("input done = %v", inputDone)
	}
	if inputDone["item_id"] != "fc_1" || inputDone["output_index"] != float64(0) {
		t.Fatalf("input done lost item references: %v", inputDone)
	}
	if _, has := inputDone["arguments"]; has {
		t.Fatalf("input done kept arguments: %v", inputDone)
	}
	if _, has := inputDone["name"]; has {
		t.Fatalf("input done kept name: %v", inputDone)
	}
	if !sawItemDone {
		t.Fatalf("missing output_item.done: %s", events)
	}
	item, _ := itemDone["item"].(map[string]any)
	if item["type"] != "custom_tool_call" || item["input"] != code || item["call_id"] != "call_1" {
		t.Fatalf("item done not restored: %v", item)
	}
	if _, has := item["arguments"]; has {
		t.Fatalf("item done kept arguments: %v", item)
	}
}

func TestPassthroughCustomEmptyInputEmitsNoDeltas(t *testing.T) {
	rt := passthroughRegistry(t, shared.RespTool{Type: "custom", Name: "exec"})
	wrapped := shared.WrapCustomToolInput("")
	raw := frame("response.output_item.added", itemAddedPayload(0, outboundFunctionCall("fc_2", "call_2", "exec", ""))) +
		frame("response.function_call_arguments.delta", argsDeltaPayload("fc_2", 0, 1, wrapped)) +
		frame("response.function_call_arguments.done", argsDonePayload("fc_2", 0, "exec", wrapped)) +
		frame("response.output_item.done", itemDonePayload(0, outboundFunctionCall("fc_2", "call_2", "exec", wrapped)))

	events, _, eErr := feedChunks(t, NewStreamConverter("openai-response", rt), perByte(raw))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	for _, ev := range events {
		event, _ := parseSSE(t, ev)
		switch event {
		case "response.function_call_arguments.delta":
			t.Fatalf("empty custom input leaked a function delta: %s", ev)
		case "response.custom_tool_call_input.delta":
			t.Fatalf("empty custom input produced a delta: %v", eventPayload(t, ev))
		case "response.custom_tool_call_input.done":
			if payload := eventPayload(t, ev); payload["input"] != "" {
				t.Fatalf("empty custom input done = %v", payload)
			}
		case "response.output_item.done":
			item, _ := eventPayload(t, ev)["item"].(map[string]any)
			if item["type"] != "custom_tool_call" || item["input"] != "" {
				t.Fatalf("empty custom item done = %v", item)
			}
		}
	}
}

func TestPassthroughCustomJSONLikeInputRoundTrip(t *testing.T) {
	rt := passthroughRegistry(t, shared.RespTool{Type: "custom", Name: "exec"})
	code := `{"a":1}`
	wrapped := shared.WrapCustomToolInput(code)
	half := len(wrapped) / 2
	raw := frame("response.output_item.added", itemAddedPayload(0, outboundFunctionCall("fc_3", "call_3", "exec", ""))) +
		frame("response.function_call_arguments.delta", argsDeltaPayload("fc_3", 0, 1, wrapped[:half])) +
		frame("response.function_call_arguments.delta", argsDeltaPayload("fc_3", 0, 2, wrapped[half:])) +
		frame("response.function_call_arguments.done", argsDonePayload("fc_3", 0, "exec", wrapped))

	events, _, eErr := feedChunks(t, NewStreamConverter("openai-response", rt), perByte(raw))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	var deltas []string
	var doneInput string
	for _, ev := range events {
		event, _ := parseSSE(t, ev)
		switch event {
		case "response.custom_tool_call_input.delta":
			delta, _ := eventPayload(t, ev)["delta"].(string)
			deltas = append(deltas, delta)
		case "response.custom_tool_call_input.done":
			doneInput, _ = eventPayload(t, ev)["input"].(string)
		}
	}
	if got := strings.Join(deltas, ""); got != code {
		t.Fatalf("JSON-like deltas = %q, want %q", got, code)
	}
	if doneInput != code {
		t.Fatalf("JSON-like done input = %q, want %q", doneInput, code)
	}
}

func TestPassthroughNamespacedItemsRestored(t *testing.T) {
	rt := namespaceRegistry(t)
	raw := frame("response.output_item.done", itemDonePayload(0,
		outboundFunctionCall("fc_1", "c1", "subagents__spawn_agent", `{"a":1}`))) +
		frame("response.output_item.done", itemDonePayload(1,
			outboundFunctionCall("fc_2", "c2", "subagents__custom_sub", shared.WrapCustomToolInput("spawn()"))))

	events, _, eErr := feedChunks(t, NewStreamConverter("openai-response", rt), perByte(raw))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d: %s", len(events), events)
	}
	fn, _ := eventPayload(t, events[0])["item"].(map[string]any)
	if fn["type"] != "function_call" || fn["name"] != "spawn_agent" || fn["namespace"] != "subagents" {
		t.Fatalf("namespaced function item not restored: %v", fn)
	}
	if fn["arguments"] != `{"a":1}` || fn["call_id"] != "c1" {
		t.Fatalf("namespaced function payload changed: %v", fn)
	}
	custom, _ := eventPayload(t, events[1])["item"].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "custom_sub" ||
		custom["namespace"] != "subagents" || custom["input"] != "spawn()" {
		t.Fatalf("namespaced custom item not restored: %v", custom)
	}
	if _, has := custom["arguments"]; has {
		t.Fatalf("namespaced custom item kept arguments: %v", custom)
	}
}

func TestPassthroughNonCustomItemsStayVerbatim(t *testing.T) {
	rt := namespaceRegistry(t)
	sc := NewStreamConverter("openai-response", rt)
	plain := frame("response.output_item.done", itemDonePayload(0, outboundFunctionCall("", "c7", "lookup", "{}")))
	events, _, eErr := feedChunks(t, sc, perByte(plain))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if len(events) != 1 || string(events[0]) != plain {
		t.Fatalf("declared plain item not verbatim:\n got %q\nwant %q", events[0], plain)
	}

	foreign := frame("response.output_item.done", encodeJSON(t, map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "function_call", "call_id": "c8", "name": "foreign",
			"namespace": "ext", "arguments": "{}"},
	}))
	events, _, eErr = feedChunks(t, sc, perByte(foreign))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if len(events) != 1 || string(events[0]) != foreign {
		t.Fatalf("unknown item not verbatim:\n got %q\nwant %q", events[0], foreign)
	}

	delta := frame("response.function_call_arguments.delta", argsDeltaPayload("fc9", 0, 1, `{"a":`))
	events, _, eErr = feedChunks(t, sc, perByte(delta))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if len(events) != 1 || string(events[0]) != delta {
		t.Fatalf("non-custom delta not verbatim:\n got %q\nwant %q", events[0], delta)
	}
}

func TestPassthroughGPTNativeStaysUntouched(t *testing.T) {
	// A GPT target keeps the client's own native bodies: BuildRequest only
	// rewrites the model id, so no tool identity is ever registered and the
	// converter must forward every frame — custom_tool_call items, their
	// custom input deltas and the terminal response — byte-identically.
	sc := NewStreamConverter("openai-response")
	added := frame("response.output_item.added", encodeJSON(t, map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1",
			"name": "exec", "input": ""},
	}))
	delta := frame("response.custom_tool_call_input.delta",
		`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"ls"}`)
	done := frame("response.output_item.done", encodeJSON(t, map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1",
			"name": "exec", "input": "ls"},
	}))
	completed := encodeJSON(t, map[string]any{
		"type": "response.completed",
		"response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "ls"},
		}},
	})
	want := []string{added, delta, done, frame("response.completed", completed)}
	events, doneSeen, eErr := feedChunks(t, sc, perByte(strings.Join(want, "")))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if !doneSeen {
		t.Fatalf("stream not terminal")
	}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i := range want {
		if string(events[i]) != want[i] {
			t.Fatalf("frame %d not verbatim:\n got %q\nwant %q", i, events[i], want[i])
		}
	}
}

func TestPassthroughCompletedOutputRestored(t *testing.T) {
	rt := namespaceRegistry(t)
	message := map[string]any{"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "done"}}}
	completed := encodeJSON(t, map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_9", "status": "completed", "model": "m",
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 4},
			"output": []any{
				map[string]any{"type": "function_call", "call_id": "c1",
					"name": "subagents__spawn_agent", "arguments": `{"a":1}`},
				map[string]any{"type": "function_call", "call_id": "c2",
					"name": "subagents__custom_sub", "arguments": shared.WrapCustomToolInput("spawn()")},
				message,
			},
		},
	})
	events, done, eErr := feedChunks(t, NewStreamConverter("openai-response", rt),
		perByte(frame("response.completed", completed)))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if !done || len(events) != 1 {
		t.Fatalf("done=%v events=%d", done, len(events))
	}
	payload := eventPayload(t, events[0])
	if payload["type"] != "response.completed" {
		t.Fatalf("terminal type = %v", payload["type"])
	}
	resp, _ := payload["response"].(map[string]any)
	if resp["id"] != "resp_9" || resp["status"] != "completed" {
		t.Fatalf("terminal identity changed: %v", resp)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage changed: %v", resp["usage"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %v", output)
	}
	fn, _ := output[0].(map[string]any)
	if fn["type"] != "function_call" || fn["name"] != "spawn_agent" || fn["namespace"] != "subagents" {
		t.Fatalf("terminal namespaced function not restored: %v", fn)
	}
	custom, _ := output[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "custom_sub" ||
		custom["namespace"] != "subagents" || custom["input"] != "spawn()" {
		t.Fatalf("terminal custom item not restored: %v", custom)
	}
	if _, has := custom["arguments"]; has {
		t.Fatalf("terminal custom item kept arguments: %v", custom)
	}
	if got, want := canonicalJSON(t, output[2]), canonicalJSON(t, message); got != want {
		t.Fatalf("terminal message changed:\n got %s\nwant %s", got, want)
	}

	plain := frame("response.completed", `{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`)
	events, _, eErr = feedChunks(t, NewStreamConverter("openai-response", rt), perByte(plain))
	if eErr != nil {
		t.Fatalf("stream error: %+v", eErr)
	}
	if len(events) != 1 || string(events[0]) != plain {
		t.Fatalf("plain terminal not verbatim:\n got %q\nwant %q", events[0], plain)
	}
}

func TestConvertPassthroughNamespaceAndCustomRestored(t *testing.T) {
	rt := namespaceRegistry(t)
	body, err := json.Marshal(map[string]any{
		"id": "resp_1", "status": "completed", "model": "m",
		"output": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "subagents__spawn_agent", "arguments": `{"a":1}`},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "subagents__custom_sub",
				"arguments": shared.WrapCustomToolInput("spawn()")},
			map[string]any{"type": "function_call", "call_id": "c3", "name": "foreign", "namespace": "ext", "arguments": "{}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, eErr := ConvertNonStreamResponse("openai-response", 200, body, rt)
	if eErr != nil {
		t.Fatalf("convert: %+v", eErr)
	}
	var doc struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(doc.Output) != 3 {
		t.Fatalf("output = %v", doc.Output)
	}
	fn := doc.Output[0]
	if fn["type"] != "function_call" || fn["name"] != "spawn_agent" || fn["namespace"] != "subagents" ||
		fn["arguments"] != `{"a":1}` {
		t.Fatalf("namespaced function restored wrong: %v", fn)
	}
	custom := doc.Output[1]
	if custom["type"] != "custom_tool_call" || custom["name"] != "custom_sub" ||
		custom["namespace"] != "subagents" || custom["input"] != "spawn()" {
		t.Fatalf("namespaced custom restored wrong: %v", custom)
	}
	if _, has := custom["arguments"]; has {
		t.Fatalf("namespaced custom kept arguments: %v", custom)
	}
	foreign := doc.Output[2]
	if foreign["name"] != "foreign" || foreign["namespace"] != "ext" || foreign["arguments"] != "{}" {
		t.Fatalf("unknown item changed: %v", foreign)
	}
}

func TestConvertPassthroughCustomEmptyAndJSONLikeInput(t *testing.T) {
	rt := passthroughRegistry(t, shared.RespTool{Type: "custom", Name: "exec"})
	for _, code := range []string{"", `{"a":1}`} {
		body, err := json.Marshal(map[string]any{
			"id": "resp_x", "status": "completed", "model": "m",
			"output": []any{
				map[string]any{"type": "function_call", "call_id": "c1", "name": "exec",
					"arguments": shared.WrapCustomToolInput(code)},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, eErr := ConvertNonStreamResponse("openai-response", 200, body, rt)
		if eErr != nil {
			t.Fatalf("convert: %+v", eErr)
		}
		var doc struct {
			Output []map[string]any `json:"output"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		item := doc.Output[0]
		if item["type"] != "custom_tool_call" || item["input"] != code || item["call_id"] != "c1" {
			t.Fatalf("code %q restored as %v", code, item)
		}
		if _, has := item["arguments"]; has {
			t.Fatalf("code %q kept arguments: %v", code, item)
		}
	}
}
