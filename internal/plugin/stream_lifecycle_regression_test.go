package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Regression pins for the post-terminal stream lifecycle. The scenarios are
// derived from the local CPA 499 investigation: the Chat -> Responses pump
// must stop once the terminal event has been delivered, must never turn a
// genuinely truncated stream into a successful terminal, must close the
// upstream exactly once when the request-timeout watchdog races the pump,
// and must not let a wedged upstream close strand the downstream close.

const (
	streamLifecycleDownID = "down-lifecycle"
	streamLifecycleWait   = 5 * time.Second

	// responsesLifecycleBody is a minimal, legal Responses request body. The
	// post-finish lifecycle pins must run through the Chat -> Responses
	// conversion path, so the client source format is openai-response.
	responsesLifecycleBody = `{"model":"x","input":[{"role":"user","content":"hi"}]}`
)

func chatStreamChunk(id, model, delta, finish string) string {
	choice := map[string]any{"index": 0, "delta": json.RawMessage(delta)}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []any{choice},
	})
	return "data: " + string(raw) + "\n\n"
}

func chatStreamUsageChunk(id, model string, prompt, completion int) string {
	raw, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	})
	return "data: " + string(raw) + "\n\n"
}

// streamLifecycleManager builds a manager whose streaming callbacks are served
// by responder, with an optional config override appended to the base YAML.
func streamLifecycleManager(t *testing.T, yamlSuffix string, responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, responder)
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML+yamlSuffix)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

func streamLifecycleStart(t *testing.T, m *Manager) pluginabi.Envelope {
	t.Helper()
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai-response", []byte(responsesLifecycleBody), streamLifecycleDownID))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream envelope = %+v", env.Error)
	}
	return env
}

// emittedStreamBlob concatenates every downstream emit payload for the
// stream under test.
func emittedStreamBlob(t *testing.T, f *fakeCaller) string {
	t.Helper()
	var blob strings.Builder
	for _, e := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		wire := decodePayload(t, e)
		if id, _ := wire["stream_id"].(string); id != streamLifecycleDownID {
			t.Fatalf("emit stream_id = %q, want %q", id, streamLifecycleDownID)
		}
		blob.Write(wireBody(t, wire, "payload"))
	}
	return blob.String()
}

func downstreamClosePayloads(t *testing.T, f *fakeCaller) []map[string]any {
	t.Helper()
	calls := f.callsOf(pluginabi.MethodHostStreamClose)
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, decodePayload(t, c))
	}
	return out
}

func upstreamCloseCount(f *fakeCaller) int {
	return len(f.callsOf(pluginabi.MethodHostHTTPStreamClose))
}

