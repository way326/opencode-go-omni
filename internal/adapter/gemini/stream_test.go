package gemini

import (
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/errclass"
)

// finishOnlyFrame is one complete SSE frame with a terminal finishReason.
const finishOnlyFrame = `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"finishReason":"STOP"}],` +
	`"modelVersion":"gemini-3-pro","responseId":"r1"}`

const noTerminalFrame = `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}],"modelVersion":"gemini-3-pro","responseId":"r1"}`

// TestFeedReassemblesFragmentedFraming covers the transport shapes an HTTP
// chunk boundary can produce: a frame split across chunks, CRLF line endings,
// a UTF-8 code point split mid-sequence, and a JSON body split across multiple
// data: lines of one event.
func TestFeedReassemblesFragmentedFraming(t *testing.T) {
	const japanese = "日本語テキスト"
	frameLF := `data: {"candidates":[{"content":{"parts":[{"text":"` + japanese + `"}],"role":"model"},"finishReason":"STOP"}]}` + "\n\n"
	frameCRLF := strings.ReplaceAll(frameLF, "\n", "\r\n")

	// split points chosen to land inside the JSON body (and inside a
	// multi-byte rune, since the byte index is not rune-aligned).
	body := []byte(frameLF)
	midRune := len(`data: {"candidates":[{"content":{"parts":[{"text":"日`)
	multiLine := `data: {"candidates":[{"content":{"parts":[` + "\n" + `data: {"text":"` + japanese + `"}],"role":"model"},"finishReason":"STOP"}]}` + "\n\n"

	cases := []struct {
		name   string
		chunks []string
	}{
		{"whole frame", []string{frameLF}},
		{"split mid-json", []string{string(body[:20]), string(body[20:])}},
		{"split mid-rune", []string{string(body[:midRune]), string(body[midRune:])}},
		{"byte at a time", bytesToStrings(body)},
		{"crlf endings", []string{frameCRLF[:12], frameCRLF[12:30], frameCRLF[30:]}},
		{"multi-line data", []string{multiLine}},
		{"comment and heartbeat lines", []string{": keep-alive\n\n" + frameLF}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
			var all []byte
			for _, chunk := range tc.chunks {
				events, _, eErr := sc.Feed([]byte(chunk))
				if eErr != nil {
					t.Fatalf("Feed(%q) failed: %v", chunk, eErr)
				}
				for _, evt := range events {
					all = append(all, evt...)
				}
			}
			if _, eErr := sc.Finish(); eErr != nil {
				t.Fatalf("Finish failed: %v", eErr)
			}
			if !strings.Contains(string(all), japanese) {
				t.Fatalf("reassembled output lost the UTF-8 text: %s", all)
			}
		})
	}
}

// bytesToStrings yields one single-byte string per byte so a code point is
// necessarily split across Feed calls.
func bytesToStrings(b []byte) []string {
	out := make([]string, 0, len(b))
	for _, c := range b {
		out = append(out, string([]byte{c}))
	}
	return out
}

// TestFinishFlushesFramingWithoutTrailingBlankLine proves a stream whose last
// frame lost its blank-line terminator still reaches the client (the framer
// has no other way to know the block ended).
func TestFinishFlushesFramingWithoutTrailingBlankLine(t *testing.T) {
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	// First frame is fully terminated; the last frame has no trailing blank
	// line, so only Finish can release it.
	payload := finishOnlyFrame + "\n\n" + `data: {"candidates":[{"content":{"parts":[{"text":"tail"}]}}]}`
	if _, _, eErr := sc.Feed([]byte(payload)); eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	tail, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish failed: %v", eErr)
	}
	if !strings.Contains(joinFrames(tail), "tail") {
		t.Fatalf("the unterminated trailing frame was dropped: %s", joinFrames(tail))
	}
}

