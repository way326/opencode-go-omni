package chatcompletions

import (
	"testing"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

// Post-terminal stream lifecycle pins (Chat Completions -> claude /
// openai-response synthesis). The upstream may deliver the include_usage
// trailer and then keep the connection open without ever sending [DONE]:
// once the client terminal is complete the converter must report done so
// the executor stops reading, the deferred terminal must carry the trailer
// counts, and a genuinely truncated upstream must surface through Finish
// instead of closing as a successful stream.

const (
	ccLifecycleContent   = `data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant","content":"x"}}]}`
	ccLifecycleFinish    = `data: {"id":"r","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`
	ccLifecycleUsage     = `data: {"id":"r","model":"m","choices":[]}`
	ccLifecycleTrailer   = `data: {"id":"r","model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
	ccLifecycleFinalTail = `data: {"id":"r","model":"m","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`
)

// feedStep feeds one complete SSE frame and reports the translated events
// plus the converter's done verdict.
func feedStep(t *testing.T, sc *StreamConverter, line string) ([]sseEvt, bool) {
	t.Helper()
	evs, done, eErr := sc.Feed([]byte(line + "\n\n"))
	if eErr != nil {
		t.Fatalf("feed %q: %v", line, eErr)
	}
	return parseEvents(t, evs), done
}

// finishEvents runs Finish and requires it to succeed.
func finishEvents(t *testing.T, sc *StreamConverter) []sseEvt {
	t.Helper()
	evs, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish: %v", eErr)
	}
	return parseEvents(t, evs)
}

func completedUsage(t *testing.T, ev sseEvt) (float64, float64) {
	t.Helper()
	resp, ok := ev.Data["response"].(map[string]any)
	if !ok {
		t.Fatalf("terminal event has no response object: %v", ev)
	}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("terminal response has no usage: %v", resp)
	}
	in, _ := u["input_tokens"].(float64)
	out, _ := u["output_tokens"].(float64)
	return in, out
}

func claudeDeltaUsage(t *testing.T, ev sseEvt) (float64, float64) {
	t.Helper()
	u, ok := ev.Data["usage"].(map[string]any)
	if !ok {
		t.Fatalf("message_delta has no usage: %v", ev)
	}
	in, _ := u["input_tokens"].(float64)
	out, _ := u["output_tokens"].(float64)
	return in, out
}

// TestStreamResponsesUsageTrailerTerminatesStream pins the main 499
// scenario at the converter seam: after finish_reason, the choice-less
// include_usage trailer completes response.completed, which is the client's
// terminal — the converter must report done instead of waiting for a
// [DONE] the upstream may never send.
func TestStreamResponsesUsageTrailerTerminatesStream(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	if evs, done := feedStep(t, sc, ccLifecycleContent); done || len(evs) == 0 {
		t.Fatalf("content frame: events=%d done=%t", len(evs), done)
	}
	if evs, done := feedStep(t, sc, ccLifecycleFinish); done || len(evs) != 0 {
		t.Fatalf("finish_reason must defer response.completed: %v done=%t", evs, done)
	}
	evs, done := feedStep(t, sc, ccLifecycleTrailer)
	if !done {
		t.Fatal("the include_usage trailer must end the stream: response.completed is terminal")
	}
	if n := len(evs); n == 0 || evs[n-1].Name != "response.completed" {
		t.Fatalf("trailer must deliver the terminal last: %v", evs)
	}
	terminal := 0
	for _, ev := range evs {
		if ev.Name != "response.completed" {
			continue
		}
		terminal++
		if in, out := completedUsage(t, ev); in != 11 || out != 7 {
			t.Fatalf("terminal usage = (%v,%v), want (11,7)", in, out)
		}
	}
	if terminal != 1 {
		t.Fatalf("response.completed emitted %d times, want exactly 1", terminal)
	}
	if evs := finishEvents(t, sc); len(evs) != 0 {
		t.Fatalf("Finish after a delivered terminal must emit nothing: %v", evs)
	}
	if evs := finishEvents(t, sc); len(evs) != 0 {
		t.Fatalf("Finish must be idempotent: %v", evs)
	}
	if evs := parseEvents(t, sc.Flush()); len(evs) != 0 {
		t.Fatalf("Flush after a delivered terminal must emit nothing: %v", evs)
	}
}

// TestStreamClaudeUsageTrailerTerminatesStream pins the Messages-route
// equivalent: the trailer batch closes the deferred message_delta AND
// message_stop (Anthropic clients hang without it) and reports done.
func TestStreamClaudeUsageTrailerTerminatesStream(t *testing.T) {
	sc := NewStreamConverter("claude")
	feedStep(t, sc, ccLifecycleContent)
	if evs, done := feedStep(t, sc, ccLifecycleFinish); done || len(evs) != 1 || evs[0].Name != "content_block_stop" {
		t.Fatalf("finish_reason must close blocks and defer message_delta: %v done=%t", evs, done)
	}
	evs, done := feedStep(t, sc, ccLifecycleTrailer)
	if !done {
		t.Fatal("the include_usage trailer must complete the Messages stream")
	}
	if len(evs) != 2 || evs[0].Name != "message_delta" || evs[1].Name != "message_stop" {
		t.Fatalf("trailer batch = %v, want [message_delta message_stop]", evs)
	}
	if in, out := claudeDeltaUsage(t, evs[0]); in != 11 || out != 7 {
		t.Fatalf("message_delta usage = (%v,%v), want (11,7)", in, out)
	}
	if evs := finishEvents(t, sc); len(evs) != 0 {
		t.Fatalf("Finish after message_stop must emit nothing: %v", evs)
	}
	if evs := parseEvents(t, sc.Flush()); len(evs) != 0 {
		t.Fatalf("Flush after message_stop must emit nothing: %v", evs)
	}
}

