// Streaming response conversion for the native Gemini route (FR-006).
//
// Upstream `:streamGenerateContent?alt=sse` delivers arbitrary HTTP chunks of
// an SSE byte stream whose `data:` payloads are Gemini
// GenerateContentResponse chunks. Feed reassembles the framing
// (CRLF or LF line endings, multi-line data, UTF-8 code points split across
// chunks) and hands each payload to the registered gemini→target transformer.
//
// Emitted frames are exactly what each target's CPA HTTP handler expects: raw
// JSON chunks for gemini (whose handler adds `data: ` framing) and openai
// (likewise), and complete `event: …\ndata: …\n\n` SSE blocks for claude and
// openai-response (whose handlers write or reframe them verbatim). See the
// framing note in gemini.go.
package gemini

import (
	"context"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// StreamConverter converts an upstream Gemini SSE stream into the client
// protocol incrementally.
//
// Contract: instances are single-use and strictly sequential — Feed is called
// from one goroutine with consecutive network chunks and there is no internal
// locking. Feed never reports completion for content chunks, not even for the
// frame carrying finishReason: Gemini appends usageMetadata and detached
// thought-signature frames after it, so the stream is read to EOF and the
// client's terminal events are flushed once from Finish.
type StreamConverter struct {
	framer *shared.SSEFramer
	source sdktranslator.Format

	model      string
	original   []byte
	translated []byte
	param      any

	// emitted reports whether any client frame was produced, which decides
	// whether a late failure may still be retried (§7 post-first-byte rule).
	emitted bool
	// sawFinishReason, sawPromptFeedback, and sentinelSeen are the terminal
	// observations Finish validates: Gemini marks termination with a
	// finishReason, a safety block reports promptFeedback, and some relays
	// append an explicit [DONE] sentinel. Any one of them is a complete
	// stream; none of them is a truncation.
	sawFinishReason   bool
	sawPromptFeedback bool
	sentinelSeen      bool
	// terminalFlushed guards the exactly-once client terminal events: either
	// an upstream `data: [DONE]` marker or Finish's EOF flush produces them,
	// never both.
	terminalFlushed bool

	err       *errclass.Error
	finished  bool
	finishErr *errclass.Error
}

// Compile-time proof the converter satisfies the executor's streamConverter
// shape (internal/plugin cannot be imported here without a cycle).
var _ interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
	Finish() (events [][]byte, eErr *errclass.Error)
} = (*StreamConverter)(nil)

// NewStreamConverter builds a converter for sourceFormat ("gemini", "openai",
// "claude", "openai-response") without request context; unknown formats fail
// on the first Feed with ClassUnsupported, matching BuildRequest.
func NewStreamConverter(sourceFormat string, tools ...*shared.ResponseTools) *StreamConverter {
	return NewStreamConverterWithContext(sourceFormat, "", nil, nil, tools...)
}

// NewStreamConverterWithContext is the variant the executor wires: `model` is
// the resolved upstream model ID, `original` the client's own request body,
// and `translated` the Gemini request body this adapter built. CPA's
// transformers read both for tool identity and model metadata.
//
// `tools` is accepted for adapter uniformity but not consumed: CPA's
// gemini→target transformers own tool identity on this route, including the
// Responses namespace/custom restoration that the sibling routes implement
// locally with the shared registry.
func NewStreamConverterWithContext(sourceFormat, model string, original, translated []byte, tools ...*shared.ResponseTools) *StreamConverter {
	sc := &StreamConverter{
		framer:     shared.NewSSEFramer(false),
		source:     geminiToFormat(sourceFormat),
		model:      model,
		original:   original,
		translated: translated,
	}
	if _, eErr := sourceFormatOf(sourceFormat); eErr != nil {
		sc.err = eErr
	}
	return sc
}

// Feed consumes one upstream HTTP chunk and returns the client events it
// completed. done is true only for an explicit upstream `data: [DONE]`
// sentinel (the translators ignore it for gemini/openai targets and render
// the terminal events for claude/openai-response targets); every other chunk
// is translated as it arrives so a trailing usage or signature frame is never
// cut off.
func (sc *StreamConverter) Feed(chunk []byte) ([][]byte, bool, *errclass.Error) {
	if sc.err != nil {
		return nil, true, sc.err
	}
	sc.framer.Push(chunk)
	events, done, eErr := sc.drain()
	if eErr != nil {
		sc.err = eErr
		return nil, true, eErr
	}
	if done {
		sc.sentinelSeen = true
	}
	return events, done, nil
}

