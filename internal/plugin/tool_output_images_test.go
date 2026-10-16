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

// Regression cover for TOOL_IMAGE_FIX_PLAN §3/§4 "fake host 核验实际向 Chat
// 上游发送的消息": a Responses history whose view_image result is a pure image
// must bridge to Chat Completions as assistant tool_calls, a matching textual
// tool message, then one attributed user attachment carrying the verbatim image
// URL and detail. Invalid image payloads and unpaired image results must fail
// during prepareUpstreamRequest, before the fake host sees any inference
// request. The fixture is a synthetic 1x1 PNG; no screenshot is read and no
// upstream call is made.

const (
	toolImgReasoning  = "先看一下这张图片。"
	toolImgFixtureB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	toolImgDataURL    = "data:image/png;base64," + toolImgFixtureB64
	toolImgCatalog    = `{"data":[{"id":"deepseek-v4.1-flash"}]}`
)

// toolImgChatCompletion is the canned non-stream Chat answer; its bare model id
// must surface downstream as the public prefixed id.
const toolImgChatCompletion = `{"id":"chatcmpl-img","object":"chat.completion","model":"deepseek-v4.1-flash",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"收到图片。"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`

// toolImgChatFrames is the streaming sibling: chunks carry the bare model id so
// the terminal response.completed proves the public-id rewrite on stream too.
var toolImgChatFrames = []string{
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"收到图片。"}}]}` + "\n\n",
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	"data: [DONE]\n\n",
}

// toolImgHost records every non-stream Chat inference exchange the fake host
// observes; the streaming body is read back from the captured do_stream payload.
type toolImgHost struct {
	mu   sync.Mutex
	hits int
	body []byte
}

// toolImgManager registers a manager whose fake host answers catalog fetches
// locally, captures Chat inference requests, and serves the canned non-stream
// and streaming answers.
func toolImgManager(t *testing.T, chatBody string) (*Manager, *fakeCaller, *toolImgHost) {
	t.Helper()
	host := &toolImgHost{}
	stream := streamResponder(streamScript{upstreamID: "tool-img-up", frames: toolImgChatFrames})
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
				host.body = wire.Body
				host.mu.Unlock()
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(chatBody),
				}), nil
			}
			return hostErr("test", "unexpected bridged endpoint"), nil
		}
		return stream(method, payload)
	}
	f := &fakeCaller{responder: wrapWithCatalog(toolImgCatalog, next)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f, host
}

func (h *toolImgHost) inferenceRequests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

func (h *toolImgHost) chatRequest(t *testing.T) []byte {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hits != 1 {
		t.Fatalf("non-stream inference requests = %d, want 1", h.hits)
	}
	return append([]byte(nil), h.body...)
}

// toolImgBody renders the Responses request: reasoning, the view_image call and
// the tool result carrying the given output, exactly as the failing Codex turn
// presented them.
func toolImgBody(t *testing.T, items []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "opencode-go/deepseek-v4.1-flash",
		"input": items,
		"tools": []any{map[string]any{
			"type": "function", "name": "view_image",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func toolImgImageOutput(output []any) []any {
	return []any{
		map[string]any{"type": "reasoning", "content": toolImgReasoning},
		map[string]any{"type": "function_call", "call_id": "call_view", "name": "view_image", "arguments": `{"path":"shot.png"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_view", "output": output},
	}
}

func toolImgPairedInput() []any {
	return toolImgImageOutput([]any{
		map[string]any{"type": "input_image", "image_url": toolImgDataURL, "detail": "high"},
	})
}

