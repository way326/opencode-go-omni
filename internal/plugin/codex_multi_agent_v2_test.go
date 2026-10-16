// Offline unit tests for the Codex multi-agent v2 input helper
// (codex_multi_agent_v2.go): in-place envelope rewriting, the structural
// Fernet refusal, and byte-identical no-ops. Every case pins plain []byte
// bodies — no network, no host callbacks, no decryption.
package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

// normalizeV2OK runs the helper and fails the test when it returns an error.
func normalizeV2OK(t *testing.T, sourceFormat string, body []byte) []byte {
	t.Helper()
	out, eErr := normalizeCodexV2Input(sourceFormat, body)
	if eErr != nil {
		t.Fatalf("normalizeCodexV2Input(%q) failed: %v", sourceFormat, eErr)
	}
	return out
}

// normalizeV2Err runs the helper and requires a translation failure.
func normalizeV2Err(t *testing.T, sourceFormat string, body []byte) *errclass.Error {
	t.Helper()
	out, eErr := normalizeCodexV2Input(sourceFormat, body)
	if eErr == nil {
		t.Fatalf("normalizeCodexV2Input(%q) succeeded with %s", sourceFormat, out)
	}
	requireClass(t, eErr, errclass.ClassTranslation)
	return eErr
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact JSON %q: %v", raw, err)
	}
	return buf.Bytes()
}

// v2RawItems returns the raw JSON text of every input item, so callers can
// check byte preservation of the items the helper must not touch.
func v2RawItems(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var envelope struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode input raws: %v (%s)", err, body)
	}
	return envelope.Input
}

func v2InputItems(t *testing.T, body []byte) []any {
	t.Helper()
	items, ok := decodeJSONObject(t, body)["input"].([]any)
	if !ok {
		t.Fatalf("input is not an array in %s", body)
	}
	return items
}

func v2Item(t *testing.T, items []any, index int) map[string]any {
	t.Helper()
	if index < 0 || index >= len(items) {
		t.Fatalf("input item %d out of range (%d items)", index, len(items))
	}
	item, ok := items[index].(map[string]any)
	if !ok {
		t.Fatalf("input item %d is not an object: %#v", index, items[index])
	}
	return item
}

func v2Parts(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	entries, ok := item["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %#v", item["content"])
	}
	parts := make([]map[string]any, 0, len(entries))
	for i, entry := range entries {
		part, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("content part %d is not an object: %#v", i, entry)
		}
		parts = append(parts, part)
	}
	return parts
}

func v2TextPart(t *testing.T, part map[string]any) string {
	t.Helper()
	if typ, _ := part["type"].(string); typ != "input_text" {
		t.Fatalf("part type = %#v, want input_text (%#v)", part["type"], part)
	}
	text, ok := part["text"].(string)
	if !ok {
		t.Fatalf("part text is not a string: %#v", part["text"])
	}
	return text
}

// v2PrefixMetadata splits the metadata prefix of a converted agent message:
// a human-readable first line, then the JSON object of envelope metadata. The
// JSON must stay on a single line, so the first newline separates the two.
func v2PrefixMetadata(t *testing.T, prefix string) (map[string]string, string) {
	t.Helper()
	idx := strings.Index(prefix, "\n")
	if idx < 0 {
		t.Fatalf("metadata prefix has no line break: %q", prefix)
	}
	metadata := prefix[idx+1:]
	decoded := make(map[string]string)
	if err := json.Unmarshal([]byte(metadata), &decoded); err != nil {
		t.Fatalf("metadata is not a JSON string map: %v (%s)", err, metadata)
	}
	return decoded, metadata
}

