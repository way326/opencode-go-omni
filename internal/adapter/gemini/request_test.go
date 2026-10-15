package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"opencode-go-cliproxyapi/internal/errclass"
)

const upstreamModel = "gemini-3-pro-upstream"

// clientModel is the prefixed ID a client sends; every formatted route must
// replace it with the resolved upstream model (FR-003).
const clientModel = "opencode-go/gemini-3-pro"

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("translated body is not a JSON object: %v (%s)", err, body)
	}
	return out
}

func nested(t *testing.T, body map[string]any, path ...string) any {
	t.Helper()
	var cur any = body
	for _, key := range path {
		if idx, err := strconv.Atoi(key); err == nil {
			arr, ok := cur.([]any)
			if !ok || idx >= len(arr) {
				t.Fatalf("path %v: %q is not an array index into %T", path, key, cur)
			}
			cur = arr[idx]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, key)
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("path %v: %q missing in %v", path, key, body)
		}
	}
	return cur
}

// requestFixtures holds one representative client body per served protocol.
// Each declares the same single "lookup" function tool and an explicit
// reasoning control so the guard and the tool/reasoning conversions are both
// exercised.
func requestFixtures() map[string]string {
	return map[string]string{
		FormatGemini: `{"model":"` + clientModel + `","contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
			`"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}],` +
			`"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`,
		FormatOpenAI: `{"model":"` + clientModel + `","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`,
		FormatClaude: `{"model":"` + clientModel + `","max_tokens":128,"thinking":{"type":"enabled","budget_tokens":8192},` +
			`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],` +
			`"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`,
		FormatOpenAIResp: `{"model":"` + clientModel + `","reasoning":{"effort":"high"},"instructions":"sys",` +
			`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
			`"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}`,
	}
}

// TestBuildRequestRewritesModelForEveryClientFormat proves the resolved
// upstream ID replaces the client-facing model on all four legs, including
// gemini→gemini (whose transformer never touches the field) and
// responses→gemini (whose transformer omits it entirely).
func TestBuildRequestRewritesModelForEveryClientFormat(t *testing.T) {
	for format, body := range requestFixtures() {
		t.Run(format, func(t *testing.T) {
			out, eErr := BuildRequest(upstreamModel, format, []byte(body), nil)
			if eErr != nil {
				t.Fatalf("BuildRequest(%s) failed: %v", format, eErr)
			}
			decoded := decode(t, out)
			if got := decoded["model"]; got != upstreamModel {
				t.Fatalf("model = %v, want %q", got, upstreamModel)
			}
			if strings.Contains(string(out), clientModel) {
				t.Fatalf("translated body still carries the client model: %s", out)
			}
		})
	}
}

// TestBuildRequestToolAndReasoningConversionForEveryClientFormat pins the
// per-format request shape: functionDeclarations stay intact under the
// gemini field names and the client's reasoning control reaches the
// generationConfig.
func TestBuildRequestToolAndReasoningConversionForEveryClientFormat(t *testing.T) {
	cases := []struct {
		format      string
		wantTool    []string
		wantReason  []string
		wantReasonV any
	}{
		{FormatGemini,
			[]string{"tools", "0", "functionDeclarations", "0", "name"},
			[]string{"generationConfig", "thinkingConfig", "thinkingLevel"}, "high"},
		{FormatOpenAI,
			[]string{"tools", "0", "functionDeclarations", "0", "name"},
			[]string{"generationConfig", "thinkingConfig", "thinkingLevel"}, "high"},
		{FormatClaude,
			[]string{"tools", "0", "functionDeclarations", "0", "name"},
			[]string{"generationConfig", "thinkingConfig", "thinkingBudget"}, nil},
		{FormatOpenAIResp,
			[]string{"tools", "0", "functionDeclarations", "0", "name"},
			[]string{"generationConfig", "thinkingConfig", "thinkingLevel"}, "high"},
	}
	fixtures := requestFixtures()
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			out, eErr := BuildRequest(upstreamModel, tc.format, []byte(fixtures[tc.format]), nil)
			if eErr != nil {
				t.Fatalf("BuildRequest(%s) failed: %v", tc.format, eErr)
			}
			decoded := decode(t, out)
			if got := nested(t, decoded, tc.wantTool...); got != "lookup" {
				t.Fatalf("tool name = %v, want lookup", got)
			}
			got := nested(t, decoded, tc.wantReason...)
			if tc.wantReasonV != nil {
				if got != tc.wantReasonV {
					t.Fatalf("reasoning level = %v, want %v", got, tc.wantReasonV)
				}
				return
			}
			budget, ok := got.(float64)
			if !ok || budget != 8192 {
				t.Fatalf("thinking budget = %v, want 8192", got)
			}
		})
	}
}