// toolImgAssertBridgedMessages pins the approved bridge shape without pinning
// the attachment wording: assistant tool_calls with reasoning_content, one
// textual tool result matched by call_id, then one user attachment whose image
// URL and detail are the original values and whose payload appears exactly once.
func toolImgAssertBridgedMessages(t *testing.T, body []byte, wantReasoning string) {
	t.Helper()
	var req struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("bridged Chat request is not JSON: %v (%s)", err, body)
	}
	if req.Model != "deepseek-v4.1-flash" {
		t.Fatalf("upstream model = %q, want the bare upstream id", req.Model)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("pure-image result must bridge to assistant+tool+attachment, got %d: %v", len(req.Messages), req.Messages)
	}
	assistant := req.Messages[0]
	if assistant["role"] != "assistant" || assistant["reasoning_content"] != wantReasoning {
		t.Fatalf("assistant reasoning lost: %v", assistant)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v, want the view_image call", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if call["id"] != "call_view" || function["name"] != "view_image" {
		t.Fatalf("view_image call identity changed: %v", call)
	}
	tool := req.Messages[1]
	note, _ := tool["content"].(string)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_view" || strings.TrimSpace(note) == "" {
		t.Fatalf("tool result must stay textual and paired: %v", tool)
	}
	if !strings.Contains(note, "call_view") {
		t.Fatalf("tool note must reference the exact call_id: %q", note)
	}
	if strings.Contains(note, toolImgFixtureB64) {
		t.Fatalf("tool note repeats the image payload instead of attaching it once")
	}
	attachment := req.Messages[2]
	if attachment["role"] != "user" {
		t.Fatalf("image attachment role = %v", attachment["role"])
	}
	parts, _ := attachment["content"].([]any)
	if len(parts) < 2 {
		t.Fatalf("attachment must carry a source note and the image: %v", attachment["content"])
	}
	header, _ := parts[0].(map[string]any)
	headerText, _ := header["text"].(string)
	if header["type"] != "text" || strings.TrimSpace(headerText) == "" {
		t.Fatalf("attachment must lead with a text source note: %v", parts[0])
	}
	if !strings.Contains(headerText, "call_view") {
		t.Fatalf("attachment source note must reference the exact call_id: %q", headerText)
	}
	url, detail := toolImgImagePart(t, parts[1:])
	if url != toolImgDataURL || detail != "high" {
		t.Fatalf("attachment image = %q detail=%q, want verbatim data URL and high detail", url, detail)
	}
	if got := strings.Count(string(body), toolImgDataURL); got != 1 {
		t.Fatalf("image data URL occurs %d times, want exactly once (attachment only)", got)
	}
}

// toolImgImagePart returns the URL and detail of the first image part in the
// attachment tail. Only the standard Chat Completions nested encoding
// image_url:{url,detail} is accepted, so a future non-standard encoding (flat
// part-level detail or a bare string URL) fails this regression instead of
// passing silently.
func toolImgImagePart(t *testing.T, parts []any) (string, string) {
	t.Helper()
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok || part["type"] != "image_url" {
			continue
		}
		image, ok := part["image_url"].(map[string]any)
		if !ok {
			t.Fatalf("image_url must be the nested {url,detail} object, got %T: %v", part["image_url"], part["image_url"])
		}
		url, _ := image["url"].(string)
		detail, _ := image["detail"].(string)
		return url, detail
	}
	t.Fatalf("attachment carries no image_url part: %v", parts)
	return "", ""
}