// TestFinishReportsTruncatedStreams covers EOF without a terminal signal,
// which the executor must surface rather than report a clean completion.
func TestFinishReportsTruncatedStreams(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"no terminal signal", `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n"},
		{"nothing at all", ``},
		{"truncated final frame", `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`},
		{"valid frames but no terminal", noTerminalFrame + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
			if _, _, eErr := sc.Feed([]byte(tc.payload)); eErr != nil {
				t.Fatalf("Feed failed: %v", eErr)
			}
			events, eErr := sc.Finish()
			if eErr == nil {
				t.Fatalf("Finish accepted a truncated stream (events=%s)", joinFrames(events))
			}
			if eErr.Class != errclass.ClassUpstream {
				t.Fatalf("class = %s, want %s", eErr.Class, errclass.ClassUpstream)
			}
			if events != nil {
				t.Fatalf("truncated stream returned events alongside the error: %s", joinFrames(events))
			}
		})
	}
}

// TestFinishAcceptsPromptFeedbackAsTerminal proves a safety-blocked stream is a
// complete stream, not a truncation.
func TestFinishAcceptsPromptFeedbackAsTerminal(t *testing.T) {
	blocked := `data: {"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked"}}` + "\n\n"
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	if _, _, eErr := sc.Feed([]byte(blocked)); eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	if _, eErr := sc.Finish(); eErr != nil {
		t.Fatalf("Finish rejected a promptFeedback-terminated stream: %v", eErr)
	}
}

// TestFeedHonoursUpstreamDoneSentinel proves an explicit `data: [DONE]` ends
// the stream and that a later Finish does not re-emit the terminal events.
// TestFinishAcceptsSentinelAsTerminal proves an explicit upstream `[DONE]` is
// a terminal observation in its own right: a relay that appends the sentinel
// without a finishReason must not be reported as a truncated stream.
func TestFinishAcceptsSentinelAsTerminal(t *testing.T) {
	stream := noTerminalFrame + "\n\n" + "data: [DONE]\n\n"
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	_, done, eErr := sc.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	if !done {
		t.Fatalf("Feed did not report completion for the sentinel")
	}
	if _, eErr := sc.Finish(); eErr != nil {
		t.Fatalf("Finish reported a truncation for a sentinel-terminated stream: %v", eErr)
	}
}
func TestFeedHonoursUpstreamDoneSentinel(t *testing.T) {
	stream := finishOnlyFrame + "\n\n" + "data: [DONE]\n\n"
	sc := NewStreamConverterWithContext(FormatClaude, upstreamModel, nil, nil)
	events, done, eErr := sc.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	if !done {
		t.Fatalf("Feed did not report completion for the [DONE] sentinel")
	}
	if got := strings.Count(joinFrames(events), "event: message_stop"); got != 1 {
		t.Fatalf("message_stop appeared %d times on the sentinel, want 1: %s", got, joinFrames(events))
	}
	tail, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish after sentinel failed: %v", eErr)
	}
	if len(tail) != 0 {
		t.Fatalf("Finish after sentinel re-emitted %d frames: %s", len(tail), joinFrames(tail))
	}
}

// TestFeedIgnoresFramesAfterDoneSentinel proves nothing is emitted after the
// client already saw its terminal event.
func TestFeedIgnoresFramesAfterDoneSentinel(t *testing.T) {
	stream := finishOnlyFrame + "\n\n" + "data: [DONE]\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"LATE"}]}}]}` + "\n\n"
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	events, done, eErr := sc.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	if !done {
		t.Fatalf("Feed did not report completion for the sentinel")
	}
	if strings.Contains(joinFrames(events), "LATE") {
		t.Fatalf("a frame after [DONE] was forwarded: %s", joinFrames(events))
	}
	tail, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish failed: %v", eErr)
	}
	if strings.Contains(joinFrames(tail), "LATE") {
		t.Fatalf("Finish forwarded a frame after [DONE]: %s", joinFrames(tail))
	}
}

