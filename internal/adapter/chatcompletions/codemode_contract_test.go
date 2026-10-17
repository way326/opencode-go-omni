package chatcompletions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
)

// The Code Mode contract is protocol-level, not model-level: every upstream
// model served through this route (DeepSeek and the remaining OpenCode Go
// families alike) must see the same custom-tool surface.
var codeModeModels = []string{
	"deepseek-v4.1-flash",
	"deepseek-flash",
	"gemini-4-argon-contributor",
	"mimo-v2.6-flash",
	"muse-spark-1.3-contributor",
}

const codeModeInputSchemaJSON = `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`

// codeModeToolDecls declares one custom Code Mode tool next to one ordinary
// function tool so mixed turns exercise both identities in one request.
const codeModeToolDecls = `[{"type":"custom","name":"exec","description":"Run JavaScript"},` +
	`{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]`

func jsonMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not a JSON object: %v (%s)", err, raw)
	}
	return m
}

// decodeCustomArguments asserts the transport-level argument contract: the
// arguments are exactly one JSON object with one string "input" property
// carrying the raw custom text (no double wrapping, no escaping loss).
func decodeCustomArguments(t *testing.T, args, want string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("arguments are not a JSON object: %v (%q)", err, args)
	}
	if len(m) != 1 || m["input"] == nil {
		t.Fatalf("arguments must carry exactly the string input wrapper: %q", args)
	}
	var got string
	if err := json.Unmarshal(m["input"], &got); err != nil {
		t.Fatalf("input value is not a string: %v (%q)", err, args)
	}
	if got != want {
		t.Fatalf("input round trip changed the text: got %q want %q", got, want)
	}
}

func codeModeBody(t *testing.T, items []any, tools string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"input": items, "tools": json.RawMessage(tools)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCodeModeCustomSchemaAcrossModels(t *testing.T) {
	want := jsonMap(t, codeModeInputSchemaJSON)
	for _, model := range codeModeModels {
		t.Run(model, func(t *testing.T) {
			body := `{"input":"hi","tools":[{"type":"namespace","name":"functions","tools":[` +
				`{"type":"custom","name":"exec","description":"Run JavaScript"}]}],` +
				`"tool_choice":{"type":"function","name":"exec","namespace":"functions"}}`
			out, eErr := BuildRequest(model, "openai-response", []byte(body), nil, shared.NewResponseTools())
			m := decodeOut(t, out, eErr)
			tools, ok := m["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Fatalf("tools = %v, want one flattened custom tool", m["tools"])
			}
			tool := tools[0].(map[string]any)
			if tool["type"] != "function" {
				t.Fatalf("custom tool was not rewritten as a function tool: %v", tool)
			}
			fn := tool["function"].(map[string]any)
			got, _ := json.Marshal(fn["parameters"])
			if !reflect.DeepEqual(jsonMap(t, string(got)), want) {
				t.Fatalf("input schema = %s, want %s", got, codeModeInputSchemaJSON)
			}
			desc, _ := fn["description"].(string)
			if !strings.Contains(desc, "one required string property named input") ||
				!strings.Contains(desc, "Preserve the raw tool text exactly inside input") {
				t.Fatalf("description does not explain the upstream wrapper: %q", desc)
			}
			name, _ := fn["name"].(string)
			if name == "exec" || !strings.HasSuffix(name, "exec") {
				t.Fatalf("namespaced custom tool was not flattened to a wire name: %q", name)
			}
			choice := m["tool_choice"].(map[string]any)["function"].(map[string]any)["name"]
			if choice != name {
				t.Fatalf("tool_choice name %v does not reference the declared wire name %q", choice, name)
			}
		})
	}
}

func TestCodeModeMixedHistoryPairing(t *testing.T) {
	code := "const a = 1;\ntext(a);"
	calls := func(reverse bool) []any {
		custom := map[string]any{"type": "custom_tool_call", "call_id": "call_c", "name": "exec", "input": code}
		plain := map[string]any{"type": "function_call", "call_id": "call_f", "name": "read", "arguments": `{"path":"x"}`}
		customOut := map[string]any{"type": "custom_tool_call_output", "call_id": "call_c", "output": "code-receipt"}
		plainOut := map[string]any{"type": "function_call_output", "call_id": "call_f", "output": "file-receipt"}
		if reverse {
			return []any{map[string]any{"type": "message", "role": "user", "content": "go"}, plain, custom, plainOut, customOut}
		}
		return []any{map[string]any{"type": "message", "role": "user", "content": "go"}, custom, plain, customOut, plainOut}
	}
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom-first", true: "function-first"}[reverse], func(t *testing.T) {
			out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", codeModeBody(t, calls(reverse), codeModeToolDecls), nil, shared.NewResponseTools())
			m := decodeOut(t, out, eErr)
			msgs, ok := m["messages"].([]any)
			if !ok || len(msgs) != 4 {
				t.Fatalf("want user + merged assistant + two tool messages, got %v", m["messages"])
			}
			asst := msgs[1].(map[string]any)
			if asst["role"] != "assistant" {
				t.Fatalf("message 1 role = %v, want the merged assistant turn", asst["role"])
			}
			toolCalls, ok := asst["tool_calls"].([]any)
			if !ok || len(toolCalls) != 2 {
				t.Fatalf("mixed turn must merge both calls: %v", asst["tool_calls"])
			}
			byID := map[string]map[string]any{}
			for _, raw := range toolCalls {
				call := raw.(map[string]any)
				byID[call["id"].(string)] = call
			}
			custom, plain := byID["call_c"], byID["call_f"]
			if custom == nil || plain == nil {
				t.Fatalf("call ids not preserved across the merge: %v", toolCalls)
			}
			cFn, pFn := custom["function"].(map[string]any), plain["function"].(map[string]any)
			if cFn["name"] != "exec" {
				t.Fatalf("custom call name changed: %v", cFn)
			}
			decodeCustomArguments(t, cFn["arguments"].(string), code)
			if pFn["name"] != "read" || pFn["arguments"] != `{"path":"x"}` {
				t.Fatalf("ordinary function call changed: %v", pFn)
			}
			// Receipts keep the history's own order; the ids carry the pairing.
			// The merge must never reorder the calls inside the assistant turn.
			wantIDs := []string{"call_c", "call_f"}
			if reverse {
				wantIDs = []string{"call_f", "call_c"}
			}
			for index, id := range wantIDs {
				tool := msgs[2+index].(map[string]any)
				if tool["role"] != "tool" || tool["tool_call_id"] != id {
					t.Fatalf("receipt %d is not paired with %s: %v", index, id, tool)
				}
			}
			if toolCalls[0].(map[string]any)["id"] != wantIDs[0] || toolCalls[1].(map[string]any)["id"] != wantIDs[1] {
				t.Fatalf("merged call order changed: %v", toolCalls)
			}
			if msgs[2].(map[string]any)["content"] != map[bool]string{false: "code-receipt", true: "file-receipt"}[reverse] ||
				msgs[3].(map[string]any)["content"] != map[bool]string{false: "file-receipt", true: "code-receipt"}[reverse] {
				t.Fatalf("receipt order or text changed: %v %v", msgs[2], msgs[3])
			}
		})
	}
}

