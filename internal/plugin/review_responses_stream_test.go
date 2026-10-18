package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const (
	reviewResponsesCreated = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_review\",\"model\":\"gpt-5.6-luna\"}}\n\n"
	reviewResponsesDelta   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"before-terminal\"}\n\n"
	reviewResponsesDone    = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_review\",\"model\":\"gpt-5.6-luna\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"output\":[]}}\n\n"
	reviewResponsesAfter   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"after-terminal\"}\n\n"
)

func reviewResponsesBody(source string) []byte {
	switch source {
	case "openai":
		return []byte(`{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	case "claude":
		return []byte(`{"model":"x","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	default:
		return []byte(`{"model":"x","stream":true,"input":"hi"}`)
	}
}

func runReviewResponsesExecutor(t *testing.T, source string, frames []string) (*fakeCaller, string, []map[string]any, int) {
	t.Helper()
	m, f := newStreamManager(t, streamScript{upstreamID: "up-review-responses", frames: frames})
	resp, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream,
		execStreamReqBody("gpt-5.6-luna", source, reviewResponsesBody(source), streamLifecycleDownID))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("execute_stream envelope = %+v", env.Error)
	}
	if !m.bridge.WaitForInFlight(streamLifecycleWait) {
		t.Fatalf("Responses executor pump did not finish")
	}
	return f, emittedStreamBlob(t, f), downstreamClosePayloads(t, f), len(f.callsOf(pluginabi.MethodHostHTTPStreamRead))
}

func reviewDownstreamError(t *testing.T, closes []map[string]any) string {
	t.Helper()
	if len(closes) != 1 {
		t.Fatalf("downstream closes=%d, want 1: %v", len(closes), closes)
	}
	errText, _ := closes[0]["error"].(string)
	return errText
}

func reviewChatFinishReasons(t *testing.T, f *fakeCaller) []string {
	t.Helper()
	var out []string
	for _, call := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(wireBody(t, decodePayload(t, call), "payload"), &chunk); err != nil {
			continue // native SSE and Messages frames are not Chat chunks
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				out = append(out, *choice.FinishReason)
			}
		}
	}
	return out
}

func TestReviewResponsesExecutorEOFContract(t *testing.T) {
	t.Run("converted half-frame without terminal fails", func(t *testing.T) {
		f, blob, closes, reads := runReviewResponsesExecutor(t, "openai", []string{
			reviewResponsesCreated,
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial-at-eof\"}",
		})
		if errText := reviewDownstreamError(t, closes); !strings.Contains(errText, "truncated upstream stream") {
			t.Fatalf("downstream close error=%q, want truncated-stream error", errText)
		}
		if len(reviewChatFinishReasons(t, f)) != 0 || strings.Contains(blob, "response.completed") {
			t.Fatalf("truncated Responses stream emitted a terminal: %s", blob)
		}
		if reads < 3 { // created, partial tail, then EOF
			t.Fatalf("upstream reads=%d, expected the EOF seam", reads)
		}
	})

	t.Run("valid terminal block at EOF is flushed", func(t *testing.T) {
		partialTerminal := strings.TrimSuffix(reviewResponsesDone, "\n\n")
		f, blob, closes, _ := runReviewResponsesExecutor(t, "openai", []string{
			reviewResponsesCreated + reviewResponsesDelta,
			partialTerminal,
		})
		if errText := reviewDownstreamError(t, closes); errText != "" {
			t.Fatalf("valid terminal close error=%q", errText)
		}
		if got := reviewChatFinishReasons(t, f); len(got) != 1 || got[0] != "stop" {
			t.Fatalf("finish reasons=%v, want exactly [stop]; blob=%s", got, blob)
		}
	})

	t.Run("upstream failure is not converted to success", func(t *testing.T) {
		_, blob, closes, _ := runReviewResponsesExecutor(t, "openai", []string{
			reviewResponsesCreated,
			"event: response.failed\ndata: {\"status_code\":503,\"message\":\"upstream busy\"}\n\n",
		})
		if errText := reviewDownstreamError(t, closes); !strings.Contains(errText, "upstream busy") {
			t.Fatalf("failure close error=%q, want upstream message", errText)
		}
		if strings.Contains(blob, "response.completed") {
			t.Fatalf("failed Responses stream emitted a successful terminal: %s", blob)
		}
	})

	t.Run("native Responses EOF remains passthrough", func(t *testing.T) {
		_, blob, closes, _ := runReviewResponsesExecutor(t, "openai-response", []string{reviewResponsesCreated})
		if errText := reviewDownstreamError(t, closes); errText != "" {
			t.Fatalf("native EOF close error=%q, want existing clean passthrough behavior", errText)
		}
		if !strings.Contains(blob, "event: response.created") || strings.Contains(blob, "response.completed") {
			t.Fatalf("native EOF output changed or invented a terminal: %s", blob)
		}
	})

	t.Run("incomplete terminal maps to length", func(t *testing.T) {
		incomplete := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_review\",\"status\":\"incomplete\"}}\n\n"
		f, blob, closes, _ := runReviewResponsesExecutor(t, "openai", []string{reviewResponsesCreated, incomplete})
		if errText := reviewDownstreamError(t, closes); errText != "" {
			t.Fatalf("incomplete terminal close error=%q", errText)
		}
		if got := reviewChatFinishReasons(t, f); len(got) != 1 || got[0] != "length" {
			t.Fatalf("finish reasons=%v, want exactly [length]; blob=%s", got, blob)
		}
	})
}

func TestReviewResponsesExecutorStopsReadingAfterFirstTerminal(t *testing.T) {
	for _, source := range []string{"openai-response", "openai", "claude"} {
		t.Run(source, func(t *testing.T) {
			firstRead := reviewResponsesCreated + reviewResponsesDelta + reviewResponsesDone + reviewResponsesAfter
			f, blob, closes, reads := runReviewResponsesExecutor(t, source, []string{firstRead, reviewResponsesAfter})
			if errText := reviewDownstreamError(t, closes); errText != "" {
				t.Fatalf("terminal stream close error=%q", errText)
			}
			if strings.Contains(blob, "after-terminal") {
				t.Fatalf("post-terminal content was emitted: %s", blob)
			}
			if reads != 1 {
				t.Fatalf("upstream reads=%d, want exactly one read through the first terminal", reads)
			}
			if source == "openai-response" {
				if strings.Count(blob, "event: response.completed") != 1 {
					t.Fatalf("native terminal not preserved exactly once: %s", blob)
				}
			} else if source == "openai" {
				if got := reviewChatFinishReasons(t, f); len(got) != 1 || got[0] != "stop" {
					t.Fatalf("Chat finish reasons=%v, want [stop]; blob=%s", got, blob)
				}
			} else if strings.Count(blob, "event: message_stop") != 1 {
				t.Fatalf("Messages terminal not emitted exactly once: %s", blob)
			}
		})
	}
}