// TestStreamChoiceLessFrameWithoutUsageKeepsTerminalPending pins F6: an
// accounting frame that carries no counts yet must not release the terminal
// or end the stream, because the usage trailer may still be in flight and
// its counts have to land in the terminal event.
func TestStreamChoiceLessFrameWithoutUsageKeepsTerminalPending(t *testing.T) {
	for _, format := range []string{"claude", "openai-response"} {
		t.Run(format, func(t *testing.T) {
			sc := NewStreamConverter(format)
			feedStep(t, sc, ccLifecycleContent)
			feedStep(t, sc, ccLifecycleFinish)
			if evs, done := feedStep(t, sc, ccLifecycleUsage); done || len(evs) != 0 {
				t.Fatalf("a choice-less frame without usage must not deliver or end the terminal: %v done=%t", evs, done)
			}
			evs, done := feedStep(t, sc, ccLifecycleFinalTail)
			if !done {
				t.Fatal("the trailing usage frame must end the stream")
			}
			if format == "claude" {
				if len(evs) != 2 || evs[0].Name != "message_delta" || evs[1].Name != "message_stop" {
					t.Fatalf("trailer batch = %v, want [message_delta message_stop]", evs)
				}
				if in, out := claudeDeltaUsage(t, evs[0]); in != 9 || out != 4 {
					t.Fatalf("message_delta usage = (%v,%v), want (9,4)", in, out)
				}
				return
			}
			if n := len(evs); n == 0 || evs[n-1].Name != "response.completed" {
				t.Fatalf("trailer must deliver the terminal last: %v", evs)
			}
			if in, out := completedUsage(t, evs[len(evs)-1]); in != 9 || out != 4 {
				t.Fatalf("terminal usage = (%v,%v), want (9,4)", in, out)
			}
		})
	}
}

// TestStreamFinishLegalCloseRendersTerminalOnce pins close-without-[DONE]:
// a finish_reason frame may legally be the last upstream bytes, and Finish
// must render the held terminal exactly once.
func TestStreamFinishLegalCloseRendersTerminalOnce(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		feedStep(t, sc, ccLifecycleContent)
		feedStep(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
		evs := finishEvents(t, sc)
		if len(evs) != 2 || evs[0].Name != "message_delta" || evs[1].Name != "message_stop" {
			t.Fatalf("Finish = %v, want [message_delta message_stop]", evs)
		}
		if in, out := claudeDeltaUsage(t, evs[0]); in != 3 || out != 4 {
			t.Fatalf("message_delta usage = (%v,%v), want (3,4)", in, out)
		}
		if again := finishEvents(t, sc); len(again) != 0 {
			t.Fatalf("second Finish must emit nothing: %v", again)
		}
	})
	t.Run("responses", func(t *testing.T) {
		sc := NewStreamConverter("openai-response")
		feedStep(t, sc, ccLifecycleContent)
		feedStep(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
		evs := finishEvents(t, sc)
		if n := len(evs); n == 0 || evs[n-1].Name != "response.completed" {
			t.Fatalf("Finish must end in response.completed: %v", evs)
		}
		if in, out := completedUsage(t, evs[len(evs)-1]); in != 3 || out != 4 {
			t.Fatalf("terminal usage = (%v,%v), want (3,4)", in, out)
		}
		if again := finishEvents(t, sc); len(again) != 0 {
			t.Fatalf("second Finish must emit nothing: %v", again)
		}
	})
}

// TestStreamFinishDONEFinalLineWithoutNewline pins the legal EOF shape where
// the upstream closes right after its last line without a trailing newline.
func TestStreamFinishDONEFinalLineWithoutNewline(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		feedStep(t, sc, ccLifecycleContent)
		feedStep(t, sc, ccLifecycleFinish)
		if evs, done, eErr := sc.Feed([]byte("data: [DONE]")); eErr != nil || done || len(evs) != 0 {
			t.Fatalf("unterminated final line must buffer: %v done=%t err=%v", evs, done, eErr)
		}
		evs := finishEvents(t, sc)
		if len(evs) != 2 || evs[0].Name != "message_delta" || evs[1].Name != "message_stop" {
			t.Fatalf("Finish = %v, want [message_delta message_stop]", evs)
		}
	})
	t.Run("responses", func(t *testing.T) {
		sc := NewStreamConverter("openai-response")
		feedStep(t, sc, ccLifecycleContent)
		feedStep(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":5}}`)
		if evs, done, eErr := sc.Feed([]byte("data: [DONE]")); eErr != nil || done || len(evs) != 0 {
			t.Fatalf("unterminated final line must buffer: %v done=%t err=%v", evs, done, eErr)
		}
		evs := finishEvents(t, sc)
		if n := len(evs); n == 0 || evs[n-1].Name != "response.completed" {
			t.Fatalf("Finish must end in response.completed: %v", evs)
		}
		if in, out := completedUsage(t, evs[len(evs)-1]); in != 2 || out != 5 {
			t.Fatalf("terminal usage = (%v,%v), want (2,5)", in, out)
		}
	})
}