func responsesTerminalUsage(t *testing.T, blob string) (int64, int64, int) {
	t.Helper()
	terminal := 0
	var input, output int64
	for _, block := range strings.Split(blob, "\n\n") {
		if !strings.Contains(block, "response.completed") {
			continue
		}
		terminal++
		for _, line := range strings.Split(block, "\n") {
			payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
			if !ok {
				continue
			}
			var ev struct {
				Response struct {
					Usage struct {
						InputTokens  int64 `json:"input_tokens"`
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev) == nil {
				input = ev.Response.Usage.InputTokens
				output = ev.Response.Usage.OutputTokens
			}
		}
	}
	return input, output, terminal
}

// TestStreamLifecyclePostFinishUpstreamHang pins the main 499 scenario: the
// upstream delivered a finish_reason chunk plus the include_usage trailer, then
// never sent [DONE] and kept the stream open. The pump already emitted the
// deferred response.completed, so it must stop reading and close cleanly
// instead of waiting for the request-timeout watchdog and reporting a timeout
// on a request that was fully delivered.
func TestStreamLifecyclePostFinishUpstreamHang(t *testing.T) {
	hang := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hang) }) }
	t.Cleanup(release)

	frames := []string{
		chatStreamChunk("c1", "glm-5.3", `{"role":"assistant","content":"done"}`, ""),
		chatStreamChunk("c1", "glm-5.3", `{}`, "stop"),
		chatStreamUsageChunk("c1", "glm-5.3", 11, 7),
	}
	idx := 0
	responder := func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "up-hang",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if idx < len(frames) {
				p := frames[idx]
				idx++
				return hostOK(hostStreamReadResp{Payload: []byte(p)}), nil
			}
			<-hang // upstream never terminates until the stream is closed
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			release()
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			release()
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
	m, f := streamLifecycleManager(t, "request-timeout: 250ms\n", responder)
	streamLifecycleStart(t, m)
	if !m.bridge.WaitForInFlight(streamLifecycleWait) {
		t.Fatalf("pump did not finish after terminal event (in-flight callbacks stuck)")
	}

	blob := emittedStreamBlob(t, f)
	input, output, terminal := responsesTerminalUsage(t, blob)
	if terminal != 1 {
		downDiag := downstreamClosePayloads(t, f)
		t.Fatalf("terminal event emitted %d times, want exactly 1; down=%v emits=%d upstreamReads=%d blob=%.400q",
			terminal, downDiag, len(f.callsOf(pluginabi.MethodHostStreamEmit)),
			len(f.callsOf(pluginabi.MethodHostHTTPStreamRead)), blob)
	}
	if input != 11 || output != 7 {
		t.Fatalf("terminal usage = (%d,%d), want (11,7); the include_usage trailer must land in the terminal event", input, output)
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(down))
	}
	if msg, _ := down[0]["error"].(string); msg != "" {
		t.Fatalf("downstream must close cleanly after a delivered terminal, got error %q; upCloses=%d blob=%.300q", msg, upstreamCloseCount(f), blob)
	}
	if got := upstreamCloseCount(f); got != 1 {
		t.Fatalf("upstream closes = %d, want exactly 1", got)
	}
}

// TestStreamLifecycleTruncationMustFail pins that a genuinely truncated
// upstream never yields a fabricated success terminal: malformed JSON and a
// clean EOF without finish_reason must both close the downstream with an error
// and emit no response.completed, while a passthrough client cannot be handed
// a clean end for a malformed chunk.
func TestStreamLifecycleTruncationMustFail(t *testing.T) {
	cases := []struct {
		name   string
		format string
		body   string
		frames []string
	}{
		{
			name:   "responses truncated json",
			format: "openai-response",
			body:   responsesLifecycleBody,
			frames: []string{`data: {"id":"c2","choices":[{"delta":{"content":"hel`},
		},
		{
			name:   "responses eof without finish",
			format: "openai-response",
			body:   responsesLifecycleBody,
			frames: []string{chatStreamChunk("c3", "glm-5.3", `{"content":"hello"}`, "")},
		},
		{
			name:   "chat passthrough partial json",
			format: "openai",
			body:   ccRequestBody,
			frames: []string{`data: {"id":"c4","choices":[{"delta":{"content":"hel`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f := streamLifecycleManager(t, "", streamResponder(streamScript{
				upstreamID: "up-truncated",
				frames:     tc.frames,
			}))
			resp, err := m.HandleCall("executor.execute_stream",
				execStreamReqBody("opencode-go/glm-5.3", tc.format, []byte(tc.body), streamLifecycleDownID))
			if err != nil {
				t.Fatalf("execute_stream: %v", err)
			}
			env := decodeEnv(t, resp)
			if !env.OK {
				t.Fatalf("execute_stream envelope = %+v", env.Error)
			}
			if !m.bridge.WaitForInFlight(streamLifecycleWait) {
				t.Fatalf("pump did not finish for truncated stream")
			}
			down := downstreamClosePayloads(t, f)
			if len(down) != 1 {
				t.Fatalf("downstream closes = %d, want 1", len(down))
			}
			msg, _ := down[0]["error"].(string)
			if strings.TrimSpace(msg) == "" {
				t.Fatalf("truncated stream must close downstream with an explicit error, got clean close (upstream reads=%d, emits=%d)",
					len(f.callsOf(pluginabi.MethodHostHTTPStreamRead)), len(f.callsOf(pluginabi.MethodHostStreamEmit)))
			}
			if blob := emittedStreamBlob(t, f); strings.Contains(blob, "response.completed") {
				t.Fatalf("truncated stream must not fabricate response.completed:\n%s", blob)
			}
		})
	}
}

