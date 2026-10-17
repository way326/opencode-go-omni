package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCustomToolStringInputContract(t *testing.T) {
	for _, target := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(target, func(t *testing.T) {
			r := &ResponsesRequest{Tools: []RespTool{{Type: "custom", Name: "exec", Description: "Run raw code."}}}
			if _, eErr := NewResponseTools().Normalize(r, target); eErr != nil {
				t.Fatal(eErr)
			}
			var schema struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required             []string `json:"required"`
				AdditionalProperties bool     `json:"additionalProperties"`
			}
			if err := json.Unmarshal(r.Tools[0].Parameters, &schema); err != nil {
				t.Fatal(err)
			}
			if schema.Type != "object" || len(schema.Properties) != 1 || schema.Properties["input"].Type != "string" ||
				len(schema.Required) != 1 || schema.Required[0] != "input" || schema.AdditionalProperties {
				t.Fatalf("custom input schema = %s", r.Tools[0].Parameters)
			}
			if !strings.Contains(r.Tools[0].Description, "input") || !strings.Contains(r.Tools[0].Description, "JSON") {
				t.Fatalf("upstream description does not explain the wrapper: %q", r.Tools[0].Description)
			}
		})
	}
}

func TestCustomToolHistoryStringRoundTrip(t *testing.T) {
	inputs := []string{"", "text('中文');\r\n", `{"input":"literal JSON, not a wrapper"}`, "const p = \"C:\\\\中文\\\\file.txt\";\ntext(p);"}
	for _, namespace := range []string{"", "functions"} {
		for _, input := range inputs {
			t.Run(namespace+"/"+input, func(t *testing.T) {
				declared := RespTool{Type: "custom", Name: "exec"}
				if namespace != "" {
					declared = RespTool{Type: "namespace", Name: namespace, Tools: []RespTool{declared}}
				}
				r := reqWithInput(t, []any{
					map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "namespace": namespace, "input": input},
					map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "receipt"},
				}, []RespTool{declared}, nil)
				rt := NewResponseTools()
				items, eErr := rt.Normalize(r, "/v1/chat/completions")
				if eErr != nil {
					t.Fatal(eErr)
				}
				call := items[0]
				var args map[string]string
				if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
					t.Fatalf("history arguments are not a JSON object: %q (%v)", call.Arguments, err)
				}
				if call.Type != "function_call" || call.Input != "" || call.CallID != "call_1" || len(args) != 1 || args["input"] != input {
					t.Fatalf("history contract changed input or identity: %+v", call)
				}
				if !rt.IsCustom(call.Name) || UnwrapCustomToolInput(call.Arguments) != input {
					t.Fatalf("return path cannot restore exact custom input: %+v", call)
				}
				if items[1].CallID != "call_1" || string(items[1].Output) != `"receipt"` {
					t.Fatalf("receipt changed: %+v", items[1])
				}
				// Reprocessing a normalized history must not wrap it twice.
				r.Input, _ = json.Marshal(items)
				again, eErr := rt.Normalize(r, "/v1/chat/completions")
				if eErr != nil || again[0].Arguments != call.Arguments {
					t.Fatalf("normalization is not idempotent: %+v, %v", again, eErr)
				}
			})
		}
	}
}

func TestCustomHistoryWithoutCurrentDeclaration(t *testing.T) {
	r := reqWithInput(t, []any{map[string]any{
		"type": "custom_tool_call", "name": "exec", "namespace": "functions", "call_id": "call_old", "input": "text(1)",
	}}, nil, nil)
	rt := NewResponseTools()
	items, eErr := rt.Normalize(r, "/v1/chat/completions")
	if eErr != nil || len(items) != 1 || !rt.IsCustom(items[0].Name) {
		t.Fatalf("historical custom identity lost: %+v, %v", items, eErr)
	}
}

func TestOrdinaryFunctionHistoryNotWrapped(t *testing.T) {
	r := reqWithInput(t, []any{map[string]any{
		"type": "function_call", "name": "read", "call_id": "ordinary", "arguments": `{"path":"x"}`,
	}}, []RespTool{{Type: "function", Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}, nil)
	items, eErr := NewResponseTools().Normalize(r, "/v1/chat/completions")
	if eErr != nil || items[0].Arguments != `{"path":"x"}` || string(r.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("ordinary function changed: %+v, %v", items, eErr)
	}
}

func TestExportedCustomToolIdentityRoundTrip(t *testing.T) {
	for _, namespace := range []string{"", "functions", strings.Repeat("long_namespace_", 8)} {
		r := reqWithInput(t, []any{map[string]any{
			"type": "custom_tool_call", "name": "exec", "namespace": namespace,
			"call_id": "original", "input": "text('中文')",
		}}, nil, nil)
		rt := NewResponseTools()
		items, eErr := rt.Normalize(r, "/v1/responses")
		if eErr != nil {
			t.Fatal(eErr)
		}
		wire := QualifyName("exec", namespace)
		name, restoredNamespace := rt.RestoreWire(wire)
		if items[0].Name != wire || name != "exec" || restoredNamespace != namespace || !rt.IsCustom(wire) || len(wire) > 64 {
			t.Fatalf("custom identity mismatch: item=%+v, wire=%q, restored=(%q,%q)", items[0], wire, name, restoredNamespace)
		}
	}
	var rt *ResponseTools
	name, namespace := rt.RestoreWire("unknown")
	if name != "unknown" || namespace != "" {
		t.Fatalf("unknown tool identity changed: (%q,%q)", name, namespace)
	}
}

func TestEmptyCustomInputRemainsExplicitInResponse(t *testing.T) {
	rt := NewResponseTools()
	r := &ResponsesRequest{Tools: []RespTool{{Type: "custom", Name: "exec"}}}
	if _, eErr := rt.Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatal(eErr)
	}
	assembler := NewOutputAssembler("message", rt)
	assembler.AppendFunctionCall("call_empty", "exec", WrapCustomToolInput(""))
	encoded, err := json.Marshal(assembler.Render())
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || string(items[0]["input"]) != `""` || string(items[0]["type"]) != `"custom_tool_call"` {
		t.Fatalf("empty code lost its required input field: %s", encoded)
	}
	ordinary, err := json.Marshal(RespItem{Type: "message", Role: "assistant"})
	if err != nil || strings.Contains(string(ordinary), `"input"`) {
		t.Fatalf("non-custom item acquired an input field: %s, %v", ordinary, err)
	}
}
