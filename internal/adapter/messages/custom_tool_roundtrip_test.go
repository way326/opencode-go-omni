package messages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
)

// itemInput reads a rendered custom_tool_call input. RespItem.Input
// carries omitempty, so a custom call with empty code ships no "input"
// key at all rather than an explicit empty string.
func itemInput(t *testing.T, item map[string]any) string {
	t.Helper()
	raw, ok := item["input"]
	if !ok {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("custom input is not a string: %v", item)
	}
	return s
}

// msgFrame renders one Anthropic SSE frame carrying a JSON payload.
func msgFrame(event string, payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

// chunkEvery splits raw stream text into fixed-size pieces so transport
// reassembly (partial-line buffering) is exercised alongside the
// conversion logic.
func chunkEvery(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		end := n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[:end])
		s = s[end:]
	}
	return out
}

// runeChunks splits s into pieces of at most n runes, never inside a rune.
func runeChunks(s string, n int) []string {
	var out []string
	var b strings.Builder
	count := 0
	for _, r := range s {
		b.WriteRune(r)
		count++
		if count == n {
			out = append(out, b.String())
			b.Reset()
			count = 0
		}
	}
	if count > 0 {
		out = append(out, b.String())
	}
	return out
}

func completedResponse(t *testing.T, byName map[string][]map[string]any) map[string]any {
	t.Helper()
	completed := byName["response.completed"]
	if len(completed) != 1 {
		t.Fatalf("response.completed events = %v", completed)
	}
	resp, ok := completed[0]["response"].(map[string]any)
	if !ok {
		t.Fatalf("completed payload has no response object: %v", completed[0])
	}
	return resp
}

func renderedOutput(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("completed response output = %v", resp["output"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("output item is not an object: %v", item)
		}
		out = append(out, m)
	}
	return out
}

