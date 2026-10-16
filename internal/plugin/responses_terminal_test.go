package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/catalog"
)

// WP-1 pins for the gate and terminal buffer: only recognized Codex HTTP
// Responses requests over a Chat Completions upstream may defer the single
// successful terminal, and no non-canonical or trailing chunk may leak a
// terminal ahead of the EOF commit.

func rtGateRequest(source, format string, stream bool, header http.Header) executorRequest {
	return executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		SourceFormat: source,
		Format:       format,
		Stream:       stream,
		Headers:      header,
	}}
}

func rtFrame(name, data string) []byte {
	return []byte("event: " + name + "\ndata: " + data + "\n\n")
}

func TestShouldDeferResponsesTerminalGate(t *testing.T) {
	codexCLIUA := "codex_cli_rs/0.162.0-alpha.17.2 (Windows 11; x86_64)"
	// liveCodexDesktopUA and liveCodexDesktopOriginator are copied verbatim
	// from the local CPA error-v1-responses header capture (headers only).
	liveCodexDesktopUA := "Codex Desktop/0.162.0-alpha.17.2 (Windows 10.0.19045; x86_64) unknown (Codex Desktop; 26.1007.71326)"
	liveCodexDesktopOriginator := "Codex Desktop"
	for _, tc := range []struct {
		name    string
		source  string
		format  string
		stream  bool
		route   catalog.Route
		headers http.Header
		want    bool
	}{
		{name: "live codex desktop", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {liveCodexDesktopOriginator}, "User-Agent": {liveCodexDesktopUA}}, want: true},
		{name: "live codex desktop ua only", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {liveCodexDesktopUA}}, want: true},
		{name: "live codex desktop originator only", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {liveCodexDesktopOriginator}}, want: true},
		{name: "source format mismatch", source: "openai", format: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {liveCodexDesktopOriginator}}, want: false},
		{name: "response format mismatch", source: "openai-response", format: "openai", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {liveCodexDesktopOriginator}}, want: false},
		{name: "matching explicit formats", source: "openai-response", format: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {liveCodexDesktopOriginator}}, want: true},
		{name: "codex cli real ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: true},
		{name: "unverified session source ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex_desktop/1.4.2 (Windows NT 10.0; Win64; x64)"}}, want: false},
		{name: "unverified app-server ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex-app-server/1.0.0"}}, want: false},
		{name: "originator only", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {"codex_cli_rs"}}, want: true},
		{name: "unrecognized originator with cli ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {"codex_desktop"}, "User-Agent": {codexCLIUA}}, want: true},
		{name: "multiple originator values", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {"something-else", "codex_cli_rs"}}, want: true},
		{name: "unknown originator with codex ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {"something-else"}, "User-Agent": {codexCLIUA}}, want: true},
		{name: "originator case and spacing", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"Originator": {"  codex   desktop  "}}, want: true},
		{name: "unknown ua", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"MyApp/1.0"}}, want: false},
		{name: "ua contains codex token", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"Mozilla/5.0 codex_cli_rs/1.0"}}, want: false},
		{name: "ua prefix collision", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex_cli_rs_extra/1.0"}}, want: false},
		{name: "ua without version", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex_cli_rs"}}, want: false},
		{name: "ua empty version", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex_cli_rs/ (x)"}}, want: false},
		{name: "ua extra path segment", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {"codex_cli_rs/1.0/extra"}}, want: false},
		{name: "no identifying headers", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{}, want: false},
		{name: "nil headers", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: nil, want: false},
		{name: "upstream not chat", source: "openai-response", stream: true, route: catalog.RouteResponses,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: false},
		{name: "upstream messages", source: "openai-response", stream: true, route: catalog.RouteMessages,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: false},
		{name: "downstream openai chat", source: "openai", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: false},
		{name: "downstream gemini", source: "gemini", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: false},
		{name: "non streaming", source: "openai-response", stream: false, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}}, want: false},
		{name: "websocket upgrade", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, want: false},
		{name: "websocket connection token", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}, "Connection": {"keep-alive, Upgrade"}}, want: false},
		{name: "websocket upgrade token only", source: "openai-response", stream: true, route: catalog.RouteChatCompletions,
			headers: http.Header{"User-Agent": {codexCLIUA}, "Upgrade": {"WebSocket"}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldDeferResponsesTerminal(rtGateRequest(tc.source, tc.format, tc.stream, tc.headers), tc.route)
			if got != tc.want {
				t.Fatalf("shouldDeferResponsesTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResponsesTerminalBufferCanonicalPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk []byte
	}{
		{name: "delta event", chunk: rtFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`)},
		{name: "created data only", chunk: []byte(`data: {"type":"response.created","response":{"id":"r"}}` + "\n\n")},
		{name: "done sentinel", chunk: []byte("data: [DONE]\n\n")},
		{name: "event without data", chunk: []byte("event: ping\n\n")},
		{name: "undecodable non-terminal data", chunk: []byte("data: {oops}\n\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			ordinary, err := buffer.accept(tc.chunk)
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			if !bytes.Equal(ordinary, tc.chunk) {
				t.Fatalf("ordinary bytes changed: %q -> %q", tc.chunk, ordinary)
			}
			if _, terminalErr := buffer.terminal(); terminalErr == nil {
				t.Fatal("terminal() unexpectedly reported success for an ordinary-only stream")
			}
		})
	}
}

func TestResponsesTerminalBufferDefersSuccessTerminal(t *testing.T) {
	largeText, _ := json.Marshal(strings.Repeat("你好\n", 20000))
	for _, tc := range []struct {
		name     string
		terminal string
	}{
		{name: "minimal", terminal: `{"type":"response.completed","response":{"status":"completed"}}`},
		{name: "data only frame", terminal: `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":7}}}`},
		{name: "explicit null errors", terminal: `{"type":"response.completed","error":null,"response":{"status":"completed","error":null}}`},
		{name: "large multiline escaped", terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + string(largeText) + `}]}],"usage":{"input_tokens":13,"output_tokens":7,"input_tokens_details":{"cached_tokens":5}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			ordinaryFrame := rtFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`)
			ordinary, err := buffer.accept(ordinaryFrame)
			if err != nil || !bytes.Equal(ordinary, ordinaryFrame) {
				t.Fatalf("ordinary accept = %q, %v", ordinary, err)
			}
			terminalFrame := rtFrame("response.completed", tc.terminal)
			held, err := buffer.accept(terminalFrame)
			if err != nil {
				t.Fatalf("terminal accept: %v", err)
			}
			if held != nil {
				t.Fatalf("terminal frame was not held back: %q", held)
			}
			out, err := buffer.terminal()
			if err != nil {
				t.Fatalf("terminal(): %v", err)
			}
			want := append([]byte("data: "), []byte(tc.terminal)...)
			if !bytes.Equal(out, want) {
				t.Fatalf("terminal() = %.120q..., want %.120q...", out, want)
			}
			if bytes.Contains(out, []byte("event:")) || bytes.HasSuffix(out, []byte("\n")) {
				t.Fatalf("terminal() must be data-only without trailing newline: %.80q", out)
			}
			// terminal() is read-only and may be queried again before commit.
			again, err := buffer.terminal()
			if err != nil || !bytes.Equal(again, want) {
				t.Fatalf("terminal() is not repeatable: %v", err)
			}
		})
	}
}

func TestResponsesTerminalBufferTerminalAsFirstChunk(t *testing.T) {
	var buffer responsesTerminalBuffer
	terminal := `{"type":"response.completed","response":{"status":"completed"}}`
	held, err := buffer.accept(rtFrame("response.completed", terminal))
	if err != nil || held != nil {
		t.Fatalf("first-chunk terminal accept = %q, %v", held, err)
	}
	out, err := buffer.terminal()
	if err != nil {
		t.Fatalf("terminal(): %v", err)
	}
	if string(out) != "data: "+terminal {
		t.Fatalf("terminal() = %q", out)
	}
}

func TestResponsesTerminalBufferRejectsNonSuccessCompleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk []byte
	}{

		{name: "status missing", chunk: rtFrame("response.completed", `{"type":"response.completed","response":{"usage":{"input_tokens":1}}}`)},
		{name: "top level error set", chunk: rtFrame("response.completed", `{"type":"response.completed","error":{"code":"boom"},"response":{"status":"completed"}}`)},
		{name: "response error set", chunk: rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed","error":{"code":"boom"}}}`)},
		{name: "framing mismatch", chunk: rtFrame("response.completed", `{"type":"response.incomplete","response":{"status":"incomplete"}}`)},
		{name: "undecodable payload", chunk: rtFrame("response.completed", `{"type":`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			ordinary, err := buffer.accept(tc.chunk)
			if err == nil {
				t.Fatalf("accept unexpectedly succeeded with %q", ordinary)
			}
			if ordinary != nil {
				t.Fatalf("rejected terminal returned %q", ordinary)
			}
			if _, terminalErr := buffer.terminal(); terminalErr == nil {
				t.Fatal("terminal() reported success after a rejected terminal")
			}
			// The rejection is sticky: a later clean terminal cannot commit.
			if _, laterErr := buffer.accept(rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)); laterErr == nil {
				t.Fatal("clean terminal accepted after rejection")
			}
		})
	}
}