// A custom call replayed from history must restore as custom_tool_call with the
// exact text even when the current request no longer declares the tool.
func TestCodeModeHistoryWithoutDeclarationRestoresIdentity(t *testing.T) {
	rt := shared.NewResponseTools()
	items := []any{
		map[string]any{"type": "custom_tool_call", "call_id": "call_old", "name": "exec", "namespace": "functions", "input": "text(1)"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_old", "output": "1"},
	}
	out, eErr := BuildRequest("deepseek-flash", "openai-response", codeModeBody(t, items, `[]`), nil, rt)
	m := decodeOut(t, out, eErr)
	call := m["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	wireName := call["function"].(map[string]any)["name"].(string)
	if wireName == "exec" || call["id"] != "call_old" {
		t.Fatalf("historical identity was not flattened with its id preserved: %v", call)
	}
	decodeCustomArguments(t, call["function"].(map[string]any)["arguments"].(string), "text(1)")
	upstream, _ := json.Marshal(map[string]any{"id": "resp_old", "model": "deepseek-flash", "choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": "call_old", "type": "function", "function": map[string]any{"name": wireName, "arguments": `{"input":"text(1)"}`},
		}}},
		"finish_reason": "tool_calls",
	}}})
	converted, eErr := ConvertNonStreamResponse("openai-response", 200, upstream, rt)
	resp := decodeOut(t, converted, eErr)
	output := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("want the single replayed call, got %v", resp["output"])
	}
	item := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "exec" || item["namespace"] != "functions" ||
		item["call_id"] != "call_old" || item["input"] != "text(1)" {
		t.Fatalf("wire name did not restore the original custom identity: %v", item)
	}
}

