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
)

const reviewNativeFileInput = `{"model":"client-model","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_123","filename":"notes.txt","metadata":{"revision":9007199254740993}}]}]}`

func reviewSessionExecutorRequest(model string, body []byte, stream bool, sessionID, streamID string) []byte {
	req := executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider:    ProviderID,
			AuthAttributes:  map[string]string{"api_key": testKey},
			Model:           model,
			SourceFormat:    "openai-response",
			OriginalRequest: body,
			Stream:          stream,
		},
		StreamID: streamID,
	}
	if sessionID != "" {
		req.Headers = http.Header{"X-Opencode-Session": []string{sessionID}}
	}
	encoded, _ := json.Marshal(req)
	return encoded
}

func TestExecutorNativeResponsesFileSessionAffinity(t *testing.T) {
	body := []byte(reviewNativeFileInput)
	autoSession, eErr := deriveOpenCodeSessionID("openai-response", body)
	if eErr != nil {
		t.Fatalf("derive file session: %v", eErr)
	}

	for _, stream := range []bool{false, true} {
		for _, explicitSession := range []bool{false, true} {
			name := "non-stream/derived-session"
			if stream {
				name = "stream/derived-session"
			}
			if explicitSession {
				name = strings.Replace(name, "derived-session", "explicit-session", 1)
			}
			t.Run(name, func(t *testing.T) {
				sessionID := ""
				wantSession := autoSession
				if explicitSession {
					sessionID = "caller-session"
					wantSession = sessionID
				}

				var m *Manager
				var f *fakeCaller
				if stream {
					m, f = newStreamManager(t, streamScript{
						upstreamID: "review-session-upstream",
						frames: []string{
							"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_file","status":"completed","output":[]}}` + "\n\n",
						},
					})
					resp, err := m.HandleCall("executor.execute_stream", reviewSessionExecutorRequest("gpt-5.6-luna", body, true, sessionID, "review-session-downstream"))
					if err != nil {
						t.Fatalf("execute_stream: %v", err)
					}
					if env := decodeEnv(t, resp); !env.OK {
						t.Fatalf("execute_stream envelope = %+v", env.Error)
					}
					m.bridge.WaitForInFlight(5 * time.Second)
				} else {
					m, f = newExecManager(t)
					resp, err := m.HandleCall("executor.execute", reviewSessionExecutorRequest("gpt-5.6-luna", body, false, sessionID, ""))
					if err != nil {
						t.Fatalf("execute: %v", err)
					}
					if env := decodeEnv(t, resp); !env.OK {
						t.Fatalf("execute envelope = %+v", env.Error)
					}
				}

				method := pluginabi.MethodHostHTTPDo
				if stream {
					method = pluginabi.MethodHostHTTPDoStream
				}
				calls := f.callsOf(method)
				if len(calls) == 0 {
					t.Fatalf("no upstream %s call recorded", method)
				}
				wire := decodePayload(t, calls[len(calls)-1])
				headers, ok := wire["headers"].(map[string]any)
				if !ok {
					t.Fatalf("upstream headers missing: %v", wire)
				}
				gotSession := headers["X-Opencode-Session"].([]any)[0].(string)
				if gotSession != wantSession {
					t.Fatalf("session header = %q, want %q", gotSession, wantSession)
				}
				gotBody := wireBody(t, wire, "body")
				var forwarded struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(gotBody, &forwarded); err != nil {
					t.Fatalf("decode forwarded Responses body: %v", err)
				}
				var original struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(body, &original); err != nil {
					t.Fatalf("decode original Responses body: %v", err)
				}
				gotInput, gotOK := stableAffinityJSON(forwarded.Input)
				wantInput, wantOK := stableAffinityJSON(original.Input)
				if !gotOK || !wantOK || !bytes.Equal(gotInput, wantInput) {
					t.Fatalf("native file input changed: got %s, want %s", forwarded.Input, original.Input)
				}
			})
		}
	}
}