// TestFeedRejectsMalformedChunks covers a payload that is not a Gemini
// response object. `null` is the trap: it decodes into a struct without error,
// so the shape check — not JSON validity — is what rejects it.
func TestFeedRejectsMalformedChunks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"null payload", "data: null\n\n"},
		{"array payload", "data: []\n\n"},
		{"number payload", "data: 3\n\n"},
		{"string payload", `data: "hello"` + "\n\n"},
		{"truncated json", `data: {"candidates":` + "\n\n"},
		{"html error page", "data: <html>bad gateway</html>\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
			events, _, eErr := sc.Feed([]byte(tc.payload))
			if eErr == nil {
				t.Fatalf("Feed accepted %q (events=%s)", tc.payload, joinFrames(events))
			}
			if eErr.Class != errclass.ClassUpstream {
				t.Fatalf("class = %s, want %s", eErr.Class, errclass.ClassUpstream)
			}
			if events != nil {
				t.Fatalf("malformed chunk returned events alongside the error")
			}
		})
	}
}

// TestFeedClassifiesInStreamErrors covers an error object delivered inside the
// stream: it must fail the stream rather than reach the client as content.
func TestFeedClassifiesInStreamErrors(t *testing.T) {
	frame := "data: " + `{"error":{"code":429,"message":"rate limited","status":"RESOURCE_EXHAUSTED"}}` + "\n\n"
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	events, _, eErr := sc.Feed([]byte(frame))
	if eErr == nil {
		t.Fatalf("in-stream error was accepted (events=%s)", joinFrames(events))
	}
	if eErr.Class != errclass.ClassRateLimit {
		t.Fatalf("class = %s, want %s", eErr.Class, errclass.ClassRateLimit)
	}
	if !eErr.Retryable {
		t.Fatalf("pre-emission error lost retryability")
	}
	if eErr.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", eErr.StatusCode)
	}

	// Same error after a client frame was already emitted: retryable must be
	// false because a retry would duplicate a partially delivered stream.
	sc2 := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	if _, _, eErr := sc2.Feed([]byte(finishOnlyFrame + "\n\n")); eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	_, _, eErr = sc2.Feed([]byte(frame))
	if eErr == nil {
		t.Fatalf("post-emission error was not reported")
	}
	if eErr.Retryable {
		t.Fatalf("post-first-byte error is retryable, which would duplicate a partial stream")
	}
}

// TestFeedErrorsAreSticky proves a failed stream stays failed: the executor
// may keep reading, but no further frame may be produced after an error.
func TestFeedErrorsAreSticky(t *testing.T) {
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	if _, _, eErr := sc.Feed([]byte("data: null\n\n")); eErr == nil {
		t.Fatalf("expected an error")
	}
	events, _, eErr := sc.Feed([]byte(finishOnlyFrame + "\n\n"))
	if eErr == nil {
		t.Fatalf("a failed stream accepted more input")
	}
	if events != nil {
		t.Fatalf("a failed stream produced events: %s", joinFrames(events))
	}
	if _, eErr := sc.Finish(); eErr == nil {
		t.Fatalf("Finish reported success for a failed stream")
	}
}

// TestFeedRejectsUnsupportedTargetFormat proves an unregistered client
// protocol fails loudly instead of leaking raw Gemini JSON to the client.
func TestFeedRejectsUnsupportedTargetFormat(t *testing.T) {
	sc := NewStreamConverter("cohere")
	events, _, eErr := sc.Feed([]byte(finishOnlyFrame + "\n\n"))
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("eErr = %v, want %s", eErr, errclass.ClassUnsupported)
	}
	if events != nil {
		t.Fatalf("unsupported format produced events: %s", joinFrames(events))
	}
	if _, eErr := sc.Finish(); eErr == nil {
		t.Fatalf("Finish reported success for an unsupported format")
	}
}

// TestFeedNeverReturnsNilForValidStream guards the executor contract: a valid
// stream must yield events (not a silent empty slice) before completion.
func TestFeedNeverReturnsNilForValidStream(t *testing.T) {
	sc := NewStreamConverterWithContext(FormatGemini, upstreamModel, nil, nil)
	events, _, eErr := sc.Feed([]byte(finishOnlyFrame + "\n\n"))
	if eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	if len(events) == 0 {
		t.Fatalf("valid stream produced no events")
	}
}