func TestResponsesTerminalBufferNonSuccessTerminalPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk []byte
	}{
		{name: "length truncation completed incomplete", chunk: rtFrame("response.completed", `{"type":"response.completed","response":{"status":"incomplete","usage":{"input_tokens":11,"output_tokens":7},"output":[{"type":"message","content":[{"type":"output_text","text":"truncated"}]}]}}`)},
		{name: "response.failed", chunk: rtFrame("response.failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"boom"}}}`)},
		{name: "response.incomplete", chunk: rtFrame("response.incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			ordinary, err := buffer.accept(tc.chunk)
			if err != nil {
				t.Fatalf("non-success terminal accept: %v", err)
			}
			if !bytes.Equal(ordinary, tc.chunk) {
				t.Fatalf("non-success terminal bytes changed: %q", ordinary)
			}
			// (nil, nil) tells the executor the terminal was already
			// forwarded in its original framing; nothing is committed.
			out, err := buffer.terminal()
			if err != nil || out != nil {
				t.Fatalf("terminal() = %q, %v; want nil, nil", out, err)
			}
			// The forwarded terminal is the last chunk.
			if _, laterErr := buffer.accept(rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)); laterErr == nil {
				t.Fatal("clean terminal accepted after a forwarded non-success terminal")
			}
		})
	}
}