// TestCustomToolMessagesWireRoundTrip pins the full Code Mode round trip
// on the Messages route: a Responses request declaring a freeform custom
// tool inside a namespace must reach the upstream as a function with the
// required string input object, and the upstream's tool_use reply must
// return to the client as the original custom_tool_call items — name,
// namespace, call ids, receipts, empty code, and JSON-looking code
// intact.
func TestCustomToolMessagesWireRoundTrip(t *testing.T) {
	const (
		codeUnicode = "const s = \"中文\";\r\ntext(s);"
		codeJSONish = `{"input":"not a wrapper","n":1}`
	)
	body, _ := json.Marshal(map[string]any{
		"max_output_tokens": 512,
		"tools": []any{
			map[string]any{"type": "namespace", "name": "functions", "tools": []any{
				map[string]any{"type": "custom", "name": "exec", "description": "Run raw code."},
			}},
			map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run the script"},
			map[string]any{"type": "custom_tool_call", "call_id": "call_a", "name": "exec", "namespace": "functions", "input": codeUnicode},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_a", "output": "receipt A"},
			map[string]any{"type": "custom_tool_call", "call_id": "call_b", "name": "exec", "namespace": "functions", "input": codeJSONish},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_b", "output": []any{map[string]any{"type": "input_text", "text": "receipt B"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "call_c", "name": "exec", "namespace": "functions", "input": ""},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_c", "output": "receipt C"},
		},
	})
	rt := shared.NewResponseTools()
	out, eErr := BuildRequest("claude-target", "openai-response", body, nil, rt)
	if eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	req := decodeReq(t, out)

	// Tool declarations: the custom child unrolls to its qualified wire
	// name carrying the required string input schema; the plain function
	// tool keeps its name and schema.
	tools := map[string]map[string]any{}
	for _, raw := range req["tools"].([]any) {
		tm := raw.(map[string]any)
		tools[tm["name"].(string)] = tm
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %v", req["tools"])
	}
	exec, ok := tools["functions__exec"]
	if !ok {
		t.Fatalf("custom child not unrolled: %v", tools)
	}
	schema, ok := exec["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("custom schema missing: %v", exec)
	}
	props, _ := schema["properties"].(map[string]any)
	inputProp, _ := props["input"].(map[string]any)
	required, _ := schema["required"].([]any)
	if schema["type"] != "object" || inputProp["type"] != "string" || len(props) != 1 ||
		len(required) != 1 || required[0] != "input" || schema["additionalProperties"] != false {
		t.Fatalf("custom input schema = %v", schema)
	}
	if desc, _ := exec["description"].(string); !strings.Contains(desc, "input") {
		t.Fatalf("wrapper contract missing from description: %q", desc)
	}
	if _, ok := tools["functions__read"]; ok {
		t.Fatalf("plain function acquired a namespace: %v", tools)
	}
	plain, ok := tools["read"]
	if !ok {
		t.Fatalf("plain function tool missing: %v", tools)
	}
	if ps, _ := plain["input_schema"].(map[string]any); ps["type"] != "object" {
		t.Fatalf("plain function schema changed: %v", plain)
	}

	// History: every custom call arrives as a tool_use block whose input
	// object carries the raw code under the required "input" key, and
	// every receipt arrives as a tool_result with the same call id. Tool
	// results alternate with calls, so each lands in its own message.
	msgs := req["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("messages = %v", msgs)
	}
	roles := []string{"user", "assistant", "user", "assistant", "user", "assistant", "user"}
	var toolUses, receipts []map[string]any
	for i, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] != roles[i] {
			t.Fatalf("message %d role = %v, want %q", i, m["role"], roles[i])
		}
		content, ok := m["content"].([]any)
		if !ok {
			continue // single text block collapses to a string
		}
		for _, braw := range content {
			blk := braw.(map[string]any)
			switch blk["type"] {
			case "tool_use":
				toolUses = append(toolUses, blk)
			case "tool_result":
				receipts = append(receipts, blk)
			}
		}
	}
	if len(toolUses) != 3 || len(receipts) != 3 {
		t.Fatalf("tool blocks = %v / %v", toolUses, receipts)
	}
	for i, want := range []struct{ id, code string }{
		{"call_a", codeUnicode}, {"call_b", codeJSONish}, {"call_c", ""},
	} {
		blk := toolUses[i]
		if blk["type"] != "tool_use" || blk["id"] != want.id || blk["name"] != "functions__exec" {
			t.Fatalf("block %d = %v", i, blk)
		}
		in, _ := blk["input"].(map[string]any)
		if len(in) != 1 || in["input"] != want.code {
			t.Fatalf("block %d input = %v, want %q", i, blk["input"], want.code)
		}
	}
	for i, want := range []struct{ id, text string }{
		{"call_a", "receipt A"}, {"call_b", "receipt B"}, {"call_c", "receipt C"},
	} {
		blk := receipts[i]
		if blk["type"] != "tool_result" || blk["tool_use_id"] != want.id || blk["content"] != want.text {
			t.Fatalf("receipt %d = %v", i, blk)
		}
	}

	// Return leg: the upstream Messages reply restores every call to its
	// original custom identity with exact input text.
	upstream, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-target",
		"content": []any{
			map[string]any{"type": "tool_use", "id": "call_a", "name": "functions__exec", "input": map[string]any{"input": codeUnicode}},
			map[string]any{"type": "tool_use", "id": "call_b", "name": "functions__exec", "input": map[string]any{"input": codeJSONish}},
			map[string]any{"type": "tool_use", "id": "call_c", "name": "functions__exec", "input": map[string]any{"input": ""}},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 11, "output_tokens": 7},
	})
	resBody, eErr := ConvertNonStreamResponse("openai-response", 200, upstream, rt)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	var res struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(resBody, &res); err != nil {
		t.Fatalf("decode result: %v (%s)", err, resBody)
	}
	if res.Status != "completed" || len(res.Output) != 3 {
		t.Fatalf("result = %s", resBody)
	}
	for i, want := range []struct{ id, code string }{
		{"call_a", codeUnicode}, {"call_b", codeJSONish}, {"call_c", ""},
	} {
		item := res.Output[i]
		if item["type"] != "custom_tool_call" || item["call_id"] != want.id ||
			item["name"] != "exec" || item["namespace"] != "functions" {
			t.Fatalf("output %d = %v", i, item)
		}
		if _, has := item["arguments"]; has {
			t.Fatalf("output %d kept function arguments: %v", i, item)
		}
		if got := itemInput(t, item); got != want.code {
			t.Fatalf("output %d input = %q, want %q", i, got, want.code)
		}
	}
}