// Finish flushes the stream tail: a trailing block that arrived without its
// terminating blank line, then the client's terminal events, then EOF
// validation. A stream that ends without a finishReason or promptFeedback is
// reported as a truncated upstream stream, and the events gathered in this
// call are dropped (the executor closes the downstream stream with the
// error). A finishReason, a promptFeedback, or an explicit `[DONE]` sentinel
// each count as a terminal observation; only the absence of all three is a
// truncation. Finish is idempotent: a repeated call replays the recorded
// outcome.
func (sc *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if sc.finished {
		return nil, sc.finishErr
	}
	sc.finished = true
	if sc.err != nil {
		sc.finishErr = sc.err
		return nil, sc.finishErr
	}

	// Closing the pending frame with a blank line makes the shared framer
	// emit whatever it still holds: a payload that lost its trailing blank
	// line translates normally, and a truncated one is then reported as a
	// malformed chunk instead of being dropped silently.
	sc.framer.Push([]byte("\n\n"))
	events, _, eErr := sc.drain()
	if eErr != nil {
		sc.finishErr = eErr
		return nil, eErr
	}
	if !sc.terminalFlushed {
		frames, eErr := translateStreamChunk(context.Background(), sc.source, sc.model,
			sc.original, sc.translated, []byte("[DONE]"), &sc.param)
		if eErr != nil {
			sc.finishErr = eErr
			return nil, eErr
		}
		sc.terminalFlushed = true
		events = append(events, frames...)
	}
	if !sc.sawFinishReason && !sc.sawPromptFeedback && !sc.sentinelSeen {
		sc.finishErr = &errclass.Error{
			Class:      errclass.ClassUpstream,
			Message:    "gemini stream ended before any finishReason or promptFeedback (truncated upstream stream)",
			StatusCode: 502,
			Retryable:  !sc.emitted,
		}
		return nil, sc.finishErr
	}
	return events, nil
}

// drain pulls every complete frame the framer holds. A sentinel stops the
// drain: an explicit upstream terminal ends the stream.
func (sc *StreamConverter) drain() ([][]byte, bool, *errclass.Error) {
	var events [][]byte
	for {
		_, data, _, ok := sc.framer.Next()
		if !ok {
			return events, false, nil
		}
		frames, sentinel, eErr := sc.consume(data)
		if eErr != nil {
			return nil, false, eErr
		}
		events = append(events, frames...)
		if sentinel {
			return events, true, nil
		}
	}
}

// consume translates one reassembled SSE data payload. sentinel reports the
// upstream `[DONE]` marker.
func (sc *StreamConverter) consume(data string) ([][]byte, bool, *errclass.Error) {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return nil, false, nil
	}
	// An upstream that keeps writing after its own `[DONE]` sentinel has
	// nothing left to contribute; forwarding those frames would emit content
	// after the client's terminal event.
	if sc.sentinelSeen {
		return nil, false, nil
	}
	payload := []byte(trimmed)
	if shared.IsSSEDone(trimmed) {
		frames, eErr := translateStreamChunk(context.Background(), sc.source, sc.model,
			sc.original, sc.translated, []byte("[DONE]"), &sc.param)
		if eErr != nil {
			return nil, false, eErr
		}
		sc.terminalFlushed = true
		sc.emitted = sc.emitted || len(frames) > 0
		return frames, true, nil
	}
	info, eErr := classifyChunk(payload)
	if eErr != nil {
		return nil, false, eErr
	}
	if info.errMessage != "" {
		return nil, false, streamChunkError(info, payload, sc.emitted)
	}
	frames, eErr := translateStreamChunk(context.Background(), sc.source, sc.model,
		sc.original, sc.translated, payload, &sc.param)
	if eErr != nil {
		return nil, false, eErr
	}
	if info.finishReason != "" {
		sc.sawFinishReason = true
	}
	if info.blocked {
		sc.sawPromptFeedback = true
	}
	sc.emitted = sc.emitted || len(frames) > 0
	return frames, false, nil
}