// toolImgCompletedModels parses the emitted SSE frames and returns the model on
// every response.completed event, so the terminal event itself carries the
// exact public model id instead of the id merely appearing somewhere in the
// reassembled stream blob.
func toolImgCompletedModels(t *testing.T, events []string) []string {
	t.Helper()
	var models []string
	for _, event := range events {
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var parsed struct {
				Type     string `json:"type"`
				Response struct {
					Model string `json:"model"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &parsed); err != nil {
				continue
			}
			if parsed.Type == "response.completed" {
				models = append(models, parsed.Response.Model)
			}
		}
	}
	if len(models) == 0 {
		t.Fatalf("no response.completed event in the emitted stream: %q", events)
	}
	return models
}

// TestExecuteToolOutputImageBridgesToChatUpstream covers TOOL_IMAGE_FIX_PLAN §4
// "流式与非流式执行": the fake host verifies the actual prepareUpstreamRequest
// output on both paths, and the downstream Responses payload keeps the public
// model id.
func TestExecuteToolOutputImageBridgesToChatUpstream(t *testing.T) {
	input := toolImgPairedInput()

	t.Run("non-stream", func(t *testing.T) {
		m, _, host := toolImgManager(t, toolImgChatCompletion)
		env := decodeEnv(t, mustHandle(t, m, "executor.execute",
			execReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", toolImgBody(t, input), false)))
		if !env.OK {
			t.Fatalf("execute envelope = %+v", env.Error)
		}
		toolImgAssertBridgedMessages(t, host.chatRequest(t), toolImgReasoning)

		var result pluginapi.ExecutorResponse
		if err := json.Unmarshal(env.Result, &result); err != nil {
			t.Fatalf("decode executor result: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(result.Payload, &payload); err != nil {
			t.Fatalf("decode Responses payload: %v", err)
		}
		if payload["model"] != "opencode-go/deepseek-v4.1-flash" {
			t.Fatalf("downstream model = %v, want the public id", payload["model"])
		}
	})

	t.Run("stream", func(t *testing.T) {
		m, f, host := toolImgManager(t, toolImgChatCompletion)
		env := decodeEnv(t, mustHandle(t, m, "executor.execute_stream",
			execStreamReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", toolImgBody(t, input), "tool-img-down")))
		if !env.OK {
			t.Fatalf("execute_stream envelope = %+v", env.Error)
		}
		opens := f.callsOf(pluginabi.MethodHostHTTPDoStream)
		if len(opens) != 1 {
			t.Fatalf("do_stream calls = %d, want 1", len(opens))
		}
		if host.inferenceRequests() != 0 {
			t.Fatalf("stream execution must not use the non-stream host.http.do path")
		}
		m.bridge.WaitForInFlight(5 * time.Second)
		toolImgAssertBridgedMessages(t, wireBody(t, decodePayload(t, opens[0]), "body"), toolImgReasoning)

		models := toolImgCompletedModels(t, emittedEvents(t, f))
		if len(models) != 1 || models[0] != "opencode-go/deepseek-v4.1-flash" {
			t.Fatalf("response.completed models = %v, want exactly the public model id", models)
		}
		assertCleanStreamClose(t, f)
	})
}

// TestExecuteToolOutputImageRejectedBeforeUpstream covers TOOL_IMAGE_FIX_PLAN §4
// "无效图片、未知类型、配对错误": each failure must happen during request
// preparation, so the fake host records no inference request at all and the
// error never echoes image data.
func TestExecuteToolOutputImageRejectedBeforeUpstream(t *testing.T) {
	orphan := []any{
		map[string]any{"type": "reasoning", "content": toolImgReasoning},
		map[string]any{"type": "function_call_output", "call_id": "call_orphan", "output": []any{
			map[string]any{"type": "input_image", "image_url": toolImgDataURL, "detail": "high"},
		}},
	}
	cases := []struct {
		name  string
		input []any
		leak  string
	}{
		{"malformed image data url", toolImgImageOutput([]any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,@@not-base64@@", "detail": "high"},
		}), "@@not-base64@@"},
		{"image part missing url", toolImgImageOutput([]any{
			map[string]any{"type": "input_image", "detail": "high"},
		}), toolImgFixtureB64},
		{"image result without matching call", orphan, toolImgFixtureB64},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			mode := map[bool]string{false: "non-stream", true: "stream"}[stream]
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				m, f, host := toolImgManager(t, toolImgChatCompletion)
				body := toolImgBody(t, tc.input)
				var env pluginabi.Envelope
				if stream {
					env = decodeEnv(t, mustHandle(t, m, "executor.execute_stream",
						execStreamReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", body, "tool-img-down")))
				} else {
					env = decodeEnv(t, mustHandle(t, m, "executor.execute",
						execReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", body, false)))
				}
				if env.OK || env.Error == nil || strings.TrimSpace(env.Error.Message) == "" {
					t.Fatalf("invalid image tool output must fail with a descriptive error: %+v", env.Error)
				}
				if tc.leak != "" && strings.Contains(env.Error.Message, tc.leak) {
					t.Fatalf("error message leaked image data: %q", env.Error.Message)
				}
				if got := host.inferenceRequests(); got != 0 {
					t.Fatalf("fake host received %d inference requests, want 0", got)
				}
				if got := len(f.callsOf(pluginabi.MethodHostHTTPDoStream)); got != 0 {
					t.Fatalf("do_stream calls = %d, want 0", got)
				}
			})
		}
	}
}
