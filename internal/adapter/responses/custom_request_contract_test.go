package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// ---- non-GPT normalized request contract (FR-005, AC §D) ----------------
//
// Code Mode pairs one custom tool declaration (a required string input)
// with history items that must replay through the very same wrapper:
// custom_tool_call becomes a function_call carrying
// {"input":"<code>"}, custom_tool_call_output becomes function_call_output
// with its call_id untouched, and namespaced history replays under the
// flattened wire name the tools list declares. These tests pin both halves
// of that pair, including empty and JSON-looking code.

const museModel = "opencode-go/muse-spark-1.3-contributor"

// normalizedCustomRequest builds a native Responses body with one custom
// tool declaration plus the supplied history items.
func normalizedCustomRequest(t *testing.T, items []any, tools []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": museModel,
		"input": items,
		"tools": tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func customExecTool() map[string]any {
	return map[string]any{"type": "custom", "name": "exec"}
}

func TestNormalizedRequestCustomSchemaAndHistoryPair(t *testing.T) {
	code := "console.log('hi');\n"
	body := normalizedCustomRequest(t, []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "run it"},
		}},
		map[string]any{"type": "custom_tool_call", "call_id": "call_c", "name": "exec", "input": code},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_c", "output": "ok"},
		map[string]any{"type": "function_call", "call_id": "call_f", "name": "lookup"},
		map[string]any{"type": "function_call_output", "call_id": "call_f", "output": "42"},
	}, []any{
		customExecTool(),
		map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
		}},
	})

	out, eErr := BuildRequest(museModel, "openai-response", body, nil, shared.NewResponseTools())
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	m := decodeReq(t, out)

	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v", m["tools"])
	}
	tool := itemMap(t, tools, 0)
	if tool["type"] != "function" || tool["name"] != "exec" {
		t.Fatalf("custom tool not rewritten: %v", tool)
	}
	params, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("tool parameters missing: %v", tool)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing: %v", params)
	}
	inputProp, ok := props["input"].(map[string]any)
	if !ok || inputProp["type"] != "string" {
		t.Fatalf("required string input property missing: %v", props)
	}
	required, _ := params["required"].([]any)
	if len(required) != 1 || required[0] != "input" {
		t.Fatalf("required = %v, want [input]", params["required"])
	}
	if params["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", params["additionalProperties"])
	}
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "required string property named input") {
		t.Fatalf("wrapper guidance missing from description: %q", desc)
	}

	items := inputItems(t, m)
	if len(items) != 5 {
		t.Fatalf("input items = %d, want 5", len(items))
	}
	call := itemMap(t, items, 1)
	if call["type"] != "function_call" || call["name"] != "exec" || call["call_id"] != "call_c" {
		t.Fatalf("custom history not converted: %v", call)
	}
	if _, has := call["input"]; has {
		t.Fatalf("custom history kept input: %v", call)
	}
	if _, has := call["namespace"]; has {
		t.Fatalf("custom history kept namespace: %v", call)
	}
	args, _ := call["arguments"].(string)
	var wrapped struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &wrapped); err != nil {
		t.Fatalf("arguments not a JSON object: %q (%v)", args, err)
	}
	if wrapped.Input != code {
		t.Fatalf("wrapped input = %q, want %q", wrapped.Input, code)
	}

	result := itemMap(t, items, 2)
	if result["type"] != "function_call_output" || result["call_id"] != "call_c" || result["output"] != "ok" {
		t.Fatalf("custom output not converted in place: %v", result)
	}

	plain := itemMap(t, items, 3)
	if plain["type"] != "function_call" || plain["name"] != "lookup" || plain["arguments"] != "{}" {
		t.Fatalf("plain function history not defaulted: %v", plain)
	}
	plainOut := itemMap(t, items, 4)
	if plainOut["type"] != "function_call_output" || plainOut["output"] != "42" {
		t.Fatalf("plain output rewritten: %v", plainOut)
	}
}

func TestNormalizedRequestCustomHistoryInputShapes(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
		want string
	}{
		{"empty", map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec", "input": ""}, ""},
		{"missing", map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec"}, ""},
		{"json-like", map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec", "input": `{"a":1}`}, `{"a":1}`},
		{"quotes-and-backslashes", map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec", "input": "say \"hi\"\\n"}, "say \"hi\"\\n"},
		{"crlf-unicode", map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec", "input": "行一\r\n行二"}, "行一\r\n行二"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := normalizedCustomRequest(t, []any{tc.item}, []any{customExecTool()})
			out, eErr := BuildRequest(museModel, "openai-response", body, nil, shared.NewResponseTools())
			if eErr != nil {
				t.Fatalf("BuildRequest: %+v", eErr)
			}
			call := itemMap(t, inputItems(t, decodeReq(t, out)), 0)
			args, _ := call["arguments"].(string)
			var wrapped map[string]string
			if err := json.Unmarshal([]byte(args), &wrapped); err != nil {
				t.Fatalf("arguments not a JSON string map: %q (%v)", args, err)
			}
			if wrapped["input"] != tc.want || len(wrapped) != 1 {
				t.Fatalf("wrapped = %v, want exactly {input:%q}", wrapped, tc.want)
			}
			if shared.UnwrapCustomToolInput(args) != tc.want {
				t.Fatalf("round trip = %q, want %q", shared.UnwrapCustomToolInput(args), tc.want)
			}
		})
	}
}

