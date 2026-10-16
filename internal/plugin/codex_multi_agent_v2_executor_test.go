package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/way326/opencode-go-omni/internal/adapter/chatcompletions"
	"github.com/way326/opencode-go-omni/internal/catalog"
)

func TestCodexV2OriginalAdapterReproducesFailure(t *testing.T) {
	_, e := chatcompletions.BuildRequest("deepseek-v4.1-flash", "openai-response", codexV2Fixture("PONG-391"), nil)
	if e == nil || !strings.Contains(e.Error(), `unsupported Responses input item type "agent_message"`) {
		t.Fatalf("expected original adapter failure, got %v", e)
	}
}

func codexV2Fixture(task string) []byte {
	body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{
		"type": "agent_message", "author": "/root", "recipient": "/root/worker",
		"content": []any{map[string]string{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
			map[string]string{"type": "encrypted_content", "encrypted_content": task}},
	}}})
	return body
}

func TestCodexV2PrepareAllRoutes(t *testing.T) {
	for _, route := range []catalog.Route{catalog.RouteChatCompletions, catalog.RouteMessages, catalog.RouteResponses, catalog.RouteGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(string(route)+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				original := codexV2Fixture("ORIGINAL-DO-NOT-SEND")
				payload := codexV2Fixture("完整任务\nPONG-391")
				before := bytes.Clone(payload)
				res := &resolvedExecution{rec: catalog.ModelRecord{Protocol: route, UpstreamID: "test-model"}}
				req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "openai-response", OriginalRequest: original, Payload: payload, Stream: stream}}
				out, e := prepareUpstreamRequest(res, req)
				if e != nil {
					t.Fatal(e)
				}
				var decoded any
				if json.Unmarshal(out, &decoded) != nil {
					t.Fatalf("invalid upstream JSON: %s", out)
				}
				if !strings.Contains(string(out), "PONG-391") || !strings.Contains(string(out), "/root/worker") || strings.Contains(string(out), "ORIGINAL-DO-NOT-SEND") || strings.Contains(string(out), `"agent_message"`) || strings.Contains(string(out), `"encrypted_content"`) {
					t.Fatalf("incomplete normalization: %s", out)
				}
				if !bytes.Equal(payload, before) || !bytes.Equal(res.original, original) {
					t.Fatal("mutated effective or original request")
				}
			})
		}
	}
}

func TestCodexV2ExecuteActualHostBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
			m, f, host := toolImgManager(t, toolImgChatCompletion)
			req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey},
				Model: "opencode-go/deepseek-v4.1-flash", SourceFormat: "openai-response",
				OriginalRequest: codexV2Fixture("ORIGINAL"), Payload: codexV2Fixture("V2-EXACT-TASK"), Stream: stream,
				Headers: http.Header{"X-Opencode-Session": {"child-A"}}, Metadata: map[string]any{"canonical_session_id": "child-B"},
			}, StreamID: "v2-down", HostCallbackID: "v2-callback"}
			wire, _ := json.Marshal(req)
			method := pluginabi.MethodExecutorExecute
			if stream {
				method = pluginabi.MethodExecutorExecuteStream
			}
			env := decodeEnv(t, mustHandle(t, m, method, wire))
			if !env.OK {
				t.Fatalf("execute failed: %+v", env.Error)
			}
			var out []byte
			if stream {
				m.bridge.WaitForInFlight(5 * time.Second)
				calls := f.callsOf(pluginabi.MethodHostHTTPDoStream)
				if len(calls) != 1 {
					t.Fatalf("stream requests = %d", len(calls))
				}
				var request hostHTTPReq
				_ = json.Unmarshal(calls[0].payload, &request)
				if request.Headers.Get("X-Opencode-Session") != "child-A" || request.HostCallbackID != "v2-callback" {
					t.Fatal("lost explicit session/callback")
				}
				out = request.Body
				models := toolImgCompletedModels(t, emittedEvents(t, f))
				if len(models) != 1 || models[0] != req.Model {
					t.Fatalf("terminal public model = %v", models)
				}
				assertCleanStreamClose(t, f)
			} else {
				out = host.chatRequest(t)
			}
			if !bytes.Contains(out, []byte("V2-EXACT-TASK")) || bytes.Contains(out, []byte("ORIGINAL")) || bytes.Contains(out, []byte(`"agent_message"`)) {
				t.Fatalf("incorrect host payload: %s", out)
			}
		})
	}
}

func TestCodexV2InvalidMessageNeverCallsInference(t *testing.T) {
	for _, stream := range []bool{false, true} {
		m, f, host := toolImgManager(t, toolImgChatCompletion)
		body := []byte(`{"input":[{"type":"agent_message","content":[{"type":"unknown","text":"PRIVATE-BODY"}]}]}`)
		var result []byte
		if stream {
			result = mustHandle(t, m, pluginabi.MethodExecutorExecuteStream, execStreamReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", body, "v2-invalid"))
		} else {
			result = mustHandle(t, m, pluginabi.MethodExecutorExecute, execReqBody("opencode-go/deepseek-v4.1-flash", "openai-response", body, false))
		}
		env := decodeEnv(t, result)
		if env.OK || env.Error == nil || strings.Contains(env.Error.Message, "PRIVATE-BODY") {
			t.Fatalf("invalid rejection: %s", result)
		}
		if host.inferenceRequests() != 0 || len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != 0 {
			t.Fatal("invalid input contacted inference host")
		}
	}
}
