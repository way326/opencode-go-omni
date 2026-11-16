package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Claude tool_result blocks may carry an Anthropic image block. Chat
// Completions has no image-capable tool message, so the executor must emit a
// textual, paired tool receipt and then an attributed user attachment. Keep
// this fixture independent of screenshots and real upstream traffic.
const (
	claudeToolImageB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	claudeToolImageURL = "data:image/png;base64," + claudeToolImageB64
	claudeToolCatalog  = `{"data":[{"id":"deepseek-v4.1-flash"}]}`
	claudeToolReply    = `{"id":"chatcmpl-claude-tool-image","object":"chat.completion","model":"deepseek-v4.1-flash",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`
)

var claudeToolStreamFrames = []string{
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n",
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	"data: [DONE]\n\n",
}

type claudeToolHost struct {
	mu   sync.Mutex
	hits int
	body []byte
}

func newClaudeToolManager(t *testing.T) (*Manager, *fakeCaller, *claudeToolHost) {
	t.Helper()
	host := &claudeToolHost{}
	stream := streamResponder(streamScript{upstreamID: "claude-tool-image-up", frames: claudeToolStreamFrames})
	next := func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			var wire struct {
				URL  string `json:"url"`
				Body []byte `json:"body"`
			}
			if err := json.Unmarshal(payload, &wire); err != nil {
				return hostErr("test", "malformed bridged request"), nil
			}
			if strings.HasSuffix(wire.URL, "/v1/chat/completions") {
				host.mu.Lock()
				host.hits++
				host.body = append([]byte(nil), wire.Body...)
				host.mu.Unlock()
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(claudeToolReply),
				}), nil
			}
			return hostErr("test", "unexpected bridged endpoint"), nil
		}
		return stream(method, payload)
	}
	f := &fakeCaller{responder: wrapWithCatalog(claudeToolCatalog, next)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f, host
}

func (h *claudeToolHost) inferenceRequests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

func (h *claudeToolHost) request(t *testing.T) []byte {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hits != 1 {
		t.Fatalf("non-stream inference requests = %d, want 1", h.hits)
	}
	return append([]byte(nil), h.body...)
}

func claudeToolUse(id string) map[string]any {
	return map[string]any{
		"type": "tool_use", "id": id, "name": "view_image", "input": map[string]any{"path": "shot.png"},
	}
}

func claudeToolImageBlock() map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": claudeToolImageB64,
		},
	}
}

func claudeToolMixedResult(id, before, after string) map[string]any {
	return map[string]any{
		"type": "tool_result", "tool_use_id": id,
		"content": []any{
			map[string]any{"type": "text", "text": before},
			claudeToolImageBlock(),
			map[string]any{"type": "text", "text": after},
		},
	}
}

