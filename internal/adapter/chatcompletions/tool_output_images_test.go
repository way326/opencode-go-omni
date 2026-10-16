package chatcompletions

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A synthetic 1x1 PNG, independent of user screenshots or production history.
const toolImageFixture = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func imageToolItem(callID string, content ...any) map[string]any {
	if content == nil {
		content = []any{map[string]any{"type": "input_image", "image_url": toolImageFixture, "detail": "high"}}
	}
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": content}
}

func imageToolCall(callID string) map[string]any {
	return map[string]any{"type": "function_call", "call_id": callID, "name": "view_image", "arguments": "{}"}
}

func buildImageToolHistory(t *testing.T, items ...any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"input": items})
	if err != nil {
		t.Fatal(err)
	}
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	return decodeOut(t, out, eErr)
}

func TestResponsesToolOutputImage(t *testing.T) {
	m := buildImageToolHistory(t, imageToolCall("c1"), imageToolItem("c1"))
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want assistant, tool, image attachment; got %v", msgs)
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || !strings.Contains(tool["content"].(string), "c1") {
		t.Fatalf("tool result lost association: %v", tool)
	}
	attachment := msgs[2].(map[string]any)
	parts := attachment["content"].([]any)
	if attachment["role"] != "user" || len(parts) != 2 || !strings.Contains(parts[0].(map[string]any)["text"].(string), "c1") {
		t.Fatalf("missing tool attachment attribution: %v", attachment)
	}
	image := parts[1].(map[string]any)
	url := image["image_url"].(map[string]any)
	if image["type"] != "image_url" || url["url"] != toolImageFixture || url["detail"] != "high" {
		t.Fatalf("image or detail changed: %v", image)
	}
}

func TestResponsesToolOutputImagesPreserveOrderedContent(t *testing.T) {
	remote := "https://example.com/second.png?size=2#preview"
	m := buildImageToolHistory(t, imageToolCall("c1"), imageToolItem("c1",
		map[string]any{"type": "input_text", "text": "before"},
		map[string]any{"type": "input_image", "image_url": toolImageFixture, "detail": "high"},
		map[string]any{"type": "output_text", "text": "between"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": remote, "detail": "low"}},
		map[string]any{"type": "text", "text": "after"},
	))
	msgs := m["messages"].([]any)
	parts := msgs[2].(map[string]any)["content"].([]any)
	want := []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": toolImageFixture, "detail": "high"}},
		map[string]any{"type": "text", "text": "between"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": remote, "detail": "low"}},
		map[string]any{"type": "text", "text": "after"},
	}
	if !reflect.DeepEqual(parts[1:], want) {
		t.Fatalf("ordered multimodal output changed: got %v, want %v", parts[1:], want)
	}
	if strings.Contains(msgs[1].(map[string]any)["content"].(string), "before") {
		t.Fatal("mixed content was duplicated into the textual tool receipt")
	}
}

func TestResponsesToolOutputImageParallelBatch(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "in order", true: "reverse order"}[reverse], func(t *testing.T) {
			first, second := "a", "b"
			if reverse {
				first, second = second, first
			}
			m := buildImageToolHistory(t,
				imageToolCall("a"), imageToolCall("b"), imageToolItem(first),
				map[string]any{"type": "additional_tools", "tools": []any{}},
				imageToolItem(second),
			)
			msgs := m["messages"].([]any)
			if len(msgs) != 5 {
				t.Fatalf("batch should have 1 assistant, 2 tools, 2 attachments: %v", msgs)
			}
			if calls := msgs[0].(map[string]any)["tool_calls"].([]any); len(calls) != 2 {
				t.Fatalf("parallel calls split: %v", calls)
			}
			for i, id := range []string{first, second} {
				tool := msgs[i+1].(map[string]any)
				if tool["role"] != "tool" || tool["tool_call_id"] != id {
					t.Fatalf("tool batch interrupted: %v", msgs)
				}
				attachment := msgs[i+3].(map[string]any)
				if attachment["role"] != "user" || !strings.Contains(attachment["content"].([]any)[0].(map[string]any)["text"].(string), id) {
					t.Fatalf("image association changed: %v", attachment)
				}
			}
		})
	}
}