// v2FernetToken builds a structurally framed Fernet token (version byte,
// 8-byte timestamp, 16-byte IV, ciphertext blocks, 32-byte HMAC) as raw
// bytes; the guard only recognizes the framing and never authenticates.
func v2FernetToken(t *testing.T, ciphertextBytes int, version byte) string {
	t.Helper()
	raw := make([]byte, 0, 57+ciphertextBytes)
	raw = append(raw, version)
	raw = append(raw, bytes.Repeat([]byte{0x11}, 8)...)
	raw = append(raw, bytes.Repeat([]byte{0x22}, 16)...)
	raw = append(raw, bytes.Repeat([]byte{0x33}, ciphertextBytes)...)
	raw = append(raw, bytes.Repeat([]byte{0x44}, 32)...)
	token := base64.RawURLEncoding.EncodeToString(raw)
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 57+ciphertextBytes {
		t.Fatalf("fixture token failed to round-trip: %v (%d bytes)", err, len(decoded))
	}
	return token
}

// v2EnvelopeBody wraps one raw agent envelope as the only Responses input item.
func v2EnvelopeBody(envelope string) []byte {
	return []byte(`{"model":"m","input":[` + envelope + `]}`)
}

// v2EncryptedPartBody builds a one-item body whose agent envelope carries a
// single encrypted_content part holding the given string.
func v2EncryptedPartBody(t *testing.T, token string) []byte {
	t.Helper()
	return v2EnvelopeBody(`{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"encrypted_content","encrypted_content":` + string(mustJSON(token)) + `}]}`)
}

// Non-Responses sources and every body without an agent envelope must come
// back byte-identical, including bodies that are not valid JSON at all.
func TestNormalizeCodexV2NoOpSourcesAndBodies(t *testing.T) {
	envelope := []byte(`{"model":"m","input":[{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"任务\n说明"},{"type":"encrypted_content","encrypted_content":"明文"}]}]}`)
	malformed := []byte(`{"input":[`)
	formats := []struct{ name, format string }{
		{"chat-completions", "openai-chat-completions"},
		{"anthropic", "anthropic"},
		{"gemini", "gemini"},
		{"empty", ""},
	}
	for _, f := range formats {
		t.Run(f.name+"/agent-envelope", func(t *testing.T) {
			if got := normalizeV2OK(t, f.format, envelope); !bytes.Equal(got, envelope) {
				t.Fatalf("non-Responses source rewritten:\n got %s\nwant %s", got, envelope)
			}
		})
		t.Run(f.name+"/malformed", func(t *testing.T) {
			if got := normalizeV2OK(t, f.format, malformed); !bytes.Equal(got, malformed) {
				t.Fatalf("malformed non-Responses body rewritten: %q", got)
			}
		})
	}
}

// Responses bodies without an agent envelope keep their exact bytes,
// including whitespace layout.
func TestNormalizeCodexV2ResponseWithoutAgentMessageNoOp(t *testing.T) {
	bodies := map[string][]byte{
		"conversation":    []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","id":"rs_1","summary":["s"],"encrypted_content":"opaque-blob"}]}`),
		"pretty printed":  []byte("{\n  \"model\": \"m\",\n  \"input\": [\n    {\"type\": \"message\", \"role\": \"user\", \"content\": [{\"type\": \"input_text\", \"text\": \"保留\\n换行\"}]}\n  ]\n}"),
		"no input":        []byte(`{"model":"m"}`),
		"empty input":     []byte(`{"model":"m","input":[]}`),
		"non-array input": []byte(`{"model":"m","input":"not-an-array"}`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			got := normalizeV2OK(t, "openai-response", body)
			if !bytes.Equal(got, body) {
				t.Fatalf("body without agent envelope changed:\n got %s\nwant %s", got, body)
			}
		})
	}
}

// Malformed or non-object JSON fails the Responses route only; other sources
// return it untouched (covered above).
func TestNormalizeCodexV2ResponseMalformedJSONErrors(t *testing.T) {
	bodies := map[string][]byte{
		"empty":     []byte(``),
		"truncated": []byte(`{"input":[`),
		"prose":     []byte(`not json at all`),
		"null":      []byte(`null`),
		"array":     []byte(`[{"type":"agent_message"}]`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			normalizeV2Err(t, "openai-response", body)
		})
	}
}

