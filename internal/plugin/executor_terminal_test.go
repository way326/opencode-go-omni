package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

func TestExecutorEOFDeferredTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		codex, fail  bool
	}{
		{name: "codex success", codex: true},
		{name: "codex optional DONE", codex: true, suffix: "data: [DONE]\n\n"},
		{name: "ordinary client unchanged"},
		{name: "codex rejects trailing error", codex: true, suffix: "data: {\"error\":\"late failure\"}\n\n", fail: true},
		{name: "codex rejects duplicate usage", codex: true, suffix: chatStreamUsageChunk("eof1", "glm-5.3", 9, 9), fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{
				chatStreamChunk("eof1", "glm-5.3", `{"role":"assistant","content":"你好\nEOF"}`, ""),
				chatStreamChunk("eof1", "glm-5.3", `{}`, "stop"),
				chatStreamUsageChunk("eof1", "glm-5.3", 11, 7) + tc.suffix,
			}
			m, f := newStreamManager(t, streamScript{upstreamID: "up-eof", frames: frames})
			var req executorRequest
			_ = json.Unmarshal(execStreamReqBody("opencode-go/glm-5.3", "openai-response", []byte(responsesLifecycleBody), streamLifecycleDownID), &req)
			if tc.codex {
				req.Headers = http.Header{"User-Agent": {"codex_cli_rs/0.162.0"}}
			}
			body, _ := json.Marshal(req)
			resp, err := m.HandleCall("executor.execute_stream", body)
			if err != nil || !decodeEnv(t, resp).OK {
				t.Fatalf("execute_stream: %v %s", err, resp)
			}
			if !m.bridge.WaitForInFlight(5 * time.Second) {
				t.Fatal("stream did not drain")
			}
			down := downstreamClosePayloads(t, f)
			if len(down) != 1 || upstreamCloseCount(f) != 1 {
				t.Fatalf("close counts down=%d up=%d", len(down), upstreamCloseCount(f))
			}
			message, _ := down[0]["error"].(string)
			if (message != "") != tc.fail {
				t.Fatalf("downstream error=%q wantFailure=%v", message, tc.fail)
			}
			blob := emittedStreamBlob(t, f)
			if tc.fail {
				if strings.Contains(blob, "response.completed") {
					t.Fatal("success escaped before tail validation")
				}
				return
			}
			in, out, count := responsesTerminalUsage(t, blob)
			if in != 11 || out != 7 || count != 1 {
				t.Fatalf("terminal usage=%d/%d count=%d", in, out, count)
			}
			emits := f.callsOf(pluginabi.MethodHostStreamEmit)
			last := string(wireBody(t, decodePayload(t, emits[len(emits)-1]), "payload"))
			if tc.codex {
				if !strings.HasPrefix(last, "data: {") || strings.Contains(last, "event:") || strings.HasSuffix(last, "\n") {
					t.Fatalf("terminal was not deferred to EOF: %.150q", last)
				}
			} else if !strings.HasPrefix(last, "event: response.completed\n") || !strings.HasSuffix(last, "\n\n") {
				t.Fatalf("ordinary client's framing changed: %.150q", last)
			}
			// Emission of the final terminal must be followed by normal close;
			// no late event or watchdog error may appear between those callbacks.
			calls := f.recorded()
			for i, call := range calls {
				if call.method == pluginabi.MethodHostStreamClose && (i == 0 || calls[i-1].method != pluginabi.MethodHostStreamEmit) {
					t.Fatal("terminal emit and downstream close were not adjacent")
				}
			}
		})
	}
}

type eofFinishRejectingConverter struct{}

