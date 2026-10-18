package responses

import (
	"encoding/json"
	"testing"
)

func TestFromChatCompletionsImageDetailPreserved(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"stream": stream,
				"messages": []any{map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": "before"},
						map[string]any{"type": "image_url", "image_url": map[string]any{
							"url": "https://images.example/low.png", "detail": "low",
						}},
						map[string]any{"type": "text", "text": "between"},
						map[string]any{"type": "image_url", "image_url": map[string]any{
							"url": "https://images.example/high.png", "detail": "high",
						}},
						map[string]any{"type": "image_url", "image_url": map[string]any{
							"url": "https://images.example/auto.png", "detail": "auto",
						}},
						map[string]any{"type": "image_url", "image_url": map[string]any{
							"url": "https://images.example/default.png",
						}},
						map[string]any{"type": "image_url", "image_url": "https://images.example/flat.png", "detail": "low"},
						map[string]any{"type": "text", "text": "after"},
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}

			out, eErr := BuildRequest("gpt-5.6-luna", "openai", body, nil)
			if eErr != nil {
				t.Fatalf("BuildRequest: %+v", eErr)
			}
			m := decodeReq(t, out)
			if stream {
				if m["stream"] != true {
					t.Fatalf("stream = %v, want true", m["stream"])
				}
			} else if m["stream"] != nil && m["stream"] != false {
				t.Fatalf("stream = %v, want false or omitted", m["stream"])
			}

			items := inputItems(t, m)
			if len(items) != 1 {
				t.Fatalf("input items = %d, want one user message: %v", len(items), items)
			}
			content, ok := itemMap(t, items, 0)["content"].([]any)
			if !ok || len(content) != 8 {
				t.Fatalf("content parts = %v, want 8 ordered parts", content)
			}

			want := []struct {
				typ, text, url, detail string
			}{
				{typ: "input_text", text: "before"},
				{typ: "input_image", url: "https://images.example/low.png", detail: "low"},
				{typ: "input_text", text: "between"},
				{typ: "input_image", url: "https://images.example/high.png", detail: "high"},
				{typ: "input_image", url: "https://images.example/auto.png", detail: "auto"},
				{typ: "input_image", url: "https://images.example/default.png"},
				{typ: "input_image", url: "https://images.example/flat.png", detail: "low"},
				{typ: "input_text", text: "after"},
			}
			for i, expected := range want {
				part, ok := content[i].(map[string]any)
				if !ok {
					t.Fatalf("content[%d] = %T, want object", i, content[i])
				}
				if part["type"] != expected.typ {
					t.Errorf("content[%d].type = %v, want %q", i, part["type"], expected.typ)
				}
				if expected.typ == "input_text" {
					if part["text"] != expected.text {
						t.Errorf("content[%d].text = %v, want %q", i, part["text"], expected.text)
					}
					continue
				}
				if part["image_url"] != expected.url {
					t.Errorf("content[%d].image_url = %v, want %q", i, part["image_url"], expected.url)
				}
				if expected.detail == "" {
					if _, exists := part["detail"]; exists {
						t.Errorf("content[%d].detail = %v, want omitted", i, part["detail"])
					}
				} else if part["detail"] != expected.detail {
					t.Errorf("content[%d].detail = %v, want %q", i, part["detail"], expected.detail)
				}
			}
		})
	}
}

func TestNativeResponsesImageDetailStillPreserved(t *testing.T) {
	source := toolOutputImageSourceRequest(t)
	var sourceRequest map[string]any
	if err := json.Unmarshal(source, &sourceRequest); err != nil {
		t.Fatalf("decode native request: %v", err)
	}
	nativeItems := sourceRequest["input"].([]any)
	userMessage := nativeItems[0].(map[string]any)
	userContent := userMessage["content"].([]any)
	userMessage["content"] = append(userContent, map[string]any{
		"type": "input_image", "image_url": toolOutputImageFixture, "detail": "low",
	})
	source, err := json.Marshal(sourceRequest)
	if err != nil {
		t.Fatalf("encode native request: %v", err)
	}
	for _, tc := range []struct {
		name, model string
	}{
		{name: "gpt passthrough", model: "gpt-5.6-luna"},
		{name: "non-gpt normalized", model: "deepseek-v4.1-flash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, eErr := BuildRequest(tc.model, "openai-response", source, nil)
			if eErr != nil {
				t.Fatalf("BuildRequest: %+v", eErr)
			}
			items := inputItems(t, decodeReq(t, out))
			assertToolOutputImageItemsIntact(t, items)
			nativeUserContent := itemMap(t, items, 0)["content"].([]any)
			nativeImage := nativeUserContent[1].(map[string]any)
			if nativeImage["detail"] != "low" {
				t.Errorf("native input_image detail = %v, want low", nativeImage["detail"])
			}

			functionOutput := itemMap(t, items, 2)["output"].([]any)
			functionImage := functionOutput[1].(map[string]any)
			if functionImage["detail"] != "high" {
				t.Errorf("function_call_output image detail = %v, want high", functionImage["detail"])
			}

			customOutput := itemMap(t, items, 4)["output"].([]any)
			customImage := customOutput[0].(map[string]any)["image_url"].(map[string]any)
			if customImage["detail"] != "high" {
				t.Errorf("custom tool output image detail = %v, want high", customImage["detail"])
			}
		})
	}
}