// One request mixing Chinese, embedded newlines, quotes, a large-integer
// sibling, and two agent envelopes: envelopes become user messages at their
// original positions, plain items stay raw JSON, and author/recipient survive
// as JSON-escaped metadata in a new leading input_text part.
func TestNormalizeCodexV2AgentMessagesConvertInPlace(t *testing.T) {
	const (
		beforeItem  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"before"},{"type":"input_text","text":"保留 原样"}]}`
		firstAgent  = `{"type":"agent_message","author":"评审 王\n强","recipient":"小明 \"团队\"","content":[{"type":"input_text","text":"第一行\n第二行"},{"type":"input_text","text":"尾部 \"引号\" \\ 反斜杠"},{"type":"encrypted_content","encrypted_content":"优化明文\n末尾"}]}`
		callItem    = `{"type":"custom_event","value":12345678901234567890}`
		secondAgent = `{"type":"agent_message","author":"worker/2","recipient":"main","content":[{"type":"input_text","text":"第二条\n消息"}]}`
		afterItem   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"after"}]}`
	)
	body := []byte(`{"model":"gpt-5.6-luna","input":[` + beforeItem + `,` + firstAgent + `,` + callItem + `,` + secondAgent + `,` + afterItem + `]}`)
	got := normalizeV2OK(t, "openai-response", body)
	if bytes.Equal(got, body) {
		t.Fatal("agent envelopes were not rewritten")
	}
	if obj := decodeJSONObject(t, got); obj["model"] != "gpt-5.6-luna" {
		t.Fatalf("sibling field changed: %#v", obj)
	}
	items := v2InputItems(t, got)
	if len(items) != 5 {
		t.Fatalf("input length = %d, want 5", len(items))
	}
	before := v2RawItems(t, body)
	after := v2RawItems(t, got)
	for _, index := range []int{0, 2, 4} {
		if !bytes.Equal(compactJSON(t, after[index]), compactJSON(t, before[index])) {
			t.Fatalf("plain item %d was not kept raw:\n got %s\nwant %s", index, after[index], before[index])
		}
	}
	if !bytesContain(got, []byte("12345678901234567890")) {
		t.Fatalf("large integer sibling was reformatted: %s", got)
	}

	first := v2Item(t, items, 1)
	if first["type"] != "message" || first["role"] != "user" {
		t.Fatalf("converted envelope = %#v, want a user message", first)
	}
	parts := v2Parts(t, first)
	if len(parts) != 4 {
		t.Fatalf("converted content parts = %d, want prefix + 3", len(parts))
	}
	metadata, metaRaw := v2PrefixMetadata(t, v2TextPart(t, parts[0]))
	if metadata["author"] != "评审 王\n强" || metadata["recipient"] != "小明 \"团队\"" {
		t.Fatalf("author/recipient not preserved: %#v", metadata)
	}
	if !strings.Contains(metaRaw, `\n`) || !strings.Contains(metaRaw, `\"`) {
		t.Fatalf("metadata is not JSON-escaped: %s", metaRaw)
	}
	wantTexts := []string{"第一行\n第二行", "尾部 \"引号\" \\ 反斜杠", "优化明文\n末尾"}
	for i, want := range wantTexts {
		if text := v2TextPart(t, parts[i+1]); text != want {
			t.Fatalf("part %d = %q, want %q", i+1, text, want)
		}
	}

	second := v2Item(t, items, 3)
	secondParts := v2Parts(t, second)
	if len(secondParts) != 2 {
		t.Fatalf("second envelope parts = %d, want prefix + 1", len(secondParts))
	}
	secondMetadata, _ := v2PrefixMetadata(t, v2TextPart(t, secondParts[0]))
	if secondMetadata["author"] != "worker/2" || secondMetadata["recipient"] != "main" {
		t.Fatalf("second envelope metadata = %#v", secondMetadata)
	}
	if text := v2TextPart(t, secondParts[1]); text != "第二条\n消息" {
		t.Fatalf("second envelope text = %q", text)
	}
	if bytesContain(got, []byte(`"agent_message"`)) || bytesContain(got, []byte(`"encrypted_content"`)) {
		t.Fatalf("agent structural fields survived: %s", got)
	}
}

