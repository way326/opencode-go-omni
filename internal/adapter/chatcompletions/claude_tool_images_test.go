package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"
)

const claudeToolImageFixture = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func claudeImageToolUse(callID string) map[string]any {
	return map[string]any{
		"type": "tool_use", "id": callID, "name": "view_image", "input": map[string]any{},
	}
}

func claudeImageToolResult(callID string, content ...any) map[string]any {
	return map[string]any{
		"type": "tool_result", "tool_use_id": callID, "content": content,
	}
}

func claudeBase64Image() map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": claudeToolImageFixture,
		},
	}
}

func buildClaudeToolImageRequest(t *testing.T, messages ...any) ([]byte, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"max_tokens": 64, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	out, eErr := BuildRequest("deepseek-v4.1-flash", "claude", body, nil)
	if eErr != nil {
		return nil, eErr
	}
	return out, nil
}

func TestClaudeToolResultImageBridgedAsOrderedAttachment(t *testing.T) {
	out, err := buildClaudeToolImageRequest(t,
		map[string]any{"role": "assistant", "content": []any{claudeImageToolUse("img-1")}},
		map[string]any{"role": "user", "content": []any{
			claudeImageToolResult("img-1",
				map[string]any{"type": "text", "text": "before"},
				claudeBase64Image(),
				map[string]any{"type": "text", "text": "after"},
			),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeOut(t, out, nil)
	messages := decoded["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("want assistant, textual receipt and image attachment; got %v", messages)
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "img-1" ||
		!strings.Contains(tool["content"].(string), "img-1") ||
		strings.Contains(tool["content"].(string), claudeToolImageFixture) {
		t.Fatalf("tool receipt lost association or exposed image data: %v", tool)
	}
	attachment := messages[2].(map[string]any)
	parts := attachment["content"].([]any)
	if attachment["role"] != "user" || len(parts) != 4 {
		t.Fatalf("attachment shape wrong: %v", attachment)
	}
	if !strings.Contains(parts[0].(map[string]any)["text"].(string), "img-1") ||
		parts[1].(map[string]any)["text"] != "before" ||
		parts[3].(map[string]any)["text"] != "after" {
		t.Fatalf("ordered text attribution changed: %v", parts)
	}
	image := parts[2].(map[string]any)
	gotURL := image["image_url"].(map[string]any)["url"]
	if image["type"] != "image_url" ||
		gotURL != "data:image/png;base64,"+claudeToolImageFixture {
		t.Fatalf("image or source changed: %v", image)
	}
}

func TestClaudeToolResultImageParallelBatchPrecedesAttachments(t *testing.T) {
	out, err := buildClaudeToolImageRequest(t,
		map[string]any{"role": "assistant", "content": []any{
			claudeImageToolUse("a"), claudeImageToolUse("b"),
		}},
		map[string]any{"role": "user", "content": []any{
			claudeImageToolResult("b", claudeBase64Image()),
			claudeImageToolResult("a", map[string]any{"type": "text", "text": "text result"}),
			map[string]any{"type": "text", "text": "continue"},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeOut(t, out, nil)["messages"].([]any)
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "assistant,tool,tool,user,user" {
		t.Fatalf("tool batch was interrupted: %v", messages)
	}
	if messages[1].(map[string]any)["tool_call_id"] != "b" ||
		messages[2].(map[string]any)["tool_call_id"] != "a" ||
		messages[2].(map[string]any)["content"] != "text result" {
		t.Fatalf("tool results were reordered or flattened incorrectly: %v", messages[1:3])
	}
	if !strings.Contains(messages[3].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "b") ||
		messages[4].(map[string]any)["content"] != "continue" {
		t.Fatalf("attachment or trailing user content lost: %v", messages[3:])
	}
}

func TestClaudeToolResultImageErrorMarkerIsPreserved(t *testing.T) {
	result := claudeImageToolResult("e1", map[string]any{"type": "text", "text": "render failed"}, claudeBase64Image())
	result["is_error"] = true
	out, err := buildClaudeToolImageRequest(t,
		map[string]any{"role": "assistant", "content": []any{claudeImageToolUse("e1")}},
		map[string]any{"role": "user", "content": []any{result}},
	)
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeOut(t, out, nil)["messages"].([]any)
	if !strings.HasPrefix(messages[1].(map[string]any)["content"].(string), "[error] ") ||
		!strings.Contains(messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "marked as an error") {
		t.Fatalf("error marker was lost: %v", messages)
	}
}

func TestClaudeToolResultImageRejectsIncompleteOrAmbiguousBatches(t *testing.T) {
	call := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "content": []any{claudeImageToolUse(id)}}
	}
	user := func(results ...any) map[string]any {
		return map[string]any{"role": "user", "content": results}
	}
	cases := []struct {
		name     string
		messages []any
	}{
		{"orphan result", []any{user(claudeImageToolResult("a", claudeBase64Image()))}},
		{"wrong id", []any{call("a"), user(claudeImageToolResult("b", claudeBase64Image()))}},
		{"duplicate assistant id", []any{
			map[string]any{"role": "assistant", "content": []any{claudeImageToolUse("a"), claudeImageToolUse("a")}},
			user(claudeImageToolResult("a", claudeBase64Image())),
		}},
		{"duplicate result", []any{
			call("a"), user(claudeImageToolResult("a", claudeBase64Image()), claudeImageToolResult("a", map[string]any{"type": "text", "text": "x"})),
		}},
		{"incomplete parallel batch", []any{
			map[string]any{"role": "assistant", "content": []any{claudeImageToolUse("a"), claudeImageToolUse("b")}},
			user(claudeImageToolResult("a", claudeBase64Image())),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildClaudeToolImageRequest(t, tc.messages...)
			if err == nil {
				t.Fatal("incomplete or ambiguous image batch was accepted")
			}
		})
	}
}

func TestClaudeToolResultImageRejectsInvalidContent(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
	}{
		{"missing source", claudeImageToolResult("a", map[string]any{"type": "image"})},
		{"invalid base64", claudeImageToolResult("a", map[string]any{
			"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "not-base64!"},
		})},
		{"non image media", claudeImageToolResult("a", map[string]any{
			"type": "image", "source": map[string]any{"type": "base64", "media_type": "text/plain", "data": claudeToolImageFixture},
		})},
		{"unknown mixed part", claudeImageToolResult("a", claudeBase64Image(), map[string]any{"type": "video", "data": "secret-payload"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildClaudeToolImageRequest(t,
				map[string]any{"role": "assistant", "content": []any{claudeImageToolUse("a")}},
				map[string]any{"role": "user", "content": []any{tc.result}},
			)
			if err == nil {
				t.Fatal("invalid image content was accepted")
			}
			if strings.Contains(err.Error(), claudeToolImageFixture) || strings.Contains(err.Error(), "secret-payload") {
				t.Fatalf("error exposed image or block payload: %v", err)
			}
		})
	}
}