func TestCodeModeInputFidelityBothDirections(t *testing.T) {
	inputs := []string{
		"",
		`{"input":"literal JSON, not a wrapper"}`,
		"const s = \"中文 😀\";\r\ntext(s);\n",
		"</script> & \"quotes\" \\backslash\\ and {\"nested\":[1,2]}",
	}
	for index, input := range inputs {
		t.Run(fmt.Sprintf("%d/%s", index, strings.ReplaceAll(input, "/", "_")), func(t *testing.T) {
			items := []any{
				map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": input},
				map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "receipt"},
			}
			first, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", codeModeBody(t, items, codeModeToolDecls), nil, shared.NewResponseTools())
			wire := decodeOut(t, first, eErr)
			args := wire["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
			decodeCustomArguments(t, args, input)
			// Upstream echoes the same wire arguments back as a custom call.
			upstream, _ := json.Marshal(map[string]any{"id": "resp_1", "model": "deepseek-v4.1-flash", "choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function", "function": map[string]any{"name": "exec", "arguments": args},
				}}},
				"finish_reason": "tool_calls",
			}}})
			rt := shared.NewResponseTools()
			if _, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", codeModeBody(t, nil, codeModeToolDecls), nil, rt); eErr != nil {
				t.Fatal(eErr)
			}
			converted, eErr := ConvertNonStreamResponse("openai-response", 200, upstream, rt)
			resp := decodeOut(t, converted, eErr)
			returned := resp["output"].([]any)[0].(map[string]any)
			var rawItems struct {
				Output []map[string]json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(converted, &rawItems); err != nil {
				t.Fatal(err)
			}
			if len(rawItems.Output) != 1 || rawItems.Output[0]["input"] == nil {
				t.Fatalf("custom call must always carry the input field on the wire: %s", converted)
			}
			if returned["type"] != "custom_tool_call" || returned["name"] != "exec" || returned["call_id"] != "call_1" {
				t.Fatalf("custom identity changed on the return leg: %v", returned)
			}
			if got, _ := returned["input"].(string); got != input {
				t.Fatalf("returned input = %q want %q", got, input)
			}
			// Replaying the returned call must not wrap it twice.
			replay := []any{returned, map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "receipt"}}
			second, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", codeModeBody(t, replay, codeModeToolDecls), nil, shared.NewResponseTools())
			secondWire := decodeOut(t, second, eErr)
			replayArgs := secondWire["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
			decodeCustomArguments(t, replayArgs, input)
		})
	}
}

// Mixed custom and ordinary calls must keep every client-visible fragment
// exact, including \uXXXX escapes split across transport chunks.
func TestStreamCodeModeMixedCallsFragmentsExact(t *testing.T) {
	code := "const s = \"中文\";\r\ntext(\"input\");\n"
	wrapped := `{"input":"const s = \"\u4e2d\u6587\";\r\ntext(\"input\");\n"}`
	fnArgs := `{"path":"a b.txt","limit":5}`
	rt := shared.NewResponseTools()
	if _, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", codeModeBody(t, nil, codeModeToolDecls), nil, rt); eErr != nil {
		t.Fatal(eErr)
	}
	sc := NewStreamConverter("openai-response", rt)
	var events [][]byte
	var seq int
	feed := func(payload map[string]any) {
		t.Helper()
		seq++
		payload["id"], payload["model"] = "resp_1", "deepseek-v4.1-flash"
		b, _ := json.Marshal(payload)
		b = append(append([]byte("data: "), b...), '\n', '\n')
		// Split the transport arbitrarily inside both SSE frames and payloads.
		for i := 0; i < len(b); i += 7 {
			end := min(i+7, len(b))
			evs, _, err := sc.Feed(b[i:end])
			if err != nil {
				t.Fatalf("feed %d: %v", seq, err)
			}
			events = append(events, evs...)
		}
	}
	chunk := func(calls ...map[string]any) map[string]any {
		return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}}}}
	}
	fragment := func(index int, args string) map[string]any {
		return map[string]any{"index": index, "function": map[string]any{"arguments": args}}
	}
	feed(chunk(map[string]any{"index": 0, "id": "call_c", "function": map[string]any{"name": "exec", "arguments": ""}}))
	feed(chunk(map[string]any{"index": 1, "id": "call_f", "function": map[string]any{"name": "read", "arguments": ""}}))
	for i, j := 0, 0; i < len(wrapped) || j < len(fnArgs); {
		var batch []map[string]any
		if i < len(wrapped) {
			batch, i = append(batch, fragment(0, wrapped[i:i+1])), i+1
		}
		if j < len(fnArgs) {
			batch, j = append(batch, fragment(1, fnArgs[j:j+1])), j+1
		}
		feed(chunk(batch...))
	}
	feed(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	feed(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 4}})

	var customDeltas, functionDeltas strings.Builder
	added := map[int]map[string]any{}
	var inputDone, argsDone, completed map[string]any
	for _, ev := range parseEvents(t, events) {
		switch ev.Name {
		case "response.custom_tool_call_input.delta":
			if ev.Data["item_id"] != "call_c" {
				t.Fatalf("custom delta carries the wrong item id: %v", ev)
			}
			customDeltas.WriteString(ev.Data["delta"].(string))
		case "response.function_call_arguments.delta":
			if ev.Data["item_id"] != "call_f" {
				t.Fatalf("ordinary function delta carries the wrong item id: %v", ev)
			}
			functionDeltas.WriteString(ev.Data["delta"].(string))
		case "response.output_item.added":
			added[int(ev.Data["output_index"].(float64))] = ev.Data["item"].(map[string]any)
		case "response.custom_tool_call_input.done":
			inputDone = ev.Data
		case "response.function_call_arguments.done":
			argsDone = ev.Data
		case "response.completed":
			completed = ev.Data
		}
	}
	if customDeltas.String() != code || functionDeltas.String() != fnArgs {
		t.Fatalf("streamed fragments changed: custom=%q function=%q", customDeltas.String(), functionDeltas.String())
	}
	if item := added[0]; item == nil || item["type"] != "custom_tool_call" || item["call_id"] != "call_c" || item["name"] != "exec" || item["input"] != "" {
		t.Fatalf("custom item announcement wrong: %v", added[0])
	}
	if item := added[1]; item == nil || item["type"] != "function_call" || item["call_id"] != "call_f" || item["name"] != "read" {
		t.Fatalf("function item announcement wrong: %v", added[1])
	}
	if inputDone == nil || inputDone["input"] != code || argsDone == nil || argsDone["arguments"] != fnArgs {
		t.Fatalf("terminal completions changed: input=%v args=%v", inputDone, argsDone)
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	response, ok := completed["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.completed carries no response object: %v", completed)
	}
	output, ok := response["output"].([]any)
	if !ok {
		t.Fatalf("response.completed carries no output array: %v", completed)
	}
	if len(output) != 2 {
		t.Fatalf("completed output = %v, want the two calls", output)
	}
	custom, plain := output[0].(map[string]any), output[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["input"] != code || custom["call_id"] != "call_c" {
		t.Fatalf("completed custom call changed: %v", custom)
	}
	if plain["type"] != "function_call" || plain["arguments"] != fnArgs || plain["call_id"] != "call_f" {
		t.Fatalf("completed function call changed: %v", plain)
	}
}