// The allowed envelope fields are consumed structurally: they never leak into
// the converted message, which carries only type, role, and content.
func TestNormalizeCodexV2AgentPrivateFieldsDropped(t *testing.T) {
	envelope := `{"type":"agent_message","author":"planner","recipient":"worker","content":[{"type":"input_text","text":"正文"}],"internal_chat_message_metadata_passthrough":{"thread":"meta-strip-zzz"},"id":"id-strip-abc","status":"completed"}`
	got := normalizeV2OK(t, "openai-response", v2EnvelopeBody(envelope))
	item := v2Item(t, v2InputItems(t, got), 0)
	if len(item) != 3 || item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("converted message = %#v", item)
	}
	if _, ok := item["content"]; !ok {
		t.Fatalf("converted message lost content: %#v", item)
	}
	for _, leaked := range []string{"meta-strip-zzz", "id-strip-abc", "internal_chat_message_metadata_passthrough"} {
		if bytesContain(got, []byte(leaked)) {
			t.Fatalf("private envelope field %q leaked into %s", leaked, got)
		}
	}
}

// Malformed envelopes, unknown parts, non-string values, and unlisted
// envelope fields are translation failures.
func TestNormalizeCodexV2RejectsInvalidAgentEnvelopes(t *testing.T) {
	cases := []struct{ name, envelope string }{
		{"missing content", `{"type":"agent_message","author":"a","recipient":"b"}`},
		{"null content", `{"type":"agent_message","author":"a","recipient":"b","content":null}`},
		{"empty content", `{"type":"agent_message","author":"a","recipient":"b","content":[]}`},
		{"content object", `{"type":"agent_message","author":"a","recipient":"b","content":{"type":"input_text","text":"x"}}`},
		{"content string entry", `{"type":"agent_message","author":"a","content":["text"]}`},
		{"content number entry", `{"type":"agent_message","author":"a","content":[7]}`},
		{"part type missing", `{"type":"agent_message","author":"a","content":[{"text":"x"}]}`},
		{"part type null", `{"type":"agent_message","author":"a","content":[{"type":null,"text":"x"}]}`},
		{"part type number", `{"type":"agent_message","author":"a","content":[{"type":7,"text":"x"}]}`},
		{"unknown part output_text", `{"type":"agent_message","author":"a","content":[{"type":"output_text","text":"x"}]}`},
		{"unknown part image_url", `{"type":"agent_message","author":"a","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/i.png"}}]}`},
		{"part extra field", `{"type":"agent_message","author":"a","content":[{"type":"input_text","text":"x","extra":1}]}`},
		{"text missing", `{"type":"agent_message","author":"a","content":[{"type":"input_text"}]}`},
		{"text null", `{"type":"agent_message","author":"a","content":[{"type":"input_text","text":null}]}`},
		{"text number", `{"type":"agent_message","author":"a","content":[{"type":"input_text","text":5}]}`},
		{"text array", `{"type":"agent_message","author":"a","content":[{"type":"input_text","text":["x"]}]}`},
		{"encrypted part using text field", `{"type":"agent_message","author":"a","content":[{"type":"encrypted_content","text":"x"}]}`},
		{"encrypted value null", `{"type":"agent_message","author":"a","content":[{"type":"encrypted_content","encrypted_content":null}]}`},
		{"author null", `{"type":"agent_message","author":null,"content":[{"type":"input_text","text":"x"}]}`},
		{"author number", `{"type":"agent_message","author":3,"content":[{"type":"input_text","text":"x"}]}`},
		{"recipient object", `{"type":"agent_message","author":"a","recipient":{},"content":[{"type":"input_text","text":"x"}]}`},
		{"unlisted field agent_private", `{"type":"agent_message","author":"a","content":[{"type":"input_text","text":"x"}],"agent_private":"y"}`},
		{"unlisted field role", `{"type":"agent_message","role":"user","author":"a","content":[{"type":"input_text","text":"x"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalizeV2Err(t, "openai-response", v2EnvelopeBody(tc.envelope))
		})
	}
}

// Refusal errors must never echo the content or unlisted field values.
func TestNormalizeCodexV2RejectionDoesNotEchoAgentContent(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"model":"m","input":[{"type":"agent_message","author":"a","content":[{"type":"unknown_part","text":"PRIVATE-CONTENT-MARKER"}]}]}`),
		[]byte(`{"model":"m","input":[{"type":"agent_message","author":"a","content":[{"type":"input_text","text":"PRIVATE-CONTENT-MARKER"}],"unsupported_field":"PRIVATE-FIELD-MARKER"}]}`),
	}
	for _, body := range bodies {
		eErr := normalizeV2Err(t, "openai-response", body)
		if strings.Contains(eErr.Message, "PRIVATE-CONTENT-MARKER") || strings.Contains(eErr.Message, "PRIVATE-FIELD-MARKER") {
			t.Fatalf("error echoed private content: %s", eErr.Message)
		}
	}
}