// TestCustomToolMessagesStreamRoundTrip pins the streaming leg: the
// {"input":...} envelope is fragmented across deltas (including inside
// JSON escape sequences) and across transport chunks, yet only unwrapped
// input reaches the client, deltas reconstruct the raw code exactly, and
// the terminal items restore the original custom identity.
func TestCustomToolMessagesStreamRoundTrip(t *testing.T) {
	const code = `const a = {"input":"decoy"};` + "\r\n" + `text("中文 \ ok");` + "\n"
	rt := shared.NewResponseTools()
	decl, _ := json.Marshal(map[string]any{
		"input": "run",
		"tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "Run raw code."},
			map[string]any{"type": "namespace", "name": "functions", "tools": []any{
				map[string]any{"type": "custom", "name": "run"},
			}},
			map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}},
		},
	})
	if _, eErr := BuildRequest("m", "openai-response", decl, nil, rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}

	wrapper := shared.WrapCustomToolInput(code)
	// Split right after the backslash of the first escape sequence so no
	// fragment is individually valid JSON; the accumulator still
	// reconstructs the wrapper exactly.
	cut := strings.Index(wrapper, "\\\"") + 1
	if cut <= 0 || cut >= len(wrapper) {
		t.Fatalf("wrapper has no escape to split: %q", wrapper)
	}
	frags := append([]string{wrapper[:cut]}, runeChunks(wrapper[cut:], 4)...)
	if strings.Join(frags, "") != wrapper || !strings.HasSuffix(frags[0], `\`) {
		t.Fatalf("fragmentation not exercised: %q", frags)
	}

	var stream strings.Builder
	stream.WriteString(msgFrame("message_start", map[string]any{
		"type":    "message_start",
		"message": map[string]any{"id": "msg_1", "model": "m", "usage": map[string]any{"input_tokens": 3}},
	}))
	stream.WriteString(msgFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "call_1", "name": "exec"},
	}))
	for _, frag := range frags {
		stream.WriteString(msgFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
		}))
	}
	stream.WriteString(msgFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
	stream.WriteString(msgFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 1,
		"content_block": map[string]any{"type": "tool_use", "id": "call_2", "name": "functions__run"},
	}))
	stream.WriteString(msgFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"input":""}`},
	}))
	stream.WriteString(msgFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}))
	stream.WriteString(msgFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 2,
		"content_block": map[string]any{"type": "tool_use", "id": "call_3", "name": "read"},
	}))
	stream.WriteString(msgFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"x"}`},
	}))
	stream.WriteString(msgFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2}))
	stream.WriteString(msgFrame("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"},
		"usage": map[string]any{"output_tokens": 4},
	}))
	stream.WriteString(msgFrame("message_stop", map[string]any{"type": "message_stop"}))

	sc := NewStreamConverter("openai-response", rt)
	events, done, eErr := feed(t, sc, chunkEvery(stream.String(), 7)...)
	if eErr != nil || !done {
		t.Fatalf("done=%v err=%v", done, eErr)
	}
	byName := namedEvents(t, events)

	added := byName["response.output_item.added"]
	if len(added) != 3 {
		t.Fatalf("output_item.added = %v", added)
	}
	first := added[0]["item"].(map[string]any)
	if first["type"] != "custom_tool_call" || first["name"] != "exec" ||
		first["call_id"] != "call_1" || first["input"] != "" {
		t.Fatalf("custom announcement = %v", first)
	}
	if _, has := first["arguments"]; has {
		t.Fatalf("announcement kept function arguments: %v", first)
	}
	if _, has := first["namespace"]; has {
		t.Fatalf("top-level custom gained a namespace: %v", first)
	}
	second := added[1]["item"].(map[string]any)
	if second["type"] != "custom_tool_call" || second["name"] != "run" || second["namespace"] != "functions" {
		t.Fatalf("namespaced custom announcement = %v", second)
	}
	third := added[2]["item"].(map[string]any)
	if third["type"] != "function_call" || third["name"] != "read" || third["arguments"] != "" {
		t.Fatalf("plain function announcement = %v", third)
	}

	var got strings.Builder
	for _, ev := range byName["response.custom_tool_call_input.delta"] {
		if ev["item_id"] != "call_1" || ev["output_index"] != float64(0) {
			t.Fatalf("custom delta targets %v", ev)
		}
		got.WriteString(ev["delta"].(string))
	}
	if got.String() != code {
		t.Fatalf("custom deltas = %q, want %q", got.String(), code)
	}
	inputDone := byName["response.custom_tool_call_input.done"]
	if len(inputDone) != 2 {
		t.Fatalf("custom input.done = %v", inputDone)
	}
	if inputDone[0]["item_id"] != "call_1" || inputDone[0]["input"] != code {
		t.Fatalf("first input.done = %v", inputDone[0])
	}
	if inputDone[1]["item_id"] != "call_2" || inputDone[1]["input"] != "" {
		t.Fatalf("empty input.done = %v", inputDone[1])
	}

	var args strings.Builder
	for _, ev := range byName["response.function_call_arguments.delta"] {
		args.WriteString(ev["delta"].(string))
	}
	if args.String() != `{"path":"x"}` {
		t.Fatalf("plain function deltas = %q", args.String())
	}
	argsDone := byName["response.function_call_arguments.done"]
	if len(argsDone) != 1 || argsDone[0]["name"] != "read" || argsDone[0]["arguments"] != `{"path":"x"}` {
		t.Fatalf("plain function arguments.done = %v", argsDone)
	}

	resp := completedResponse(t, byName)
	if resp["status"] != "completed" {
		t.Fatalf("status = %v", resp["status"])
	}
	items := renderedOutput(t, resp)
	if len(items) != 3 {
		t.Fatalf("terminal output = %v", items)
	}
	if items[0]["type"] != "custom_tool_call" || items[0]["call_id"] != "call_1" ||
		items[0]["name"] != "exec" || itemInput(t, items[0]) != code {
		t.Fatalf("terminal custom item = %v", items[0])
	}
	if items[1]["type"] != "custom_tool_call" || items[1]["name"] != "run" ||
		items[1]["namespace"] != "functions" || itemInput(t, items[1]) != "" {
		t.Fatalf("terminal empty custom item = %v", items[1])
	}
	if items[2]["type"] != "function_call" || items[2]["name"] != "read" || items[2]["arguments"] != `{"path":"x"}` {
		t.Fatalf("terminal plain item = %v", items[2])
	}
	itemDone := byName["response.output_item.done"]
	if len(itemDone) != 3 || itemDone[0]["item"].(map[string]any)["call_id"] != "call_1" ||
		itemInput(t, itemDone[0]["item"].(map[string]any)) != code {
		t.Fatalf("output_item.done = %v", itemDone)
	}
}

// TestCustomToolMessagesFlushTerminalRestoresInput pins the deferred
// terminal: an upstream close without message_stop still reports the
// custom tool input to the client.
func TestCustomToolMessagesFlushTerminalRestoresInput(t *testing.T) {
	const code = "text('flush');"
	rt := shared.NewResponseTools()
	decl, _ := json.Marshal(map[string]any{
		"input": "run",
		"tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Run raw code."}},
	})
	if _, eErr := BuildRequest("m", "openai-response", decl, nil, rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	sc := NewStreamConverter("openai-response", rt)
	events, done, eErr := feed(t, sc,
		msgFrame("message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": "msg_1", "model": "m", "usage": map[string]any{"input_tokens": 1}},
		}),
		msgFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "call_f", "name": "exec"},
		}),
		msgFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": shared.WrapCustomToolInput(code)},
		}),
	)
	if eErr != nil || done {
		t.Fatalf("done=%v err=%v", done, eErr)
	}
	events = append(events, sc.Flush()...)
	byName := namedEvents(t, events)
	inputDone := byName["response.custom_tool_call_input.done"]
	if len(inputDone) != 1 || inputDone[0]["input"] != code {
		t.Fatalf("flushed input.done = %v", inputDone)
	}
	items := renderedOutput(t, completedResponse(t, byName))
	if len(items) != 1 || items[0]["type"] != "custom_tool_call" || itemInput(t, items[0]) != code {
		t.Fatalf("flushed terminal output = %v", items)
	}
}

// TestCustomToolMessagesUnwrappedUpstreamInputPassesThrough pins the
// fallback for an upstream that ignores the required wrapper: input that
// is not a {"input":...} envelope passes through verbatim (shared policy,
// identical on every route) instead of being dropped or double-wrapped,
// so an empty object arrives as literal "{}" code rather than empty code.
func TestCustomToolMessagesUnwrappedUpstreamInputPassesThrough(t *testing.T) {
	rt := shared.NewResponseTools()
	decl, _ := json.Marshal(map[string]any{
		"input": "run",
		"tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Run raw code."}},
	})
	if _, eErr := BuildRequest("m", "openai-response", decl, nil, rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	upstream := `{"id":"m","model":"m","content":[{"type":"tool_use","id":"call_x","name":"exec","input":{}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	out, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(upstream), rt)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse: %v", eErr)
	}
	var res struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode result: %v (%s)", err, out)
	}
	if len(res.Output) != 1 || res.Output[0]["type"] != "custom_tool_call" ||
		res.Output[0]["name"] != "exec" || itemInput(t, res.Output[0]) != "{}" {
		t.Fatalf("passthrough output = %v", res.Output)
	}
}