// geminiThoughtSignature builds the transport shape CPA accepts as a native
// Gemini thought signature: base64 of protobuf{field2:{field1:opaque}}.
func geminiThoughtSignature(payload []byte) string {
	inner := append([]byte{0x0a, byte(len(payload))}, payload...)
	outer := append([]byte{0x12, byte(len(inner))}, inner...)
	return base64.StdEncoding.EncodeToString(outer)
}

// Native requests transport opaque signatures without applying the pinned
// SDK's signature validator, including blobs from newer Gemini generations.
func TestBuildRequestGeminiPreservesMultiturnSignatures(t *testing.T) {
	native := geminiThoughtSignature([]byte{0x01, 0x0c, 0x39})
	body := `{"model":"` + clientModel + `","contents":[` +
		`{"role":"user","parts":[{"text":"call it"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"` + native + `"}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"reflect","args":{}},"thoughtSignature":"not-a-signature"}]}]}`
	out, eErr := BuildRequest(upstreamModel, FormatGemini, []byte(body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	text := string(out)
	if !strings.Contains(text, native) {
		t.Fatalf("verifiable native signature was not preserved: %s", text)
	}
	if !strings.Contains(text, "not-a-signature") || strings.Contains(text, "skip_thought_signature_validator") {
		t.Fatalf("opaque signature was altered by the proxy: %s", text)
	}
	if !strings.Contains(text, `"functionResponse"`) {
		t.Fatalf("functionResponse part was lost: %s", text)
	}

	var decoded struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionResponse *struct {
					Name string `json:"name"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Contents) != 4 {
		t.Fatalf("contents = %d turns, want 4 (multiturn replayed verbatim)", len(decoded.Contents))
	}
	// Both recognized and opaque text-part signatures must survive a replay.
	textBody := `{"model":"` + clientModel + `","contents":[{"role":"user","parts":[{"text":"hi"}]},` +
		`{"role":"model","parts":[{"text":"thought","thoughtSignature":"` + native + `"}]},` +
		`{"role":"model","parts":[{"text":"unverified","thoughtSignature":"not-a-signature"}]}]}`
	textOut, eErr := BuildRequest(upstreamModel, FormatGemini, []byte(textBody), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	if !strings.Contains(string(textOut), native) {
		t.Fatalf("verifiable signature on a model text part was not preserved: %s", textOut)
	}
	if !strings.Contains(string(textOut), "not-a-signature") {
		t.Fatalf("opaque text-part signature was lost: %s", textOut)
	}
	if name := decoded.Contents[2].Parts[0].FunctionResponse.Name; name != "lookup" {
		t.Fatalf("functionResponse.name = %q, want lookup", name)
	}
}

// TestBuildRequestNamespaceToolsUnrollForGemini proves the Responses
// namespace tool is unrolled by CPA's own converter into the qualified
// `namespace__child` wire name rather than being dropped or passed through
// with a `namespace` field gemini cannot express.
func TestBuildRequestNamespaceToolsUnrollForGemini(t *testing.T) {
	body := `{"model":"` + clientModel + `","input":[` +
		`{"type":"function_call","call_id":"c1","namespace":"functions","name":"exec","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}],` +
		`"tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`
	out, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, []byte(body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	text := string(out)
	if !strings.Contains(text, "functions__exec") {
		t.Fatalf("namespace tool was not qualified: %s", text)
	}
	if !strings.Contains(text, `"functionDeclarations"`) {
		t.Fatalf("namespace child became no function declaration: %s", text)
	}
	decoded := decode(t, out)
	contents, ok := nested(t, decoded, "contents").([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("no contents produced: %s", text)
	}
}

func TestBuildRequestRejectsUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest(upstreamModel, "cohere", []byte(`{}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("eErr = %v, want %s", eErr, errclass.ClassUnsupported)
	}
}

// TestBuildRequestRejectsMalformedInput covers the bodies CPA's
// from-scratch converters would otherwise accept as an empty request.
func TestBuildRequestRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated", `{"model":"x","contents":[`},
		{"json null", `null`},
		{"empty", ``},
		{"not an object", `"hi"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for format := range requestFixtures() {
				_, eErr := BuildRequest(upstreamModel, format, []byte(tc.body), nil)
				if eErr == nil || eErr.Class != errclass.ClassTranslation {
					t.Fatalf("BuildRequest(%s, %q) eErr = %v, want %s", format, tc.body, eErr, errclass.ClassTranslation)
				}
			}
		})
	}
}

// TestBuildRequestReasoningGuard documents the narrow capability guard: only
// an explicitly declared capability may reject a control.
func TestBuildRequestReasoningGuard(t *testing.T) {
	levels := &pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}
	for _, tc := range []struct {
		name    string
		format  string
		body    string
		ts      *pluginapi.ThinkingSupport
		wantErr bool
		wantMsg string
	}{
		{"declared level excluded", FormatOpenAI,
			`{"model":"m","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`,
			levels, true, "xhigh"},
		{"declared level admitted", FormatOpenAI,
			`{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			levels, false, ""},
		{"undeclared level set forwards as-is", FormatOpenAI,
			`{"model":"m","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`,
			&pluginapi.ThinkingSupport{}, false, ""},
		{"nil capability never rejects", FormatOpenAIResp,
			`{"model":"m","reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			nil, false, ""},
		{"auto sentinel is not a level", FormatOpenAIResp,
			`{"model":"m","reasoning":{"effort":"auto"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			levels, false, ""},
		{"budget over declared maximum", FormatClaude,
			`{"model":"m","max_tokens":16,"thinking":{"type":"enabled","budget_tokens":32768},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			&pluginapi.ThinkingSupport{Min: 1024, Max: 8192}, true, "exceeds"},
		{"budget within declared maximum", FormatClaude,
			`{"model":"m","max_tokens":16,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			&pluginapi.ThinkingSupport{Min: 1024, Max: 8192}, false, ""},
		{"dynamic budget unsupported", FormatGemini,
			`{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`,
			&pluginapi.ThinkingSupport{Max: 8192}, true, "dynamic"},
		{"dynamic budget supported", FormatGemini,
			`{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`,
			&pluginapi.ThinkingSupport{Max: 8192, DynamicAllowed: true}, false, ""},
		{"disabling reasoning unsupported", FormatGemini,
			`{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`,
			&pluginapi.ThinkingSupport{Min: 1024}, true, "disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest(upstreamModel, tc.format, []byte(tc.body), tc.ts)
			if !tc.wantErr {
				if eErr != nil {
					t.Fatalf("unexpected error: %v", eErr)
				}
				return
			}
			if eErr == nil {
				t.Fatalf("expected %s error", errclass.ClassUnsupported)
			}
			if eErr.Class != errclass.ClassUnsupported {
				t.Fatalf("class = %s, want %s", eErr.Class, errclass.ClassUnsupported)
			}
			if !strings.Contains(eErr.Message, tc.wantMsg) {
				t.Fatalf("message %q does not mention %q", eErr.Message, tc.wantMsg)
			}
		})
	}
}

// TestBuildRequestWithContextStreamFlagIsInert proves the stream flag rides
// the envelope without changing the translated body, matching CPA's native
// gemini executors which do not branch on it.
func TestBuildRequestWithContextStreamFlagIsInert(t *testing.T) {
	body := requestFixtures()[FormatOpenAI]
	plain, eErr := BuildRequest(upstreamModel, FormatOpenAI, []byte(body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	streaming, eErr := BuildRequestWithContext(context.Background(), upstreamModel, FormatOpenAI, []byte(body), true, nil)
	if eErr != nil {
		t.Fatalf("BuildRequestWithContext failed: %v", eErr)
	}
	if string(plain) != string(streaming) {
		t.Fatalf("stream flag changed the body:\n%s\n%s", plain, streaming)
	}
}

// TestRegistryCoversEveryClientFormat guards the delegation contract against
// a CPA dependency bump that drops a gemini pair: BuildRequest needs the
// request transformer, and the response paths need both response
// transformers. sourceFormatOf refuses the same missing pairs at run time.
func TestRegistryCoversEveryClientFormat(t *testing.T) {
	for format := range requestFixtures() {
		from := sdktranslator.FromString(format)
		if !sdktranslator.HasRequestTransformer(from, sdktranslator.FormatGemini) {
			t.Errorf("no request transformer %s→gemini", format)
		}
		// Has* predicates are keyed by the request direction (source→gemini);
		// only Translate* uses the run-time direction (gemini→source).
		if !sdktranslator.HasStreamResponseTransformer(from, sdktranslator.FormatGemini) {
			t.Errorf("no stream response transformer gemini→%s", format)
		}
		if !sdktranslator.HasNonStreamResponseTransformer(from, sdktranslator.FormatGemini) {
			t.Errorf("no non-stream response transformer gemini→%s", format)
		}
	}
}