// The opaque guard refuses only structurally framed Fernet tokens (version
// 0x80, at least 73 decoded bytes, a positive whole number of 16-byte
// blocks). Unframed ciphertext cannot be distinguished from plaintext and
// passes through byte-exact.
func TestNormalizeCodexV2FernetFramingRefused(t *testing.T) {
	refused := []struct{ name, token string }{
		{"minimum framed 73", v2FernetToken(t, 16, 0x80)},
		{"standard 89", v2FernetToken(t, 32, 0x80)},
		{"padded", v2FernetToken(t, 32, 0x80) + "=="},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			eErr := normalizeV2Err(t, "openai-response", v2EncryptedPartBody(t, tc.token))
			if strings.Contains(eErr.Message, tc.token) {
				t.Fatalf("refusal echoed the token: %s", eErr.Message)
			}
		})
	}

	accepted := []struct{ name, token string }{
		{"no ciphertext blocks 57", v2FernetToken(t, 0, 0x80)},
		{"below minimum 72", v2FernetToken(t, 15, 0x80)},
		{"non-multiple 74", v2FernetToken(t, 17, 0x80)},
		{"wrong version byte", v2FernetToken(t, 32, 0x81)},
		{"not base64url", "not a fernet token!!"},
		{"unframed ciphertext", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 96))},
	}
	for _, tc := range accepted {
		t.Run("plaintext/"+tc.name, func(t *testing.T) {
			got := normalizeV2OK(t, "openai-response", v2EncryptedPartBody(t, tc.token))
			parts := v2Parts(t, v2Item(t, v2InputItems(t, got), 0))
			if len(parts) != 2 || v2TextPart(t, parts[1]) != tc.token {
				t.Fatalf("plaintext token not preserved exactly: %#v", parts)
			}
		})
	}
}