func claudeToolRequest(t *testing.T, toolUses []any, toolResults []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      "opencode-go/deepseek-v4.1-flash",
		"max_tokens": 128,
		"messages": []any{
			map[string]any{"role": "user", "content": "inspect the tool output"},
			map[string]any{"role": "assistant", "content": toolUses},
			map[string]any{"role": "user", "content": toolResults},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func executeClaudeToolImage(t *testing.T, m *Manager, body []byte, stream bool) pluginabi.Envelope {
	t.Helper()
	if stream {
		return decodeEnv(t, mustHandle(t, m, "executor.execute_stream",
			execStreamReqBody("opencode-go/deepseek-v4.1-flash", "claude", body, "claude-tool-image-down")))
	}
	return decodeEnv(t, mustHandle(t, m, "executor.execute",
		execReqBody("opencode-go/deepseek-v4.1-flash", "claude", body, false)))
}

func decodeClaudeToolChatRequest(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var request struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("bridged Chat request is not JSON: %v (%s)", err, body)
	}
	if request.Model != "deepseek-v4.1-flash" {
		t.Fatalf("upstream model = %q, want the bare id", request.Model)
	}
	return request.Messages
}

func assertClaudeToolReceipt(t *testing.T, message map[string]any, id string) {
	t.Helper()
	note, _ := message["content"].(string)
	if message["role"] != "tool" || message["tool_call_id"] != id || strings.TrimSpace(note) == "" {
		t.Fatalf("tool receipt for %s lost its text or tool_call_id: %v", id, message)
	}
	if !strings.Contains(note, id) {
		t.Fatalf("tool receipt does not name %s: %q", id, note)
	}
	if strings.Contains(note, claudeToolImageB64) {
		t.Fatalf("tool receipt for %s repeats the image payload", id)
	}
}

func assertClaudeToolAttachment(t *testing.T, message map[string]any, id string) {
	t.Helper()
	if message["role"] != "user" {
		t.Fatalf("attachment for %s role = %v, want user: %v", id, message["role"], message)
	}
	parts, _ := message["content"].([]any)
	if len(parts) != 4 {
		t.Fatalf("attachment for %s content = %v, want header + text/image/text", id, message["content"])
	}
	header, _ := parts[0].(map[string]any)
	headerText, _ := header["text"].(string)
	if header["type"] != "text" || !strings.Contains(headerText, id) {
		t.Fatalf("attachment for %s needs an attributed text header: %v", id, parts[0])
	}
	if got := parts[1].(map[string]any); got["type"] != "text" || got["text"] != "before-"+id {
		t.Fatalf("attachment for %s lost leading text or order: %v", id, parts)
	}
	image, _ := parts[2].(map[string]any)
	imageURL, _ := image["image_url"].(map[string]any)
	if image["type"] != "image_url" || imageURL["url"] != claudeToolImageURL {
		t.Fatalf("attachment for %s lost the original image: %v", id, image)
	}
	if got := parts[3].(map[string]any); got["type"] != "text" || got["text"] != "after-"+id {
		t.Fatalf("attachment for %s lost trailing text or order: %v", id, parts)
	}
}

func TestExecuteClaudeToolResultImageBridgesToChatUpstream(t *testing.T) {
	body := claudeToolRequest(t,
		[]any{claudeToolUse("toolu_one")},
		[]any{claudeToolMixedResult("toolu_one", "before-toolu_one", "after-toolu_one")},
	)

	for _, stream := range []bool{false, true} {
		mode := map[bool]string{false: "non-stream", true: "stream"}[stream]
		t.Run(mode, func(t *testing.T) {
			m, f, host := newClaudeToolManager(t)
			env := executeClaudeToolImage(t, m, body, stream)
			if !env.OK {
				t.Fatalf("execute envelope = %+v", env.Error)
			}

			var chatBody []byte
			if stream {
				opens := f.callsOf(pluginabi.MethodHostHTTPDoStream)
				if len(opens) != 1 {
					t.Fatalf("do_stream calls = %d, want 1", len(opens))
				}
				m.bridge.WaitForInFlight(5 * time.Second)
				chatBody = wireBody(t, decodePayload(t, opens[0]), "body")
				if host.inferenceRequests() != 0 {
					t.Fatalf("stream execution used the non-stream host path")
				}
			} else {
				chatBody = host.request(t)
			}

			messages := decodeClaudeToolChatRequest(t, chatBody)
			if len(messages) != 4 {
				t.Fatalf("messages = %d, want user+assistant+tool+attachment: %v", len(messages), messages)
			}
			assistant := messages[1]
			calls, _ := assistant["tool_calls"].([]any)
			if assistant["role"] != "assistant" || len(calls) != 1 || calls[0].(map[string]any)["id"] != "toolu_one" {
				t.Fatalf("assistant tool call identity changed: %v", assistant)
			}
			assertClaudeToolReceipt(t, messages[2], "toolu_one")
			assertClaudeToolAttachment(t, messages[3], "toolu_one")
			if got := strings.Count(string(chatBody), claudeToolImageURL); got != 1 {
				t.Fatalf("image data URL occurs %d times, want exactly once", got)
			}
		})
	}
}

func TestExecuteClaudeParallelToolResultImagesPreservePairing(t *testing.T) {
	body := claudeToolRequest(t,
		[]any{claudeToolUse("toolu_a"), claudeToolUse("toolu_b")},
		[]any{
			claudeToolMixedResult("toolu_b", "before-toolu_b", "after-toolu_b"),
			claudeToolMixedResult("toolu_a", "before-toolu_a", "after-toolu_a"),
		},
	)
	m, _, host := newClaudeToolManager(t)
	env := executeClaudeToolImage(t, m, body, false)
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}
	messages := decodeClaudeToolChatRequest(t, host.request(t))
	if len(messages) != 6 {
		t.Fatalf("messages = %d, want user+assistant+2 tools+2 attachments: %v", len(messages), messages)
	}
	calls, _ := messages[1]["tool_calls"].([]any)
	if messages[1]["role"] != "assistant" || len(calls) != 2 ||
		calls[0].(map[string]any)["id"] != "toolu_a" || calls[1].(map[string]any)["id"] != "toolu_b" {
		t.Fatalf("parallel assistant tool_calls changed: %v", messages[1])
	}
	assertClaudeToolReceipt(t, messages[2], "toolu_b")
	assertClaudeToolReceipt(t, messages[3], "toolu_a")
	assertClaudeToolAttachment(t, messages[4], "toolu_b")
	assertClaudeToolAttachment(t, messages[5], "toolu_a")
}

