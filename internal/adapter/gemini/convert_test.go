package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

// nonStreamResult is one upstream Gemini generateContent result carrying a
// text part with a thought signature plus a function call, which is the shape
// every client protocol has to represent differently.
const nonStreamResult = `{"candidates":[{"content":{"parts":[` +
	`{"text":"Hello","thoughtSignature":"SIG-TEXT"},` +
	`{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-CALL"}],` +
	`"role":"model"},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalCount":0,"totalTokenCount":15},` +
	`"modelVersion":"gemini-3-pro-upstream","responseId":"resp-1"}`

// TestConvertNonStreamResponseGeminiIsVerbatimPassthrough is the native-client
// contract: the upstream body reaches a `gemini` client byte-for-byte, so
// thoughtSignature, functionCall and functionResponse survive without any
// re-encoding (the proto-JSON client decodes those fields directly).
func TestConvertNonStreamResponseGeminiIsVerbatimPassthrough(t *testing.T) {
	body := []byte(nonStreamResult)
	out, eErr := ConvertNonStreamResponseWithContext(FormatGemini, 200, body, upstreamModel, nil, nil)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
	}
	if string(out) != nonStreamResult {
		t.Fatalf("native passthrough was re-encoded:\n got %s\nwant %s", out, nonStreamResult)
	}
	for _, want := range []string{"SIG-TEXT", "SIG-CALL", `"functionCall"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("native passthrough lost %q", want)
		}
	}
}

// TestConvertNonStreamResponseRendersEveryClientFormat proves each converted
// target carries the text, the tool call, and the terminal status.
func TestConvertNonStreamResponseRendersEveryClientFormat(t *testing.T) {
	cases := []struct {
		format     string
		wantText   string
		wantTool   string
		wantStatus []string
	}{
		{FormatOpenAI, "Hello", "lookup", []string{`"finish_reason":"tool_calls"`, `"prompt_tokens":10`, `"completion_tokens":5`}},
		{FormatClaude, "Hello", "lookup", []string{`"stop_reason":"tool_use"`, `"input_tokens":10`, `"output_tokens":5`}},
		{FormatOpenAIResp, "Hello", "lookup", []string{`"status":"completed"`, `"input_tokens":10`, `"output_tokens":5`}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			out, eErr := ConvertNonStreamResponseWithContext(tc.format, 200, []byte(nonStreamResult), upstreamModel, nil, nil)
			if eErr != nil {
				t.Fatalf("convert failed: %v", eErr)
			}
			if !json.Valid(out) {
				t.Fatalf("converted body is not valid JSON: %s", out)
			}
			text := string(out)
			for _, want := range append([]string{tc.wantText, tc.wantTool}, tc.wantStatus...) {
				if !strings.Contains(text, want) {
					t.Fatalf("converted %s body lost %q: %s", tc.format, want, text)
				}
			}
		})
	}
}

// TestConvertNonStreamResponseClassifiesUpstreamStatus pins the >=400 path to
// the shared kernel (redacted, §7-classified), never a raw upstream echo.
func TestConvertNonStreamResponseClassifiesUpstreamStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   errclass.Class
	}{
		{401, errclass.ClassAuth},
		{404, errclass.ClassInvalidModel},
		{429, errclass.ClassRateLimit},
		{500, errclass.ClassUpstream},
		{503, errclass.ClassUpstream},
	} {
		t.Run(itoaTest(tc.status), func(t *testing.T) {
			body := []byte(`{"error":{"code":` + itoaTest(tc.status) + `,"message":"upstream says no","status":"FAILED"}}`)
			out, eErr := ConvertNonStreamResponseWithContext(FormatOpenAI, tc.status, body, upstreamModel, nil, nil)
			if out != nil {
				t.Fatalf("status %d produced a body: %s", tc.status, out)
			}
			if eErr == nil || eErr.Class != tc.want {
				t.Fatalf("class = %v, want %s", eErr, tc.want)
			}
			if eErr.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", eErr.StatusCode, tc.status)
			}
		})
	}
}

// TestConvertNonStreamResponseClassifiesBodyLevelError covers Gemini relaying
// an error object under HTTP 200: it is an upstream failure, never a normal
// result handed to the client.
func TestConvertNonStreamResponseClassifiesBodyLevelError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want errclass.Class
	}{
		{"rate limited", `{"error":{"code":429,"message":"too many requests","status":"RESOURCE_EXHAUSTED"}}`, errclass.ClassRateLimit},
		// The shared classifier upgrades 429 to ClassQuota when the message
		// mentions a quota, matching the sibling routes.
		{"quota word upgrades the class", `{"error":{"code":429,"message":"quota exhausted"}}`, errclass.ClassQuota},
		{"not found", `{"error":{"code":404,"message":"model not found"}}`, errclass.ClassInvalidModel},
		{"no code", `{"error":{"message":"boom"}}`, errclass.ClassUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, eErr := ConvertNonStreamResponseWithContext(FormatGemini, 200, []byte(tc.body), upstreamModel, nil, nil)
			if out != nil {
				t.Fatalf("body-level error was returned as a result: %s", out)
			}
			if eErr == nil || eErr.Class != tc.want {
				t.Fatalf("eErr = %v, want %s", eErr, tc.want)
			}
		})
	}
}

// TestConvertNonStreamResponseRejectsNonObjectBody covers the shape trap that
// json.Valid alone would let through: `null` (and an array or a bare number)
// is valid JSON but is not a Gemini response.
func TestConvertNonStreamResponseRejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `3`, `"text"`, `true`} {
		out, eErr := ConvertNonStreamResponseWithContext(FormatGemini, 200, []byte(body), upstreamModel, nil, nil)
		if out != nil {
			t.Fatalf("body %q was returned as a result: %s", body, out)
		}
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("body %q: eErr = %v, want %s", body, eErr, errclass.ClassTranslation)
		}
	}
}
func TestConvertNonStreamResponseRejectsMalformedBody(t *testing.T) {
	for _, body := range []string{``, `{"candidates":`, `not json`} {
		_, eErr := ConvertNonStreamResponseWithContext(FormatOpenAI, 200, []byte(body), upstreamModel, nil, nil)
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("body %q: eErr = %v, want %s", body, eErr, errclass.ClassTranslation)
		}
	}
}

// TestConvertNonStreamResponseUnsupportedFormat proves an unregistered client
// protocol fails explicitly instead of leaking the Gemini body through the
// registry's raw fallback.
func TestConvertNonStreamResponseUnsupportedFormat(t *testing.T) {
	out, eErr := ConvertNonStreamResponseWithContext("cohere", 200, []byte(nonStreamResult), upstreamModel, nil, nil)
	if out != nil {
		t.Fatalf("unsupported format produced a body: %s", out)
	}
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("eErr = %v, want %s", eErr, errclass.ClassUnsupported)
	}
}

// TestConvertNonStreamResponseOptionsVariantsCoverBothEntryPoints proves the
// no-context BuildRequest-style signature stays wired to the contextual one.
func TestConvertNonStreamResponseOptionsVariantsCoverBothEntryPoints(t *testing.T) {
	plain, eErr := ConvertNonStreamResponse(FormatOpenAI, 200, []byte(nonStreamResult))
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponse failed: %v", eErr)
	}
	ctxAware, eErr := ConvertNonStreamResponseWithContext(FormatOpenAI, 200, []byte(nonStreamResult), upstreamModel, nil, nil)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
	}
	if string(plain) == "" || string(ctxAware) == "" {
		t.Fatalf("empty conversion result: %q / %q", plain, ctxAware)
	}
	if !strings.Contains(string(plain), "Hello") || !strings.Contains(string(ctxAware), "Hello") {
		t.Fatalf("converted bodies lost the text: %s / %s", plain, ctxAware)
	}
}

// itoaTest formats an int for subtest names and JSON bodies.
func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