func TestNormalizedRequestCustomHistoryNonStringInputRejected(t *testing.T) {
	body := normalizedCustomRequest(t, []any{
		map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "exec",
			"input": map[string]any{"a": 1}},
	}, []any{customExecTool()})
	_, eErr := BuildRequest(museModel, "openai-response", body, nil, shared.NewResponseTools())
	wantErr(t, eErr, errclass.ClassTranslation)
}

func TestNormalizedRequestNamespacedHistoryFlattensAndRegisters(t *testing.T) {
	rt := shared.NewResponseTools()
	searchCall := map[string]any{
		"type": "web_search_call", "id": "ws_1", "status": "completed",
		"action": map[string]any{"type": "search", "query": "hi"},
	}
	body := normalizedCustomRequest(t, []any{
		map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "custom_sub",
			"namespace": "subagents", "input": "spawn()"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "done"},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "spawn_agent",
			"namespace": "subagents", "arguments": `{"a":1}`, "id": "fc_keep", "status": "completed"},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": "ok"},
		searchCall,
	}, []any{
		map[string]any{"type": "namespace", "name": "subagents", "tools": []any{
			map[string]any{"type": "custom", "name": "custom_sub"},
			map[string]any{"type": "function", "name": "spawn_agent", "parameters": map[string]any{"type": "object"}},
		}},
	})

	out, eErr := BuildRequest(museModel, "openai-response", body, nil, rt)
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	m := decodeReq(t, out)

	tools, _ := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("namespaced tools not unrolled: %v", m["tools"])
	}
	names := []string{}
	for i := range tools {
		name, _ := itemMap(t, tools, i)["name"].(string)
		names = append(names, name)
	}
	if names[0] != "subagents__custom_sub" || names[1] != "subagents__spawn_agent" {
		t.Fatalf("wire names = %v", names)
	}

	items := inputItems(t, m)
	if len(items) != 5 {
		t.Fatalf("input items = %d, want 5", len(items))
	}
	custom := itemMap(t, items, 0)
	if custom["type"] != "function_call" || custom["name"] != "subagents__custom_sub" ||
		custom["call_id"] != "c1" {
		t.Fatalf("namespaced custom history not flattened: %v", custom)
	}
	if _, has := custom["namespace"]; has {
		t.Fatalf("flattened history kept namespace: %v", custom)
	}
	if got := shared.UnwrapCustomToolInput(custom["arguments"].(string)); got != "spawn()" {
		t.Fatalf("namespaced custom input = %q", got)
	}

	fn := itemMap(t, items, 2)
	if fn["name"] != "subagents__spawn_agent" || fn["arguments"] != `{"a":1}` ||
		fn["id"] != "fc_keep" || fn["status"] != "completed" {
		t.Fatalf("namespaced function history not flattened field-faithfully: %v", fn)
	}
	if _, has := fn["namespace"]; has {
		t.Fatalf("flattened function history kept namespace: %v", fn)
	}

	if _, has := itemMap(t, items, 4)["action"]; !has {
		t.Fatalf("unmodeled item lost action: %v", itemMap(t, items, 4))
	}
	if got, want := canonicalJSON(t, items[4]), canonicalJSON(t, searchCall); got != want {
		t.Fatalf("unmodeled item not preserved byte-for-byte:\n got %s\nwant %s", got, want)
	}

	if plain, namespace := rt.RestoreWire("subagents__custom_sub"); plain != "custom_sub" || namespace != "subagents" {
		t.Fatalf("custom identity not registered: %q %q", plain, namespace)
	}
	if !rt.IsCustom("subagents__custom_sub") {
		t.Fatalf("custom identity not marked custom")
	}
	if plain, namespace := rt.RestoreWire("subagents__spawn_agent"); plain != "spawn_agent" || namespace != "subagents" {
		t.Fatalf("namespace identity not registered: %q %q", plain, namespace)
	}
}

func TestNormalizedRequestFilteredCustomHistoryStillPairs(t *testing.T) {
	// apply_patch is filtered from the tool list, but its history items keep
	// their turn pairing: the call still converts so the matching output
	// never orphans, and no undeclared tool declaration is re-introduced.
	body := normalizedCustomRequest(t, []any{
		map[string]any{"type": "custom_tool_call", "call_id": "c9", "name": "apply_patch",
			"input": "*** Begin Patch\n*** End Patch\n"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "c9", "output": "Done!"},
	}, []any{
		map[string]any{"type": "custom", "name": "apply_patch"},
		customExecTool(),
	})
	out, eErr := BuildRequest(museModel, "openai-response", body, nil, shared.NewResponseTools())
	if eErr != nil {
		t.Fatalf("BuildRequest: %+v", eErr)
	}
	m := decodeReq(t, out)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || itemMap(t, tools, 0)["name"] != "exec" {
		t.Fatalf("apply_patch declaration leaked: %v", m["tools"])
	}
	items := inputItems(t, m)
	call := itemMap(t, items, 0)
	if call["type"] != "function_call" || call["name"] != "apply_patch" || call["call_id"] != "c9" {
		t.Fatalf("filtered custom history not converted: %v", call)
	}
	if got := shared.UnwrapCustomToolInput(call["arguments"].(string)); got != "*** Begin Patch\n*** End Patch\n" {
		t.Fatalf("filtered input = %q", got)
	}
	if result := itemMap(t, items, 1); result["type"] != "function_call_output" || result["call_id"] != "c9" {
		t.Fatalf("filtered output not converted: %v", result)
	}
}