func TestExecuteClaudeToolResultImageRejectedBeforeUpstream(t *testing.T) {
	validSource := func() map[string]any {
		return map[string]any{"type": "base64", "media_type": "image/png", "data": claudeToolImageB64}
	}
	image := func(source map[string]any) []any {
		return []any{map[string]any{"type": "image", "source": source}}
	}
	result := func(id string, content []any) map[string]any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	}

	tests := []struct {
		name        string
		toolUses    []any
		toolResults []any
		leak        string
	}{
		{
			name:        "orphan image result",
			toolResults: []any{result("toolu_orphan", image(validSource()))},
			leak:        claudeToolImageB64,
		},
		{
			name:        "mismatched tool_use id",
			toolUses:    []any{claudeToolUse("toolu_expected")},
			toolResults: []any{result("toolu_other", image(validSource()))},
			leak:        claudeToolImageB64,
		},
		{
			name:     "duplicate result for one call",
			toolUses: []any{claudeToolUse("toolu_one")},
			toolResults: []any{
				result("toolu_one", image(validSource())),
				result("toolu_one", image(validSource())),
			},
			leak: claudeToolImageB64,
		},
		{
			name:     "unknown trailing tool_result block",
			toolUses: []any{claudeToolUse("toolu_one")},
			toolResults: []any{
				result("toolu_one", []any{
					map[string]any{"type": "image", "source": validSource()},
					map[string]any{"type": "document", "text": "secret-tail"},
				}),
			},
			leak: "secret-tail",
		},
		{
			name:        "base64 missing data",
			toolUses:    []any{claudeToolUse("toolu_one")},
			toolResults: []any{result("toolu_one", image(map[string]any{"type": "base64", "media_type": "image/png"}))},
		},
		{
			name:     "base64 malformed data",
			toolUses: []any{claudeToolUse("toolu_one")},
			toolResults: []any{result("toolu_one", image(map[string]any{
				"type": "base64", "media_type": "image/png", "data": "@@secret-base64@@",
			}))},
			leak: "@@secret-base64@@",
		},
		{
			name:     "base64 non image media type",
			toolUses: []any{claudeToolUse("toolu_one")},
			toolResults: []any{result("toolu_one", image(map[string]any{
				"type": "base64", "media_type": "text/plain", "data": "c2VjcmV0",
			}))},
			leak: "c2VjcmV0",
		},
		{
			name:        "url source missing url",
			toolUses:    []any{claudeToolUse("toolu_one")},
			toolResults: []any{result("toolu_one", image(map[string]any{"type": "url"}))},
		},
		{
			name:     "url source has unsupported local scheme",
			toolUses: []any{claudeToolUse("toolu_one")},
			toolResults: []any{result("toolu_one", image(map[string]any{
				"type": "url", "url": "file:///secret-image.png",
			}))},
			leak: "file:///secret-image.png",
		},
	}

	for _, tc := range tests {
		body := claudeToolRequest(t, tc.toolUses, tc.toolResults)
		for _, stream := range []bool{false, true} {
			mode := map[bool]string{false: "non-stream", true: "stream"}[stream]
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				m, f, host := newClaudeToolManager(t)
				env := executeClaudeToolImage(t, m, body, stream)
				if env.OK || env.Error == nil || strings.TrimSpace(env.Error.Message) == "" {
					t.Fatalf("invalid Claude tool_result image must fail descriptively: %+v", env.Error)
				}
				if tc.leak != "" && strings.Contains(env.Error.Message, tc.leak) {
					t.Fatalf("error message leaked image payload: %q", env.Error.Message)
				}
				if got := host.inferenceRequests(); got != 0 {
					t.Fatalf("fake host received %d non-stream inference requests, want 0", got)
				}
				if got := len(f.callsOf(pluginabi.MethodHostHTTPDoStream)); got != 0 {
					t.Fatalf("do_stream calls = %d, want 0", got)
				}
			})
		}
	}
}
