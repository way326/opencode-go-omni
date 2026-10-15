package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The config key intentionally differs from the selected credential. These
// cases exercise the dispatch boundary, not only the key-reading helper.
func TestSelectedCredentialExecutionAcrossProtocols(t *testing.T) {
	const selectedKey = "sk-selected-not-the-config-key"
	cases := []struct {
		model, format, body, response string
		frames                        []string
	}{
		{"glm-5.3", "openai", ccRequestBody, ccResponseBody, chatSSEFrames},
		{"minimax-m3", "claude", claudeRequestBody, claudeResponseBody, messagesSSEFrames},
		{"gpt-5.6-luna", "openai-response", `{"input":"hi"}`, responsesPassthrough, responsesSSEFrames},
		{geminiModel, "gemini", geminiRequest, geminiResponse, []string{"data: " + geminiResponse + "\r\n\r\n"}},
	}
	for _, tc := range cases {
		for _, shape := range []string{"metadata", "storage"} {
			for _, streaming := range []bool{false, true} {
				mode := "nonstream"
				if streaming {
					mode = "stream"
				}
				t.Run(tc.format+"/"+shape+"/"+mode, func(t *testing.T) {
					closed := make(chan struct{})
					frames := append([]string(nil), tc.frames...)
					for i := range frames {
						frames[i] = strings.ReplaceAll(frames[i], "qwen3.8-max", tc.model)
					}
					stream := streamResponder(streamScript{upstreamID: "up-selected", frames: frames})
					m, f := geminiManager(t, func(method string, payload []byte) ([]byte, error) {
						if method == pluginabi.MethodHostHTTPDo {
							return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(tc.response)}), nil
						}
						if method == pluginabi.MethodHostStreamClose {
							close(closed)
						}
						return stream(method, payload)
					})
					if selectedKey == testKey {
						t.Fatal("fixture must distinguish scheduled key from config key")
					}
					req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: "selected.json", AuthProvider: ProviderID,
						Model: "opencode-go/" + tc.model, SourceFormat: tc.format, Format: tc.format,
						OriginalRequest: []byte(tc.body), Payload: []byte(tc.body), Stream: streaming,
						Headers: http.Header{"X-Opencode-Session": {"selected-session"}},
					}, StreamID: "down-selected", HostCallbackID: "selected-callback"}
					if shape == "metadata" {
						req.AuthMetadata = map[string]any{"type": ProviderID, "api_key": selectedKey}
					} else {
						req.StorageJSON = mustJSON(map[string]string{"type": ProviderID, "api_key": selectedKey})
					}
					method, upstreamMethod := pluginabi.MethodExecutorExecute, pluginabi.MethodHostHTTPDo
					if streaming {
						method, upstreamMethod = pluginabi.MethodExecutorExecuteStream, pluginabi.MethodHostHTTPDoStream
					}
					raw, err := m.HandleCall(method, mustJSON(req))
					if err != nil || !decodeEnv(t, raw).OK {
						t.Fatalf("selected credential execution failed: err=%v envelope=%s", err, raw)
					}
					if streaming {
						select {
						case <-closed:
						case <-time.After(3 * time.Second):
							t.Fatal("selected credential stream did not complete")
						}
						for _, call := range f.callsOf(pluginabi.MethodHostStreamClose) {
							var closeRequest map[string]any
							if json.Unmarshal(call.payload, &closeRequest) != nil || closeRequest["error"] != nil {
								t.Fatal("selected credential stream closed with an error")
							}
						}
						if len(f.callsOf(pluginabi.MethodHostStreamEmit)) == 0 {
							t.Fatal("selected credential stream produced no events")
						}
					}
					var wire hostHTTPReq
					calls := f.callsOf(upstreamMethod)
					if len(calls) == 0 || json.Unmarshal(calls[len(calls)-1].payload, &wire) != nil {
						t.Fatal("selected credential inference callback missing")
					}
					credential, expected := wire.Headers.Get("Authorization"), "Bearer "+selectedKey
					if tc.format == "claude" {
						credential, expected = wire.Headers.Get("X-Api-Key"), selectedKey
						if wire.Headers.Get("Authorization") != "" || wire.Headers.Get("Anthropic-Version") == "" {
							t.Fatal("Messages authentication header contract changed")
						}
					}
					if credential != expected {
						t.Fatal("executor replaced the scheduled credential with a config key")
					}
					if wire.Headers.Get("X-Opencode-Session") != "selected-session" || wire.HostCallbackID != "selected-callback" {
						t.Fatal("session or host callback context lost")
					}
				})
			}
		}
	}
}

func TestSelectedCredentialExecutionDoesNotFallbackToConfig(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		m, f := newExecManager(t)
		req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: ProviderID, Model: "opencode-go/glm-5.3", SourceFormat: "openai",
			OriginalRequest: []byte(ccRequestBody), Stream: streaming,
		}, StreamID: "down-no-selected-key"}
		method := pluginabi.MethodExecutorExecute
		if streaming {
			method = pluginabi.MethodExecutorExecuteStream
		}
		raw, err := m.HandleCall(method, mustJSON(req))
		env := decodeEnv(t, raw)
		if err != nil || env.OK || env.Error == nil {
			t.Fatal("missing selected credential was not rejected")
		}
		// Registration fetched /models once. No inference may leave the plugin.
		if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 1 || len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != 0 {
			t.Fatal("missing selected credential contacted inference upstream")
		}
		if strings.Contains(string(raw), testKey) {
			t.Fatal("credential error leaked a configured key")
		}
	}
}