// The non-stream twin: a mixed turn returns both identities and replays with
// paired ids in one follow-up request.
func TestNonStreamCodeModeMixedCallsReplay(t *testing.T) {
	rt := shared.NewResponseTools()
	if _, eErr := BuildRequest("muse-spark-1.3-contributor", "openai-response", codeModeBody(t, nil, codeModeToolDecls), nil, rt); eErr != nil {
		t.Fatal(eErr)
	}
	upstream, _ := json.Marshal(map[string]any{"id": "resp_1", "model": "muse-spark-1.3-contributor", "choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": "working", "tool_calls": []any{
			map[string]any{"id": "call_c", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"input":"text(1)"}`}},
			map[string]any{"id": "call_f", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"x"}`}},
		}},
		"finish_reason": "tool_calls",
	}}})
	converted, eErr := ConvertNonStreamResponse("openai-response", 200, upstream, rt)
	resp := decodeOut(t, converted, eErr)
	output := resp["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %v, want message + both calls", output)
	}
	custom, plain := output[1].(map[string]any), output[2].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["input"] != "text(1)" || custom["call_id"] != "call_c" {
		t.Fatalf("custom call not restored: %v", custom)
	}
	if plain["type"] != "function_call" || plain["arguments"] != `{"path":"x"}` || plain["call_id"] != "call_f" {
		t.Fatalf("function call changed: %v", plain)
	}
	replay := []any{custom, plain,
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_c", "output": "1"},
		map[string]any{"type": "function_call_output", "call_id": "call_f", "output": "x"},
	}
	next, eErr := BuildRequest("muse-spark-1.3-contributor", "openai-response", codeModeBody(t, replay, codeModeToolDecls), nil, shared.NewResponseTools())
	wire := decodeOut(t, next, eErr)
	msgs := wire["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("replay messages = %v", msgs)
	}
	calls := msgs[0].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["id"] != "call_c" || calls[1].(map[string]any)["id"] != "call_f" {
		t.Fatalf("replayed ids not paired in order: %v", calls)
	}
	decodeCustomArguments(t, calls[0].(map[string]any)["function"].(map[string]any)["arguments"].(string), "text(1)")
	if calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != `{"path":"x"}` {
		t.Fatalf("replayed function arguments changed: %v", calls[1])
	}
	if msgs[1].(map[string]any)["tool_call_id"] != "call_c" || msgs[2].(map[string]any)["tool_call_id"] != "call_f" {
		t.Fatalf("replayed receipts not paired: %v %v", msgs[1], msgs[2])
	}
}