// TestStreamLifecycleWatchdogClosesUpstreamOnce pins the close accounting when
// the request-timeout watchdog and the pump race: the upstream stream must be
// closed exactly once, by whichever path wins.
func TestStreamLifecycleWatchdogClosesUpstreamOnce(t *testing.T) {
	hang := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hang) }) }
	t.Cleanup(release)

	responder := func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "up-watchdog",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			<-hang // never deliver anything until the watchdog closes the stream
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			release()
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
	m, f := streamLifecycleManager(t, "request-timeout: 200ms\n", responder)
	streamLifecycleStart(t, m)
	if !m.bridge.WaitForInFlight(streamLifecycleWait) {
		t.Fatalf("pump did not finish after watchdog timeout")
	}
	if got := upstreamCloseCount(f); got != 1 {
		t.Fatalf("upstream closes = %d, want exactly 1 (watchdog and pump must not both close); down=%v", got, downstreamClosePayloads(t, f))
	}
}

// blockedCloseResponder scripts an upstream whose FIRST stream_close genuinely
// wedges: it releases the parked read and then blocks until closeReleased,
// while later stream_close callers (the request-timeout watchdog) return
// immediately so the pump can still reach its downstream close. If frames is
// empty the first read parks until readReleased; otherwise the frames are
// served in order and only the post-frames read parks.
func blockedCloseResponder(releaseRead func(), readReleased, closeReleased, firstCloseReturned chan struct{}, frames []string, upstreamID string) func(string, []byte) ([]byte, error) {
	var (
		idx       int
		closeOnce sync.Once
	)
	return func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   upstreamID,
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if idx < len(frames) {
				p := frames[idx]
				idx++
				return hostOK(hostStreamReadResp{Payload: []byte(p)}), nil
			}
			<-readReleased
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			blocked := false
			closeOnce.Do(func() { blocked = true })
			if !blocked {
				// The watchdog must not wedge behind the pump's blocked close;
				// it still has to release the parked read so the pump can
				// observe the timeout and run its own close path.
				releaseRead()
				return hostOK(map[string]any{}), nil
			}
			releaseRead()
			<-closeReleased
			close(firstCloseReturned)
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			releaseRead()
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
}

