package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestReviewMessagesResponsesEOFRequiresCompletionEvidence(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cut\",\"model\":\"minimax-m3\"}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
	}
	m, f := newStreamManager(t, streamScript{upstreamID: "up-review-messages-cut", frames: frames})
	startReviewMessagesStream(t, m)
	if !m.bridge.WaitForInFlight(5 * time.Second) {
		t.Fatal("truncated Messages stream did not drain")
	}
	blob := emittedStreamBlob(t, f)
	if strings.Contains(blob, "response.completed") {
		t.Fatalf("truncated upstream produced a successful terminal: %s", blob)
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 || down[0]["error"] == nil || down[0]["error"] == "" {
		t.Fatalf("truncated stream must close with an error: %v", down)
	}
	if upstreamCloseCount(f) != 1 {
		t.Fatalf("upstream closes = %d, want 1", upstreamCloseCount(f))
	}
}

func TestReviewMessagesResponsesEOFCompletionPaths(t *testing.T) {
	for _, tc := range []struct {
		name            string
		frames          []string
		wantStatus      string
		wantInputTokens float64
		wantOutputToken float64
		wantCall        bool
	}{
		{
			name: "valid stop_reason at EOF",
			frames: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_end\",\"model\":\"minimax-m3\",\"usage\":{\"input_tokens\":4}}}\n\n",
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"complete\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}",
			},
			wantStatus: "completed", wantInputTokens: 4, wantOutputToken: 3,
		},
		{
			name: "max_tokens tool and usage at EOF",
			frames: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_tool\",\"model\":\"minimax-m3\",\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":2}}}\n\n",
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\"}}\n\n",
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":7,\"cache_creation_input_tokens\":1}}\n\n",
			},
			wantStatus: "incomplete", wantInputTokens: 8, wantOutputToken: 7, wantCall: true,
		},
		{
			name: "explicit message_stop without stop_reason",
			frames: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stop\",\"model\":\"minimax-m3\"}}\n\n",
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"complete\"}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			wantStatus: "completed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newStreamManager(t, streamScript{upstreamID: "up-review-messages-ok", frames: tc.frames})
			startReviewMessagesStream(t, m)
			if !m.bridge.WaitForInFlight(5 * time.Second) {
				t.Fatal("Messages stream did not drain")
			}
			blob := emittedStreamBlob(t, f)
			if strings.Contains(blob, "\"error\"") {
				t.Fatalf("valid completion was reported as error: %s", blob)
			}
			down := downstreamClosePayloads(t, f)
			if len(down) != 1 || down[0]["error"] != nil {
				t.Fatalf("valid stream close = %v", down)
			}
			var completed map[string]any
			for _, block := range strings.Split(blob, "\n\n") {
				if !strings.HasPrefix(block, "event: response.completed\n") {
					continue
				}
				data := strings.TrimPrefix(strings.SplitN(block, "\n", 2)[1], "data: ")
				if err := json.Unmarshal([]byte(data), &completed); err != nil {
					t.Fatalf("decode response.completed: %v", err)
				}
			}
			if completed == nil {
				t.Fatalf("response.completed missing: %s", blob)
			}
			resp := completed["response"].(map[string]any)
			if resp["status"] != tc.wantStatus {
				t.Fatalf("status = %v, want %s", resp["status"], tc.wantStatus)
			}
			usage := resp["usage"].(map[string]any)
			if usage["input_tokens"] != tc.wantInputTokens || usage["output_tokens"] != tc.wantOutputToken {
				t.Fatalf("usage = %v", usage)
			}
			if tc.wantCall {
				output := resp["output"].([]any)
				if len(output) != 1 || output[0].(map[string]any)["type"] != "function_call" {
					t.Fatalf("tool output = %v", resp["output"])
				}
			}
			if upstreamCloseCount(f) != 1 {
				t.Fatalf("upstream closes = %d, want 1", upstreamCloseCount(f))
			}
		})
	}
}

func TestReviewMessagesResponsesEOFRejectsHalfJSONFrame(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_half\",\"model\":\"minimax-m3\"}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}",
	}
	m, f := newStreamManager(t, streamScript{upstreamID: "up-review-messages-half", frames: frames})
	startReviewMessagesStream(t, m)
	if !m.bridge.WaitForInFlight(5 * time.Second) {
		t.Fatal("half-frame Messages stream did not drain")
	}
	blob := emittedStreamBlob(t, f)
	if strings.Contains(blob, "response.completed") {
		t.Fatalf("half-frame produced a successful terminal: %s", blob)
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 || down[0]["error"] == nil || down[0]["error"] == "" {
		t.Fatalf("half-frame must close with an error: %v", down)
	}
}

func startReviewMessagesStream(t *testing.T, m *Manager) {
	t.Helper()
	body, _ := json.Marshal(executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider:    ProviderID,
			AuthAttributes:  map[string]string{"api_key": testKey},
			Model:           "opencode-go/minimax-m3",
			SourceFormat:    "openai-response",
			OriginalRequest: []byte(responsesLifecycleBody),
			Stream:          true,
		},
		StreamID: streamLifecycleDownID,
	})
	resp, err := m.HandleCall("executor.execute_stream", body)
	if err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("execute_stream: %v %s", err, resp)
	}
}
