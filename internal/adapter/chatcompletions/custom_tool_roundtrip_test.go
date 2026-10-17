package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
)

func TestCustomCodeModeCompleteRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			code := "const value = {input: \"中文\\\\path\"};\r\ntext(value);\n"
			rt := shared.NewResponseTools()
			body, _ := json.Marshal(map[string]any{
				"input": "Use exec", "stream": stream,
				"tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": "custom", "name": "exec", "description": "Run JavaScript"},
				}}},
			})
			request, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil, rt)
			wire := decodeOut(t, request, eErr)
			fn := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			params := fn["parameters"].(map[string]any)
			if params["properties"].(map[string]any)["input"] == nil {
				t.Fatalf("upstream lacks an explicit input schema: %v", params)
			}
			args, _ := json.Marshal(map[string]string{"input": code})
			var returned shared.RespItem
			if !stream {
				upstream, _ := json.Marshal(map[string]any{"id": "resp_1", "model": "deepseek-v4.1-flash", "choices": []any{
					map[string]any{"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
						map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": fn["name"], "arguments": string(args)}},
					}}, "finish_reason": "tool_calls"},
				}})
				out, err := ConvertNonStreamResponse("openai-response", 200, upstream, rt)
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					Output []shared.RespItem `json:"output"`
				}
				if err := json.Unmarshal(out, &response); err != nil {
					t.Fatal(err)
				}
				returned = response.Output[0]
			} else {
				sc := NewStreamConverter("openai-response", rt)
				var events [][]byte
				feed := func(delta map[string]any, finish any, usage any) {
					t.Helper()
					chunk := map[string]any{"id": "resp_1", "model": "deepseek-v4.1-flash", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
					if usage != nil {
						chunk["choices"] = []any{}
						chunk["usage"] = usage
					}
					b, _ := json.Marshal(chunk)
					b = append(append([]byte("data: "), b...), '\n', '\n')
					// Split UTF-8, JSON escapes and SSE framing across transport chunks.
					for i := 0; i < len(b); i += 7 {
						end := min(i+7, len(b))
						evs, _, err := sc.Feed(b[i:end])
						if err != nil {
							t.Fatal(err)
						}
						events = append(events, evs...)
					}
				}
				feed(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": fn["name"], "arguments": ""}}}}, nil, nil)
				for _, char := range string(args) {
					feed(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": string(char)}}}}, nil, nil)
				}
				feed(map[string]any{}, "tool_calls", nil)
				feed(nil, nil, map[string]any{"prompt_tokens": 11, "completion_tokens": 7})
				var deltas strings.Builder
				doneCount := 0
				for _, ev := range parseEvents(t, events) {
					if ev.Name == "response.custom_tool_call_input.delta" {
						deltas.WriteString(ev.Data["delta"].(string))
					}
					if ev.Name == "response.output_item.done" {
						b, _ := json.Marshal(ev.Data["item"])
						var item shared.RespItem
						if err := json.Unmarshal(b, &item); err != nil {
							t.Fatal(err)
						}
						if item.Type == "custom_tool_call" {
							returned = item
							doneCount++
						}
					}
				}
				if doneCount != 1 || deltas.String() != code {
					t.Fatalf("stream input changed: count=%d delta=%q", doneCount, deltas.String())
				}
			}
			if returned.Type != "custom_tool_call" || returned.Input != code || returned.Namespace != "functions" || returned.CallID != "call_1" {
				t.Fatalf("custom return identity or code changed: %+v", returned)
			}
			var next map[string]any
			_ = json.Unmarshal(body, &next)
			next["input"] = []any{returned, map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "CODE_RESULT_391"}, map[string]any{"role": "user", "content": "Continue"}}
			nextBody, _ := json.Marshal(next)
			nextWire, err := BuildRequest("deepseek-v4.1-flash", "openai-response", nextBody, nil, shared.NewResponseTools())
			second := decodeOut(t, nextWire, err)
			messages := second["messages"].([]any)
			call := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
			var replay map[string]string
			if err := json.Unmarshal([]byte(call["function"].(map[string]any)["arguments"].(string)), &replay); err != nil {
				t.Fatal(err)
			}
			if replay["input"] != code || call["id"] != "call_1" || messages[1].(map[string]any)["tool_call_id"] != "call_1" || messages[1].(map[string]any)["content"] != "CODE_RESULT_391" {
				t.Fatalf("second-turn function contract or receipt changed: %s", nextWire)
			}
		})
	}
}
