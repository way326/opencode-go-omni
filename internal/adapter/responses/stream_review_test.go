package responses

import (
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

func TestReviewConvertedTargetsRejectEOFWithoutTerminal(t *testing.T) {
	raw := frame("response.created", createdPayload) +
		frame("response.output_text.delta", `{"delta":"partial"}`)
	for _, source := range []string{"openai", "claude"} {
		t.Run(source, func(t *testing.T) {
			sc := NewStreamConverter(source)
			events, done, eErr := feedChunks(t, sc, chunked(raw))
			if eErr != nil || done || len(events) == 0 {
				t.Fatalf("Feed: events=%d done=%v err=%v", len(events), done, eErr)
			}

			flushed, finishErr := sc.Finish()
			if finishErr == nil || finishErr.Class != errclass.ClassUpstream ||
				finishErr.StatusCode != 502 || finishErr.Retryable {
				t.Fatalf("Finish error = %+v, want non-retryable truncated upstream after visible output", finishErr)
			}
			if len(flushed) != 0 {
				t.Fatalf("truncated stream emitted a terminal: %q", flushed)
			}
			if again, secondErr := sc.Finish(); len(again) != 0 || secondErr != finishErr {
				t.Fatalf("Finish is not idempotent: events=%q err=%p first=%p", again, secondErr, finishErr)
			}
		})
	}

	if _, eErr := NewStreamConverter("openai").Finish(); eErr == nil || !eErr.Retryable {
		t.Fatalf("truncation before any client frame should be retryable, got %+v", eErr)
	}
}

func TestReviewNativeEOFRemainsPassthrough(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	raw := frame("response.created", createdPayload) +
		frame("response.output_text.delta", `{"delta":"partial"}`)
	events, done, eErr := feedChunks(t, sc, chunked(raw))
	if eErr != nil || done || len(events) != 2 {
		t.Fatalf("native Feed: events=%d done=%v err=%v", len(events), done, eErr)
	}
	flushed, finishErr := sc.Finish()
	if finishErr != nil || len(flushed) != 0 {
		t.Fatalf("native EOF changed: flushed=%q err=%v", flushed, finishErr)
	}
	for _, event := range append(events, flushed...) {
		if strings.Contains(string(event), "response.completed") || strings.Contains(string(event), "response.incomplete") {
			t.Fatalf("native EOF must not invent a terminal: %q", event)
		}
	}
}

func TestReviewFinishDispatchesCompleteTailAndRejectsHalfPayload(t *testing.T) {
	valid := NewStreamConverter("openai")
	prefix := frame("response.created", createdPayload) +
		frame("response.output_text.delta", `{"delta":"hi"}`)
	events, done, eErr := valid.Feed([]byte(prefix + "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1}}}"))
	if eErr != nil || done {
		t.Fatalf("partial terminal Feed: done=%v err=%v", done, eErr)
	}
	terminal, finishErr := valid.Finish()
	if finishErr != nil || len(terminal) != 1 {
		t.Fatalf("complete terminal at EOF: events=%q err=%v", terminal, finishErr)
	}
	if got := choice0(t, payloadOf(t, terminal[0]))["finish_reason"]; got != "stop" {
		t.Fatalf("EOF terminal finish_reason=%v, want stop", got)
	}
	if again, secondErr := valid.Finish(); len(again) != 0 || secondErr != nil {
		t.Fatalf("repeated successful Finish: events=%q err=%v", again, secondErr)
	}
	if len(events) != 2 {
		t.Fatalf("prefix events=%d, want role and text", len(events))
	}

	truncated := NewStreamConverter("claude")
	if _, _, eErr := truncated.Feed([]byte(frame("response.created", createdPayload) +
		"event: response.completed\ndata: {\"response\":")); eErr != nil {
		t.Fatalf("partial JSON must be deferred to EOF: %v", eErr)
	}
	if _, finishErr := truncated.Finish(); finishErr == nil || finishErr.Class != errclass.ClassTranslation {
		t.Fatalf("half-written JSON terminal error = %+v, want translation failure", finishErr)
	}
}

func TestReviewFirstTerminalLatchesEveryTarget(t *testing.T) {
	terminal := frame("response.completed", `{"response":{"id":"r","usage":{"input_tokens":1,"output_tokens":2}}}`)
	after := frame("response.output_text.delta", `{"delta":"after-terminal"}`)
	for _, source := range []string{"openai-response", "openai", "claude"} {
		t.Run(source, func(t *testing.T) {
			sc := NewStreamConverter(source)
			prefix := frame("response.created", createdPayload) +
				frame("response.output_text.delta", `{"delta":"before-terminal"}`)
			events, done, eErr := sc.Feed([]byte(prefix + terminal + after))
			if eErr != nil || !done {
				t.Fatalf("Feed: events=%d done=%v err=%v", len(events), done, eErr)
			}
			for _, event := range events {
				if strings.Contains(string(event), "after-terminal") {
					t.Fatalf("post-terminal content was emitted in the same Feed: %q", event)
				}
			}
			if source == "openai-response" {
				if len(events) != 3 || string(events[2]) != terminal {
					t.Fatalf("native terminal changed: events=%q", events)
				}
			} else if source == "openai" {
				if got := choice0(t, payloadOf(t, events[len(events)-1]))["finish_reason"]; got != "stop" {
					t.Fatalf("completed finish_reason=%v, want stop", got)
				}
			} else if got, _ := parseSSE(t, events[len(events)-1]); got != "message_stop" {
				t.Fatalf("last Claude event=%q, want message_stop", got)
			}

			extra, extraDone, extraErr := sc.Feed([]byte(after + terminal))
			if extraErr != nil || !extraDone || len(extra) != 0 {
				t.Fatalf("later Feed after terminal: events=%q done=%v err=%v", extra, extraDone, extraErr)
			}
			if flushed, finishErr := sc.Finish(); finishErr != nil || len(flushed) != 0 {
				t.Fatalf("Finish after terminal: events=%q err=%v", flushed, finishErr)
			}
		})
	}
}

func TestReviewIncompleteIsFirstTerminalAndErrorsBeforeItSurface(t *testing.T) {
	sc := NewStreamConverter("openai")
	events, done, eErr := sc.Feed([]byte(frame("response.created", createdPayload) +
		frame("response.incomplete", `{}`) +
		frame("response.completed", `{}`)))
	if eErr != nil || !done {
		t.Fatalf("incomplete terminal Feed: done=%v err=%v", done, eErr)
	}
	if got := choice0(t, payloadOf(t, events[len(events)-1]))["finish_reason"]; got != "length" {
		t.Fatalf("first incomplete finish_reason=%v, want length", got)
	}

	failed := NewStreamConverter("openai")
	if _, _, eErr := failed.Feed([]byte(frame("response.created", createdPayload) +
		"event: error\ndata: {\"status_code\":503,\"message\":\"upstream unavailable\"}")); eErr != nil {
		t.Fatalf("partial error frame should wait for EOF: %v", eErr)
	}
	if _, finishErr := failed.Finish(); finishErr == nil || finishErr.Class != errclass.ClassUpstream || finishErr.StatusCode != 503 {
		t.Fatalf("EOF error frame = %+v, want upstream 503", finishErr)
	}
}
