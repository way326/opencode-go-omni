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
	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/config"
)

const geminiModel = "gemini-4-argon-contributor"
const geminiCatalog = `{"data":[{"id":"gemini-4-argon-contributor"},{"id":"glm-5.3"},{"id":"gpt-5.6-luna"},{"id":"minimax-m3"}]}`
const geminiRequest = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
const geminiResponse = `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello","thoughtSignature":"AQID"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`

func geminiManager(t *testing.T, responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	t.Helper()
	m, f := newTestManager(wrapWithCatalog(geminiCatalog, responder))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))
	if err != nil || !decodeEnv(t, raw).OK {
		t.Fatalf("register failed: %v", err)
	}
	return m, f
}

func TestGeminiExecutorNativeAndOtherClients(t *testing.T) {
	for format, body := range map[string]string{
		"gemini":          geminiRequest,
		"openai":          ccRequestBody,
		"claude":          claudeRequestBody,
		"openai-response": `{"input":"hi"}`,
	} {
		t.Run(format, func(t *testing.T) {
			m, f := geminiManager(t, func(method string, payload []byte) ([]byte, error) {
				if method == pluginabi.MethodHostHTTPDo {
					var wire hostHTTPReq
					_ = json.Unmarshal(payload, &wire)
					if wire.HostCallbackID != "callback-A" {
						t.Error("CPA callback context ID was lost")
					}
					if wire.URL != config.DefaultBaseURL+"/models/"+geminiModel+":generateContent" {
						t.Errorf("URL = %s", wire.URL)
					}
					if wire.Headers.Get("Authorization") != "Bearer "+testKey || wire.Headers.Get("X-Goog-Api-Key") != "" {
						t.Error("authentication must use one Bearer header")
					}
					if wire.Headers.Get("X-Opencode-Session") != "conversation-A" {
						t.Error("explicit session was lost")
					}
					if !bytes.Contains(wire.Body, []byte(`"contents"`)) || bytes.Contains(wire.Body, []byte(`"messages"`)) {
						t.Errorf("non-Gemini upstream body: %s", wire.Body)
					}
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(geminiResponse)}), nil
				}
				return hostOK(map[string]any{}), nil
			})
			req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey}, Model: "opencode-go/" + geminiModel, SourceFormat: format, OriginalRequest: []byte(body), Headers: http.Header{"X-Opencode-Session": {"conversation-A"}}, Metadata: map[string]any{"canonical_session_id": "derived"}}}
			req.HostCallbackID = "callback-A"
			data, _ := json.Marshal(req)
			raw, err := m.HandleCall(pluginabi.MethodExecutorExecute, data)
			if err != nil || !decodeEnv(t, raw).OK {
				t.Fatalf("execute: %s, %v", raw, err)
			}
			var out pluginapi.ExecutorResponse
			decodeResult(t, raw, &out)
			if !bytes.Contains(out.Payload, []byte("hello")) {
				t.Errorf("text missing: %s", out.Payload)
			}
			if format == "gemini" && !bytes.Contains(out.Payload, []byte(`"thoughtSignature":"AQID"`)) {
				t.Error("native thoughtSignature lost")
			}
			if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 2 {
				t.Error("expected catalog and one inference request")
			}
		})
	}
}