func TestResponsesTerminalBufferRejectsTrailingChunks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trailer []byte
	}{
		{name: "ordinary event", trailer: rtFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"late"}`)},
		{name: "duplicate terminal", trailer: rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			if _, err := buffer.accept(rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)); err != nil {
				t.Fatalf("terminal accept: %v", err)
			}
			if _, err := buffer.accept(tc.trailer); err == nil {
				t.Fatal("trailing chunk accepted after a buffered terminal")
			}
			if out, err := buffer.terminal(); err == nil {
				t.Fatalf("withheld terminal leaked after trailing chunk: %q", out)
			}
		})
	}
}

func TestResponsesTerminalBufferRejectsNonCanonicalChunks(t *testing.T) {
	completedFrame := rtFrame("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)
	for _, tc := range []struct {
		name   string
		chunk  []byte
		expect string
	}{
		{name: "mixed ordinary then terminal", chunk: append(append([]byte{}, rtFrame("response.output_item.done", `{"type":"response.output_item.done"}`)...), completedFrame...), expect: "response.completed"},
		{name: "two ordinary frames", chunk: append(append([]byte{}, rtFrame("response.created", `{"type":"response.created"}`)...), rtFrame("response.created", `{"type":"response.created"}`)...)},
		{name: "keepalive comment", chunk: []byte(": keepalive\n\n")},
		{name: "empty chunk", chunk: []byte{}},
		{name: "crlf frame", chunk: []byte("event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\"}\r\n\r\n")},
		{name: "multi data lines", chunk: []byte("event: response.output_text.delta\ndata: {\"type\":\ndata: \"response.output_text.delta\"}\n\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer responsesTerminalBuffer
			ordinary, err := buffer.accept(tc.chunk)
			if err == nil {
				t.Fatalf("non-canonical chunk accepted as %q", ordinary)
			}
			if ordinary != nil {
				t.Fatalf("non-canonical chunk forwarded: %q", ordinary)
			}
			if tc.expect != "" && !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("diagnostic %q does not name %q", err, tc.expect)
			}
			if out, terminalErr := buffer.terminal(); terminalErr == nil {
				t.Fatalf("terminal() leaked %q", out)
			}
		})
	}
}
