package gemini

import (
	"strings"
	"testing"
)

// This file pins the *framing* contract this adapter relies on, because the
// four targets do not agree on it and getting it wrong is silent corruption:
// a doubled `data:` prefix for gemini/openai, or a missing `event:` line for
// claude/openai-response. The expectations are anchored to CPA v8.0.28's own
// HTTP handlers, which are the consumers of these frames:
//
//   - gemini  `sdk/api/handlers/gemini/gemini_handlers.go` WriteChunk writes
//     `"data: "` + chunk + `"\n\n"`, so the chunk must be raw JSON.
//   - openai  `sdk/api/handlers/openai/openai_handlers.go` writes
//     `"data: %s\n\n"`, so the chunk must be raw JSON.
//   - claude  `sdk/api/handlers/claude/code_handlers.go` writes the chunk
//     verbatim, so it must already be a complete SSE block.
//   - openai-response `sdk/api/handlers/openai/openai_responses_handlers.go`
//     feeds chunks through its SSE framer, so they must be complete blocks.

const framingStreamChunk = `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}],` +
	`"modelVersion":"gemini-3-pro","responseId":"r1"}` + "\n\n"

// TestStreamFramingMatchesTargetHandlerContract asserts, per target, whether
// the emitted frames are raw JSON payloads or complete SSE blocks.
func TestStreamFramingMatchesTargetHandlerContract(t *testing.T) {
	cases := []struct {
		format      string
		wantEntries []string // must appear somewhere in the target's output
		wantRawJSON bool     // true: every frame is a bare JSON object
	}{
		{FormatGemini, []string{`"candidates"`, `"text":"Hello"`}, true},
		{FormatOpenAI, []string{`"chat.completion.chunk"`, `"content":"Hello"`}, true},
		{FormatClaude, []string{
			"event: message_start\ndata: ", "event: content_block_delta\ndata: ", `"text":"Hello"`,
		}, false},
		{FormatOpenAIResp, []string{
			"event: response.created\ndata: ", "event: response.output_text.delta\ndata: ", `"delta":"Hello"`,
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			sc := NewStreamConverterWithContext(tc.format, upstreamModel, nil, nil)
			events, _, eErr := sc.Feed([]byte(framingStreamChunk))
			if eErr != nil {
				t.Fatalf("Feed failed: %v", eErr)
			}
			if len(events) == 0 {
				t.Fatalf("no events emitted for %s", tc.format)
			}
			all := joinFrames(events)
			for _, want := range tc.wantEntries {
				if !strings.Contains(all, want) {
					t.Fatalf("output lost %q: %s", want, all)
				}
			}
			for i, evt := range events {
				text := string(evt)
				if tc.wantRawJSON {
					if strings.HasPrefix(text, "data:") || strings.Contains(text, "\ndata:") ||
						strings.HasPrefix(text, "event:") {
						t.Fatalf("frame %d is already framed, but the %s handler adds framing: %s", i, tc.format, text)
					}
					if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
						t.Fatalf("frame %d is not a bare JSON object: %s", i, text)
					}
					continue
				}
				// A translated frame may concatenate several blocks, but every
				// block boundary must be a proper SSE terminator so the
				// claude/Responses writers can consume it.
				if !strings.HasSuffix(text, "\n\n") {
					t.Fatalf("frame %d is not a complete SSE block: %q", i, text)
				}
			}
		})
	}
}

// TestStreamTailContentIsNeverCutOffByFinishReason covers the ordering trap:
// Gemini emits finishReason *before* usageMetadata and thought-signature
// frames. Feed must not report completion at the finish reason, every trailing
// fact must reach the client, and the client's terminal event must appear
// exactly once across Feed and Finish.
func TestStreamTailContentIsNeverCutOffByFinishReason(t *testing.T) {
	// frame 1: finishReason, no usage. frame 2: usage + a thought signature.
	upstream := `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"finishReason":"STOP"}],` +
		`"modelVersion":"gemini-3-pro","responseId":"r1"}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":" tail","thoughtSignature":"SIG-TAIL"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":9,"totalTokenCount":19},` +
		`"modelVersion":"gemini-3-pro","responseId":"r1"}` + "\n\n"

	cases := []struct {
		format       string
		terminal     string   // "" for raw-JSON targets whose chunks carry the facts
		wantInOutput []string // must be present in Feed+Finish output
		// wantSignature is how the target represents the trailing
		// thoughtSignature: verbatim for the native route, a signed carrier
		// for claude/openai-response, and absent for openai (Chat Completions
		// has no signature field, which is CPA's own native behavior).
		wantSignature string
	}{
		{FormatGemini, "", []string{"tail", `"usageMetadata"`, `"totalTokenCount":19`}, "SIG-TAIL"},
		{FormatOpenAI, "", []string{"tail", `"completion_tokens":9`, `"total_tokens":19`}, ""},
		{FormatClaude, "event: message_stop", []string{
			"event: content_block_delta\ndata: ", `"output_tokens":9`,
		}, `"signature":"SIG-TAIL"`},
		{FormatOpenAIResp, "event: response.completed", []string{
			`"status":"completed"`, `"output_tokens":9`, `"total_tokens":19`,
		}, "U0lHLVRBSUw"},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			sc := NewStreamConverterWithContext(tc.format, upstreamModel, nil, nil)
			events, done, eErr := sc.Feed([]byte(upstream))
			if eErr != nil {
				t.Fatalf("Feed failed: %v", eErr)
			}
			if done {
				t.Fatalf("Feed reported completion; the stream must be read to EOF, not stopped at finishReason")
			}
			tail, eErr := sc.Finish()
			if eErr != nil {
				t.Fatalf("Finish failed: %v", eErr)
			}
			all := joinFrames(events) + joinFrames(tail)
			for _, want := range tc.wantInOutput {
				if !strings.Contains(all, want) {
					t.Fatalf("output lost %q: %s", want, all)
				}
			}
			if tc.terminal != "" {
				if got := strings.Count(all, tc.terminal); got != 1 {
					t.Fatalf("terminal event %q appeared %d times, want exactly 1: %s", tc.terminal, got, all)
				}
			}
			// The trailing frame must never be dropped. Whether its signature
			// survives depends on the target protocol, not on this adapter.
			if tc.wantSignature == "" {
				if strings.Contains(all, "SIG-TAIL") || strings.Contains(all, "U0lHLVRBSUw") {
					t.Fatalf("openai target invented a signature representation: %s", all)
				}
			} else if !strings.Contains(all, tc.wantSignature) {
				t.Fatalf("trailing signature (%s) was not carried to %s: %s", tc.wantSignature, tc.format, all)
			}

			// Finish is idempotent: a repeated call must not re-emit.
			again, eErr := sc.Finish()
			if eErr != nil {
				t.Fatalf("second Finish failed: %v", eErr)
			}
			if len(again) != 0 {
				t.Fatalf("second Finish re-emitted %d frames: %s", len(again), joinFrames(again))
			}
		})
	}
}

func joinFrames(frames [][]byte) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f)
	}
	return b.String()
}