// waitDownstreamClose waits for the downstream stream end to be delivered. On
// the broken lifecycle the upstream close is attempted first and blocks, so
// this deadline is what surfaces the stranded client.
func waitDownstreamClose(t *testing.T, f *fakeCaller, deadline time.Duration) []map[string]any {
	t.Helper()
	end := time.Now().Add(deadline)
	for len(f.callsOf(pluginabi.MethodHostStreamClose)) == 0 && time.Now().Before(end) {
		time.Sleep(10 * time.Millisecond)
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 {
		t.Fatalf("downstream close did not arrive while upstream stream_close was wedged; closes = %d", len(down))
	}
	return down
}

// TestStreamLifecycleBlockedUpstreamCloseStillEndsDownstream pins that a host
// whose upstream stream_close wedges cannot strand the downstream close: on the
// request-timeout path the client must receive the timeout error while the
// upstream close is still blocked. A completed reply is tested separately.
func TestStreamLifecycleBlockedUpstreamCloseStillEndsDownstream(t *testing.T) {
	readReleased := make(chan struct{})
	closeReleased := make(chan struct{})
	var readOnce, closeOnce sync.Once
	releaseRead := func() { readOnce.Do(func() { close(readReleased) }) }
	releaseClose := func() { closeOnce.Do(func() { close(closeReleased) }) }

	firstCloseReturned := make(chan struct{})
	m, f := streamLifecycleManager(t, "request-timeout: 250ms\n",
		blockedCloseResponder(releaseRead, readReleased, closeReleased, firstCloseReturned, nil, "up-blocked-close"))
	// Register after the manager's shutdown cleanup: testing runs cleanups
	// in reverse order, so a failed assertion must release callbacks first.
	t.Cleanup(func() {
		releaseRead()
		releaseClose()
	})
	streamLifecycleStart(t, m)

	down := waitDownstreamClose(t, f, 3*time.Second)
	if msg, _ := down[0]["error"].(string); strings.TrimSpace(msg) == "" {
		t.Fatalf("timeout path must end downstream with the timeout error; got clean close")
	}
	select {
	case <-firstCloseReturned:
		t.Fatalf("downstream close was delivered only after the wedged upstream close returned; the client was stranded behind it")
	default:
	}
	if upstreamCloseCount(f) == 0 {
		t.Fatalf("test precondition failed: upstream stream_close was never attempted")
	}
	if m.bridge.WaitForInFlight(200 * time.Millisecond) {
		t.Fatalf("bridge reported drained while the first upstream stream_close was still wedged")
	}
	releaseClose()
	if !m.bridge.WaitForInFlight(streamLifecycleWait) {
		t.Fatalf("pump did not finish after the wedged upstream close was released")
	}
}

// TestStreamLifecycleBlockedUpstreamCloseCleanTerminal pins the same blocked
// close on the clean terminal path: once the upstream delivered a terminal
// chunk set, the downstream end must arrive cleanly while the upstream close is
// still wedged, and the bridge must only report drained after the close is
// released.
func TestStreamLifecycleBlockedUpstreamCloseCleanTerminal(t *testing.T) {
	readReleased := make(chan struct{})
	closeReleased := make(chan struct{})
	var readOnce, closeOnce sync.Once
	releaseRead := func() { readOnce.Do(func() { close(readReleased) }) }
	releaseClose := func() { closeOnce.Do(func() { close(closeReleased) }) }

	frames := []string{
		chatStreamChunk("c5", "glm-5.3", `{"role":"assistant","content":"done"}`, ""),
		chatStreamChunk("c5", "glm-5.3", `{}`, "stop"),
		chatStreamUsageChunk("c5", "glm-5.3", 3, 2),
	}
	firstCloseReturned := make(chan struct{})
	m, f := streamLifecycleManager(t, "request-timeout: 500ms\n",
		blockedCloseResponder(releaseRead, readReleased, closeReleased, firstCloseReturned, frames, "up-clean-blocked"))
	// Register after the manager's shutdown cleanup: testing runs cleanups
	// in reverse order, so a failed assertion must release callbacks first.
	t.Cleanup(func() {
		releaseRead()
		releaseClose()
	})
	streamLifecycleStart(t, m)

	down := waitDownstreamClose(t, f, 3*time.Second)
	if msg, _ := down[0]["error"].(string); strings.TrimSpace(msg) != "" {
		t.Fatalf("clean terminal path ended downstream with error %q while the upstream close was wedged", msg)
	}
	select {
	case <-firstCloseReturned:
		t.Fatalf("downstream close was delivered only after the wedged upstream close returned; the client was stranded behind it")
	default:
	}
	if upstreamCloseCount(f) == 0 {
		t.Fatalf("test precondition failed: upstream stream_close was never attempted")
	}
	if m.bridge.WaitForInFlight(200 * time.Millisecond) {
		t.Fatalf("bridge reported drained while the clean-path upstream stream_close was still wedged")
	}
	releaseClose()
	if !m.bridge.WaitForInFlight(streamLifecycleWait) {
		t.Fatalf("pump did not finish after the wedged upstream close was released")
	}
}