// The guard is scoped: non-agent reasoning keeps its encrypted_content
// verbatim even when it is a framed token, and only encrypted_content parts
// of agent envelopes are inspected — an input_text part carries no ciphertext
// semantics, so a framed-looking string there stays literal.
func TestNormalizeCodexV2FernetGuardScope(t *testing.T) {
	token := v2FernetToken(t, 32, 0x80)

	t.Run("reasoning encrypted_content untouched", func(t *testing.T) {
		reasoning := `{"type":"reasoning","id":"rs_1","summary":["s"],"encrypted_content":` + string(mustJSON(token)) + `}`
		agent := `{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"继续"}]}`
		body := []byte(`{"model":"m","input":[` + reasoning + `,` + agent + `]}`)
		got := normalizeV2OK(t, "openai-response", body)
		before := v2RawItems(t, body)
		after := v2RawItems(t, got)
		if !bytes.Equal(compactJSON(t, after[0]), compactJSON(t, before[0])) {
			t.Fatalf("reasoning item was rewritten:\n got %s\nwant %s", after[0], before[0])
		}
		if !bytesContain(after[0], []byte(token)) {
			t.Fatalf("reasoning encrypted_content changed: %s", after[0])
		}
	})

	t.Run("input_text part keeps literal framed token", func(t *testing.T) {
		agent := `{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":` + string(mustJSON(token)) + `}]}`
		got := normalizeV2OK(t, "openai-response", v2EnvelopeBody(agent))
		parts := v2Parts(t, v2Item(t, v2InputItems(t, got), 0))
		if len(parts) != 2 || v2TextPart(t, parts[1]) != token {
			t.Fatalf("input_text token not preserved: %#v", parts)
		}
	})
}

// Items the helper must not touch stay raw JSON: key order, escapes, and
// integers beyond float64 precision survive the array re-encoding.
func TestNormalizeCodexV2KeepsOtherItemsRaw(t *testing.T) {
	const (
		bigItem   = `{"type":"custom_event","value":12345678901234567890,"note":"大整数"}`
		reasoning = `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-reasoning-blob"}`
		agent     = `{"type":"agent_message","author":"a","recipient":"b","content":[{"type":"input_text","text":"改写"}]}`
		toolOut   = `{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"tool-out"}]}`
	)
	body := []byte(`{"model":"m","input":[` + bigItem + `,` + reasoning + `,` + agent + `,` + toolOut + `]}`)
	got := normalizeV2OK(t, "openai-response", body)
	before := v2RawItems(t, body)
	after := v2RawItems(t, got)
	if len(after) != len(before) {
		t.Fatalf("item count changed: %d -> %d", len(before), len(after))
	}
	for _, index := range []int{0, 1, 3} {
		if !bytes.Equal(compactJSON(t, after[index]), compactJSON(t, before[index])) {
			t.Fatalf("input item %d was rewritten:\n got %s\nwant %s", index, after[index], before[index])
		}
	}
	if !bytesContain(got, []byte("12345678901234567890")) {
		t.Fatalf("large integer was rounded or reformatted: %s", got)
	}
	var decoded struct {
		Input []struct {
			EncryptedContent string `json:"encrypted_content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	if decoded.Input[1].EncryptedContent != "opaque-reasoning-blob" {
		t.Fatalf("reasoning encrypted_content = %q", decoded.Input[1].EncryptedContent)
	}
}

// A second pass over normalized bytes is byte-identical, and bodies without
// agent envelopes were already a no-op.
func TestNormalizeCodexV2Idempotent(t *testing.T) {
	cases := []struct {
		name      string
		body      []byte
		rewritten bool
	}{
		{"single agent", v2EnvelopeBody(`{"type":"agent_message","author":"a","recipient":"b","content":[{"type":"input_text","text":"中文\n多行"}]}`), true},
		{"multiple agents", []byte(`{"model":"m","input":[{"type":"agent_message","author":"a","content":[{"type":"input_text","text":"一"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"mid"}]},{"type":"agent_message","author":"b","content":[{"type":"encrypted_content","encrypted_content":"二"}]}]}`), true},
		{"no agent", []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := normalizeV2OK(t, "openai-response", tc.body)
			second := normalizeV2OK(t, "openai-response", first)
			if !bytes.Equal(second, first) {
				t.Fatalf("second pass changed bytes:\nfirst  %s\nsecond %s", first, second)
			}
			equal := bytes.Equal(first, tc.body)
			if tc.rewritten == equal {
				t.Fatalf("rewritten=%v but body equality=%v for %s", tc.rewritten, equal, first)
			}
		})
	}
}