func (eofFinishRejectingConverter) Feed([]byte) ([][]byte, bool, *errclass.Error) {
	return [][]byte{[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}, true, nil
}

func (eofFinishRejectingConverter) Finish() ([][]byte, *errclass.Error) {
	return nil, errclass.Translation("synthetic Finish validation failure")
}

func TestExecutorEOFFinishFailureWithholdsSuccess(t *testing.T) {
	f := &fakeCaller{responder: streamResponder(streamScript{upstreamID: "up-finish"})}
	m := NewManager(NewHostBridge(f.call))
	m.pumpStreamWithConverter(streamLifecycleDownID, "up-finish", &resolvedExecution{cfg: config.Config{
		RequestTimeout: time.Second, MaxResponseBytes: config.DefaultMaxResponseBytes,
	}}, "openai-response", true, eofFinishRejectingConverter{})
	if !m.bridge.WaitForInFlight(time.Second) {
		t.Fatal("Finish failure did not drain")
	}
	if len(f.callsOf(pluginabi.MethodHostStreamEmit)) != 0 {
		t.Fatal("candidate success emitted before Finish passed")
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 || !strings.Contains(down[0]["error"].(string), "Finish validation failure") {
		t.Fatalf("wrong failure close: %+v", down)
	}
}

type eofSyntheticConverter struct {
	feed, finish [][]byte
}

func (c eofSyntheticConverter) Feed([]byte) ([][]byte, bool, *errclass.Error) {
	return c.feed, true, nil
}

func (c eofSyntheticConverter) Finish() ([][]byte, *errclass.Error) {
	return c.finish, nil
}

func TestExecutorEOFTerminalOwnership(t *testing.T) {
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n")
	delta := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")
	for _, tc := range []struct {
		name string
		conv eofSyntheticConverter
		fail bool
	}{
		{name: "terminal is first frame", conv: eofSyntheticConverter{feed: [][]byte{terminal}}},
		{name: "terminal produced by Finish", conv: eofSyntheticConverter{finish: [][]byte{terminal}}},
		{name: "duplicate in Feed", conv: eofSyntheticConverter{feed: [][]byte{terminal, terminal}}, fail: true},
		{name: "duplicate in Finish", conv: eofSyntheticConverter{feed: [][]byte{terminal}, finish: [][]byte{terminal}}, fail: true},
		{name: "late delta in Finish", conv: eofSyntheticConverter{feed: [][]byte{terminal}, finish: [][]byte{delta}}, fail: true},
		{name: "missing terminal", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: streamResponder(streamScript{upstreamID: "up-owned"})}
			m := NewManager(NewHostBridge(f.call))
			m.pumpStreamWithConverter(streamLifecycleDownID, "up-owned", &resolvedExecution{cfg: config.Config{
				RequestTimeout: time.Second, MaxResponseBytes: config.DefaultMaxResponseBytes,
			}}, "openai-response", true, tc.conv)
			if !m.bridge.WaitForInFlight(time.Second) {
				t.Fatal("commit did not drain")
			}
			emits := f.callsOf(pluginabi.MethodHostStreamEmit)
			wantEmits := 1
			if tc.fail {
				wantEmits = 0
			}
			if len(emits) != wantEmits {
				t.Fatalf("emits=%d want=%d", len(emits), wantEmits)
			}
			down := downstreamClosePayloads(t, f)
			if len(down) != 1 || upstreamCloseCount(f) != 1 {
				t.Fatalf("close counts down=%d up=%d", len(down), upstreamCloseCount(f))
			}
			message, _ := down[0]["error"].(string)
			if (message != "") != tc.fail {
				t.Fatalf("close error=%q wantFailure=%v", message, tc.fail)
			}
		})
	}
}

func TestExecutorEOFLengthKeepsIncompleteTerminal(t *testing.T) {
	frames := []string{
		chatStreamChunk("length1", "glm-5.3", `{"role":"assistant","content":"partial"}`, ""),
		chatStreamChunk("length1", "glm-5.3", `{}`, "length"),
		chatStreamUsageChunk("length1", "glm-5.3", 11, 7),
	}
	m, f := newStreamManager(t, streamScript{upstreamID: "up-length", frames: frames})
	var req executorRequest
	_ = json.Unmarshal(execStreamReqBody("opencode-go/glm-5.3", "openai-response", []byte(responsesLifecycleBody), streamLifecycleDownID), &req)
	req.Headers = http.Header{"Originator": {"Codex Desktop"}}
	body, _ := json.Marshal(req)
	resp, err := m.HandleCall("executor.execute_stream", body)
	if err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("execute_stream: %v %s", err, resp)
	}
	if !m.bridge.WaitForInFlight(5 * time.Second) {
		t.Fatal("incomplete stream did not drain")
	}
	down := downstreamClosePayloads(t, f)
	if len(down) != 1 || down[0]["error"] != nil || upstreamCloseCount(f) != 1 {
		t.Fatalf("incomplete terminal changed into a transport failure: down=%v up=%d", down, upstreamCloseCount(f))
	}
	emits := f.callsOf(pluginabi.MethodHostStreamEmit)
	last := string(wireBody(t, decodePayload(t, emits[len(emits)-1]), "payload"))
	if !strings.HasPrefix(last, "event: response.completed\n") || !strings.HasSuffix(last, "\n\n") ||
		!strings.Contains(last, `"status":"incomplete"`) || strings.Contains(last, `"status":"completed"`) {
		t.Fatalf("incomplete terminal was changed or deferred: %.400q", last)
	}
	in, out, count := responsesTerminalUsage(t, emittedStreamBlob(t, f))
	if in != 11 || out != 7 || count != 1 {
		t.Fatalf("incomplete terminal usage=%d/%d count=%d", in, out, count)
	}
}
