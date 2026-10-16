package responses

import (
	"encoding/json"
	"testing"
)

// Regression cover for TOOL_IMAGE_FIX_PLAN §3/§4 "原生 Responses 保护":
// a native Responses history that carries an image inside a tool output must
// reach the upstream for GPT passthrough and survive non-GPT normalization
// with the image URL and detail intact. The non-GPT custom_tool_call_output →
// function_call_output type rewrite is pinned as the accepted current
// behaviour. Fixture is a synthetic 1x1 PNG; no screenshot or production
// session body is imported and no image is read or fetched.

// toolOutputImageFixture is a synthetic 69-byte 1x1 RGB red PNG data URL (standard IHDR/IDAT/IEND chunks, valid CRCs).
const toolOutputImageFixture = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

// toolOutputImageTextParts is the mixed result body shared by both tool output
// kinds: text, the image with flat part-level detail (the shape delivered by
// the failing view_image turn), then text again.
func toolOutputImageTextParts() []any {
	return []any{
		map[string]any{"type": "input_text", "text": "captured frame"},
		map[string]any{"type": "input_image", "image_url": toolOutputImageFixture, "detail": "high"},
		map[string]any{"type": "input_text", "text": "frame stored"},
	}
}

// toolOutputImageFunctionItem is the function_call_output under test.
func toolOutputImageFunctionItem() map[string]any {
	return map[string]any{
		"type":    "function_call_output",
		"call_id": "call_fn",
		"output":  toolOutputImageTextParts(),
	}
}

// toolOutputImageCustomItem is the custom_tool_call_output under test; its
// detail sits inside the image_url object, covering the second accepted shape.
func toolOutputImageCustomItem() map[string]any {
	return map[string]any{
		"type":    "custom_tool_call_output",
		"call_id": "call_custom",
		"output": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": toolOutputImageFixture, "detail": "high",
			}},
		},
	}
}

// toolOutputImageSourceRequest builds the native Responses body: one user
// turn plus the function and custom tool calls and their image-bearing
// outputs, mirroring the Codex view_image history shape.
func toolOutputImageSourceRequest(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "opencode-go/deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "look"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_fn", "name": "view_image", "arguments": `{"path":"shot.png"}`},
			toolOutputImageFunctionItem(),
			map[string]any{"type": "custom_tool_call", "call_id": "call_custom", "name": "exec", "input": "ls"},
			toolOutputImageCustomItem(),
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "view_image", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "custom", "name": "exec"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// canonicalJSON re-encodes any JSON value with sorted object keys so shape
// comparisons pin values, not the source key order.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var norm any
	if err := json.Unmarshal(raw, &norm); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	out, err := json.Marshal(norm)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(out)
}

func assertToolOutputImageItemsIntact(t *testing.T, items []any) {
	t.Helper()
	if len(items) != 5 {
		t.Fatalf("input items = %d, want the 5 original items with no synthesized attachment: %v", len(items), items)
	}
	if got, want := canonicalJSON(t, itemMap(t, items, 2)), canonicalJSON(t, toolOutputImageFunctionItem()); got != want {
		t.Fatalf("function tool output changed:\n got %s\nwant %s", got, want)
	}
	userMessages := 0
	for _, item := range items {
		if im, ok := item.(map[string]any); ok && im["type"] == "message" {
			userMessages++
		}
	}
	if userMessages != 1 {
		t.Fatalf("native tool outputs must stay tool items, message items = %d: %v", userMessages, items)
	}
}

func TestNativeResponsesToolOutputImagesPreserved(t *testing.T) {
	source := toolOutputImageSourceRequest(t)

	t.Run("gpt passthrough", func(t *testing.T) {
		out, eErr := BuildRequest("gpt-5.6-luna", "openai-response", source, nil)
		if eErr != nil {
			t.Fatalf("BuildRequest gpt: %+v", eErr)
		}
		m := decodeReq(t, out)
		if m["model"] != "gpt-5.6-luna" {
			t.Fatalf("model = %v, want rewritten upstream id", m["model"])
		}
		items := inputItems(t, m)
		assertToolOutputImageItemsIntact(t, items)
		if got, want := canonicalJSON(t, itemMap(t, items, 4)), canonicalJSON(t, toolOutputImageCustomItem()); got != want {
			t.Fatalf("custom tool output changed on GPT passthrough:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("non-gpt normalized", func(t *testing.T) {
		out, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", source, nil)
		if eErr != nil {
			t.Fatalf("BuildRequest normalized: %+v", eErr)
		}
		m := decodeReq(t, out)
		if m["model"] != "deepseek-v4.1-flash" {
			t.Fatalf("model = %v, want rewritten upstream id", m["model"])
		}
		items := inputItems(t, m)
		assertToolOutputImageItemsIntact(t, items)
		custom := itemMap(t, items, 4)
		if custom["type"] != "function_call_output" {
			t.Fatalf("custom_tool_call_output type = %v, want current function_call_output rewrite", custom["type"])
		}
		if custom["call_id"] != "call_custom" {
			t.Fatalf("custom call_id = %v, want call_custom", custom["call_id"])
		}
		want := toolOutputImageCustomItem()["output"]
		if got := canonicalJSON(t, custom["output"]); got != canonicalJSON(t, want) {
			t.Fatalf("custom output image or detail changed:\n got %s\nwant %s", got, canonicalJSON(t, want))
		}
	})
}