// TestStreamFinishTruncatedStreamFails pins that a genuinely truncated
// upstream never closes as a success: EOF without any finish_reason and an
// incomplete trailing fragment both fail, for the synthesized targets and
// for byte passthrough, and no terminal is fabricated.
func TestStreamFinishTruncatedStreamFails(t *testing.T) {
	t.Run("eof without finish_reason", func(t *testing.T) {
		for _, format := range []string{"claude", "openai-response"} {
			sc := NewStreamConverter(format)
			feedStep(t, sc, ccLifecycleContent)
			evs, eErr := sc.Finish()
			if eErr == nil {
				t.Fatalf("%s: EOF without finish_reason must fail", format)
			}
			if eErr.Class != errclass.ClassUpstream {
				t.Fatalf("%s: class = %q, want %q", format, eErr.Class, errclass.ClassUpstream)
			}
			if len(evs) != 0 {
				t.Fatalf("%s: failing Finish must not emit events: %v", format, evs)
			}
			if again, againErr := sc.Finish(); len(again) != 0 || againErr != eErr {
				t.Fatalf("%s: Finish must be idempotent: events=%v err=%v", format, again, againErr)
			}
		}
	})
	t.Run("incomplete trailing fragment", func(t *testing.T) {
		for _, format := range []string{"claude", "openai-response", "openai"} {
			sc := NewStreamConverter(format)
			if evs, done, eErr := sc.Feed([]byte(`data: {"id":"c","choices":[{"delta":{"content":"hel`)); eErr != nil || done || len(evs) != 0 {
				t.Fatalf("%s: partial line must buffer: %v done=%t err=%v", format, evs, done, eErr)
			}
			evs, eErr := sc.Finish()
			if eErr == nil {
				t.Fatalf("%s: an incomplete trailing fragment must fail", format)
			}
			if len(evs) != 0 {
				t.Fatalf("%s: failing Finish must not emit events: %v", format, evs)
			}
		}
	})
	t.Run("unterminated non-data fragment", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		if _, _, eErr := sc.Feed([]byte("event: message_stop")); eErr != nil {
			t.Fatalf("unterminated non-data line must buffer: %v", eErr)
		}
		if evs, eErr := sc.Finish(); eErr == nil || len(evs) != 0 {
			t.Fatalf("unterminated trailing line must fail: err=%v events=%v", eErr, evs)
		}
	})
}

// TestStreamOpenAIPassthroughFinish pins byte passthrough: legal frames are
// forwarded untouched, a legal final line without its trailing newline is
// forwarded the same way, and only a fragment that cannot be a legal chunk
// is rejected. A clean passthrough EOF stays successful — the endpoint
// writes the trailing [DONE] itself.
func TestStreamOpenAIPassthroughFinish(t *testing.T) {
	chunk := `{"id":"c4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	sc := NewStreamConverter("openai")
	evs, done, eErr := sc.Feed([]byte("data: " + chunk + "\n"))
	if eErr != nil || done {
		t.Fatalf("passthrough feed: err=%v done=%t", eErr, done)
	}
	if len(evs) != 1 || string(evs[0]) != chunk {
		t.Fatalf("passthrough must forward bytes verbatim: %q", evs)
	}
	tail := `{"id":"c4","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	if evs, done, eErr := sc.Feed([]byte("data: " + tail)); eErr != nil || done || len(evs) != 0 {
		t.Fatalf("unterminated final line must buffer: %v done=%t err=%v", evs, done, eErr)
	}
	fin, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("legal passthrough close must succeed: %v", eErr)
	}
	if len(fin) != 1 || string(fin[0]) != tail {
		t.Fatalf("final passthrough payload must stay byte-identical: %q", fin)
	}
	if evs := sc.Flush(); len(evs) != 0 {
		t.Fatalf("passthrough Flush must stay a no-op: %v", evs)
	}

	bare := NewStreamConverter("openai")
	if evs, _, eErr := bare.Feed([]byte("data: " + chunk + "\n\n")); eErr != nil || len(evs) != 1 {
		t.Fatalf("passthrough feed: %v err=%v", evs, eErr)
	}
	if fin, eErr := bare.Finish(); eErr != nil || len(fin) != 0 {
		t.Fatalf("clean passthrough EOF must stay successful: err=%v events=%v", eErr, fin)
	}
}
