package messages

import (
	"encoding/json"
	"testing"
)

func TestCustomToolHistoryUsesStringInputObject(t *testing.T) {
	code := "text('中文');\r\n"
	body, _ := json.Marshal(map[string]any{
		"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "call_code", "name": "exec", "input": code},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_code", "output": "receipt"},
		},
	})
	out, eErr := BuildRequest("minimax-m3", "openai-response", body, nil)
	if eErr != nil {
		t.Fatal(eErr)
	}
	r := decodeReq(t, out)
	msgs := r["messages"].([]any)
	call := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "call_code" || call["input"].(map[string]any)["input"] != code {
		t.Fatalf("custom call is not a valid Messages input object: %s", out)
	}
	receipt := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if receipt["type"] != "tool_result" || receipt["tool_use_id"] != "call_code" || receipt["content"] != "receipt" {
		t.Fatalf("custom receipt changed: %s", out)
	}
}