func TestGeminiExecutorUsesHostPayload(t *testing.T) {
	res := &resolvedExecution{rec: catalog.ModelRecord{Protocol: catalog.RouteGemini, UpstreamID: geminiModel}}
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "gemini", OriginalRequest: []byte(geminiRequest), Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"rewritten"}]}]}`)}}
	out, err := prepareUpstreamRequest(res, req)
	if err != nil || !bytes.Contains(out, []byte("rewritten")) || bytes.Contains(out, []byte(`"hi"`)) {
		t.Fatalf("host Payload not honored: %s, %v", out, err)
	}
}

func TestGeminiExecutorOverrideAlignsBodyModelWithURL(t *testing.T) {
	res := &resolvedExecution{cfg: config.Config{BaseURL: config.DefaultBaseURL}, rec: catalog.ModelRecord{Protocol: catalog.RouteGemini, UpstreamID: geminiModel, EndpointPath: "/v1/models/gemini-custom:generateContent"}}
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "gemini", OriginalRequest: []byte(geminiRequest)}}
	body, err := prepareUpstreamRequest(res, req)
	if err != nil {
		t.Fatal(err)
	}
	u, err := executionURL(res, true)
	if err != nil || !strings.Contains(u, "/gemini-custom:streamGenerateContent?") || !bytes.Contains(body, []byte(`"model":"gemini-custom"`)) {
		t.Fatalf("override mismatch: %s %s %v", u, body, err)
	}
}

func TestGeminiExecutorStreamFragmentedAndTrailingUsage(t *testing.T) {
	first := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}]}` + "\r\n\r\n"
	tail := `data: {"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}` + "\n\n"
	script := streamResponder(streamScript{upstreamID: "up-gemini", frames: []string{first[:9], first[9:], tail[:13], tail[13:]}})
	m, f := geminiManager(t, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			var wire hostHTTPReq
			_ = json.Unmarshal(payload, &wire)
			if wire.URL != config.DefaultBaseURL+"/models/"+geminiModel+":streamGenerateContent?alt=sse" {
				t.Errorf("stream URL = %s", wire.URL)
			}
		}
		return script(method, payload)
	})
	raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, execStreamReqBody("opencode-go/"+geminiModel, "gemini", []byte(geminiRequest), "down-gemini"))
	if err != nil || !decodeEnv(t, raw).OK {
		t.Fatalf("stream: %s, %v", raw, err)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	var parts [][]byte
	for _, call := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		var wire struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal(call.payload, &wire)
		if !json.Valid(wire.Payload) || bytes.HasPrefix(wire.Payload, []byte("data:")) {
			t.Errorf("Gemini host chunk must be raw JSON: %s", wire.Payload)
		}
		parts = append(parts, wire.Payload)
	}
	if !bytes.Contains(bytes.Join(parts, nil), []byte("usageMetadata")) {
		t.Error("usage after finishReason was lost")
	}
	closes := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(closes) != 1 || strings.Contains(string(closes[0].payload), `"error"`) {
		t.Fatalf("downstream closes: %v", closes)
	}
}

func TestGeminiSessionsStableAndIndependent(t *testing.T) {
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "gemini", OriginalRequest: []byte(geminiRequest), Headers: make(http.Header)}}
	req.Headers.Set("X-Opencode-Session", "conversation-A")
	a, _ := resolveOpenCodeSessionID(req)
	req.OriginalRequest = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"reply"}]},{"role":"user","parts":[{"text":"next"}]}]}`)
	a2, _ := resolveOpenCodeSessionID(req)
	req.Headers.Set("X-Opencode-Session", "conversation-B")
	b, _ := resolveOpenCodeSessionID(req)
	if a != a2 || a == b {
		t.Fatalf("session isolation: %q %q %q", a, a2, b)
	}
}

func TestGeminiExecutorValidatesCompletionAfterEOF(t *testing.T) {
	m, f := geminiManager(t, streamResponder(streamScript{upstreamID: "up-empty", frames: []string{"data: {\"usageMetadata\":{\"totalTokenCount\":1}}\n\n"}}))
	raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, execStreamReqBody("opencode-go/"+geminiModel, "gemini", []byte(geminiRequest), "down-empty"))
	if err != nil || !decodeEnv(t, raw).OK {
		t.Fatalf("stream start failed: %v", err)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	closed := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(closed) != 1 || !strings.Contains(string(closed[0].payload), `"error"`) {
		t.Fatalf("truncated stream silently completed: %v", closed)
	}
}

func TestGeminiExecutorFragmentedHTTPError(t *testing.T) {
	m, f := geminiManager(t, streamResponder(streamScript{startStatus: http.StatusTooManyRequests, upstreamID: "up-rate", frames: []string{`{"error":{"message":"rate `, `limit reached"}}`}}))
	raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, execStreamReqBody("opencode-go/"+geminiModel, "gemini", []byte(geminiRequest), "down-rate"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnv(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 429 || !env.Error.Retryable || !strings.Contains(env.Error.Message, "rate limit reached") {
		t.Fatalf("fragmented error classification: %s", raw)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPStreamRead)) != 2 || len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)) != 1 {
		t.Fatal("error body was not drained and closed")
	}
}