func TestResponsesToolOutputImageWaitsForTextResult(t *testing.T) {
	m := buildImageToolHistory(t, imageToolCall("image"), imageToolCall("text"), imageToolItem("image"),
		map[string]any{"type": "function_call_output", "call_id": "text", "output": "done"})
	msgs := m["messages"].([]any)
	if len(msgs) != 4 || msgs[2].(map[string]any)["role"] != "tool" || msgs[2].(map[string]any)["content"] != "done" || msgs[3].(map[string]any)["role"] != "user" {
		t.Fatalf("attachment interrupted pending textual tool result: %v", msgs)
	}
}

func TestResponsesToolImageReasoningMetadataDoesNotInterruptBatch(t *testing.T) {
	reason := func(text string) any {
		return map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
	}
	m := buildImageToolHistory(t, reason("before"), imageToolCall("a"), imageToolCall("b"), imageToolItem("a"),
		reason("inside"), map[string]any{"type": "reasoning", "encrypted_content": "opaque"}, imageToolItem("b"), reason("after"))
	msgs := m["messages"].([]any)
	roles := []string{}
	for _, msg := range msgs {
		roles = append(roles, msg.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"assistant", "tool", "tool", "user", "user", "assistant"}) {
		t.Fatalf("metadata interrupted batch: %v", msgs)
	}
	if msgs[0].(map[string]any)["reasoning_content"] != "beforeinside" || msgs[5].(map[string]any)["reasoning_content"] != "after" {
		t.Fatalf("reasoning moved to wrong turn or encrypted content leaked: %v", msgs)
	}
}

func TestResponsesToolImageWithoutDetailAndNamespacedCall(t *testing.T) {
	call := imageToolCall("ns1")
	call["namespace"] = "viewer"
	items := []any{
		map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "namespace", "name": "viewer", "tools": []any{map[string]any{"type": "function", "name": "view_image"}}}}},
		call, imageToolItem("ns1", map[string]any{"type": "input_image", "image_url": "https://example.com/img.png"}),
	}
	m := buildImageToolHistory(t, items...)
	msgs := m["messages"].([]any)
	fn := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "viewer__view_image" {
		t.Fatalf("namespace normalization lost: %v", fn)
	}
	image := msgs[2].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)
	if image["url"] != "https://example.com/img.png" {
		t.Fatalf("remote URL changed: %v", image)
	}
	if _, exists := image["detail"]; exists {
		t.Fatalf("omitted detail was fabricated: %v", image)
	}
}

func TestResponsesToolOutputImageMultipleTurns(t *testing.T) {
	reasoning := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "inspect"}}}
	user := map[string]any{"type": "message", "role": "user", "content": "continue"}
	m := buildImageToolHistory(t, reasoning, imageToolCall("a"), imageToolItem("a"),
		user, reasoning, imageToolCall("b"), imageToolItem("b"), imageToolCall("c"), imageToolItem("c"))
	msgs := m["messages"].([]any)
	roles := []string{}
	for _, msg := range msgs {
		roles = append(roles, msg.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"assistant", "tool", "user", "user", "assistant", "tool", "user", "assistant", "tool", "user"}) {
		t.Fatalf("images or calls moved across turns: %v", roles)
	}
	for _, index := range []int{0, 4} {
		if msgs[index].(map[string]any)["reasoning_content"] != "inspect" {
			t.Fatalf("reasoning detached: %v", msgs[index])
		}
	}
	if len(msgs[7].(map[string]any)["tool_calls"].([]any)) != 1 {
		t.Fatal("new call merged into an earlier batch")
	}
}

func TestResponsesCustomToolOutputImage(t *testing.T) {
	call := map[string]any{"type": "custom_tool_call", "call_id": "exec1", "name": "exec", "input": "show()"}
	result := imageToolItem("exec1")
	result["type"] = "custom_tool_call_output"
	m := buildImageToolHistory(t, call, result)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["tool_call_id"] != "exec1" || msgs[2].(map[string]any)["role"] != "user" {
		t.Fatalf("custom tool image not bridged: %v", msgs)
	}
}

func TestResponsesTextToolResultsRemainUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output any
		want   string
	}{
		{"string", "hello", "hello"}, {"empty string", "", ""}, {"null", nil, ""},
		{"empty array", []any{}, ""}, {"text array", []any{map[string]any{"type": "input_text", "text": "a"}, map[string]any{"type": "output_text", "text": "b"}}, "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := buildImageToolHistory(t, map[string]any{"type": "function_call_output", "call_id": "legacy", "output": tc.output})
			msgs := m["messages"].([]any)
			if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "tool" || msgs[0].(map[string]any)["content"] != tc.want {
				t.Fatalf("legacy text-only history changed: %v", msgs)
			}
		})
	}
}

func TestResponsesToolOutputImageErrors(t *testing.T) {
	textResult := func(id string) any {
		return map[string]any{"type": "function_call_output", "call_id": id, "output": "text"}
	}
	for _, tc := range []struct {
		name  string
		items []any
	}{
		{"orphan", []any{imageToolItem("a")}},
		{"missing id", []any{imageToolCall(""), imageToolItem("")}},
		{"wrong id", []any{imageToolCall("a"), imageToolItem("b")}},
		{"missing parallel result", []any{imageToolCall("a"), imageToolCall("b"), imageToolItem("a")}},
		{"duplicate call", []any{imageToolCall("a"), imageToolCall("a"), imageToolItem("a")}},
		{"duplicate image", []any{imageToolCall("a"), imageToolItem("a"), imageToolItem("a")}},
		{"duplicate text before image", []any{imageToolCall("a"), imageToolCall("b"), textResult("a"), textResult("a"), imageToolItem("b")}},
		{"duplicate text after image", []any{imageToolCall("a"), imageToolItem("a"), textResult("a")}},
		{"unfinished at user boundary", []any{imageToolCall("a"), imageToolCall("b"), imageToolItem("a"), map[string]any{"type": "message", "role": "user", "content": "next"}, textResult("b")}},
		{"unfinished at next call", []any{imageToolCall("a"), imageToolCall("b"), imageToolItem("a"), imageToolCall("c"), textResult("b"), imageToolItem("c")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"input": tc.items})
			out, e := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
			if e == nil || out != nil {
				t.Fatalf("invalid image pairing was accepted: %s", out)
			}
			if strings.Contains(e.Message, "base64") || strings.Contains(e.Message, toolImageFixture) {
				t.Fatalf("error exposed image data: %v", e)
			}
		})
	}
}

func TestResponsesToolOutputImageInvalidParts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output any
	}{
		{"no url", []any{map[string]any{"type": "input_image"}}},
		{"file id only", []any{map[string]any{"type": "input_image", "file_id": "secret-payload"}}},
		{"invalid url type", []any{map[string]any{"type": "input_image", "image_url": 42}}},
		{"malformed url", []any{map[string]any{"type": "input_image", "image_url": "https://%secret-payload"}}},
		{"invalid base64", []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,secret-payload!"}}},
		{"non image data", []any{map[string]any{"type": "input_image", "image_url": "data:text/plain;base64,c2VjcmV0"}}},
		{"empty image subtype", []any{map[string]any{"type": "input_image", "image_url": "data:image/;base64,c2VjcmV0"}}},
		{"local path", []any{map[string]any{"type": "input_image", "image_url": "file:///secret-payload.png"}}},
		{"invalid detail", []any{map[string]any{"type": "input_image", "image_url": toolImageFixture, "detail": 42}}},
		{"conflicting details", []any{map[string]any{"type": "input_image", "image_url": map[string]any{"url": toolImageFixture, "detail": "low"}, "detail": "high"}}},
		{"file part", []any{map[string]any{"type": "input_file", "file_id": "secret-payload"}}},
		{"audio part", []any{map[string]any{"type": "input_audio", "data": "secret-payload"}}},
		{"mixed unknown", []any{map[string]any{"type": "input_image", "image_url": toolImageFixture}, map[string]any{"type": "input_file", "file_data": "secret-payload"}}},
		{"object output", map[string]any{"type": "input_image", "image_url": toolImageFixture}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"input": []any{imageToolCall("a"), map[string]any{"type": "function_call_output", "call_id": "a", "output": tc.output}}})
			out, e := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
			if e == nil || out != nil {
				t.Fatalf("invalid output was accepted: %s", out)
			}
			if strings.Contains(e.Message, "secret-payload") || strings.Contains(e.Message, toolImageFixture) {
				t.Fatalf("error exposed payload: %v", e)
			}
		})
	}
}
