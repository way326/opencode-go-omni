package plugin

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

// normalizeCodexV2Input bridges Codex agent envelopes before protocol routing.
// Like CPA's internal codex/optimize-multi-agent-v2 input rewrite, this consumes
// the plaintext contract produced by optimize-multi-agent-v2. It does NOT decrypt
// encrypted_content. Old opaque histories must be resent from a fresh session.
// Only agent envelopes are rewritten; ordinary requests retain their bytes.
func normalizeCodexV2Input(sourceFormat string, body []byte) ([]byte, *errclass.Error) {
	if sourceFormat != "openai-response" {
		return body, nil
	}
	if !json.Valid(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, errclass.Translation("invalid Responses request body for Codex V2 normalization")
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	changed := false
	for i, item := range items {
		if item.Get("type").String() != "agent_message" {
			continue
		}
		normalized, e := normalizeCodexAgentMessage([]byte(item.Raw))
		if e != nil {
			return nil, e
		}
		items[i] = gjson.ParseBytes(normalized)
		changed = true
	}
	if !changed {
		return body, nil
	}
	// Keep non-agent JSON raw, including integers beyond float64 precision.
	raw := make([]json.RawMessage, len(items))
	for i, item := range items {
		raw[i] = json.RawMessage(item.Raw)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, errclass.Translation("cannot encode Codex V2 input")
	}
	out, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return nil, errclass.Translation("cannot replace Codex V2 input")
	}
	return out, nil
}

func normalizeCodexAgentMessage(raw []byte) ([]byte, *errclass.Error) {
	fail := func(reason string) ([]byte, *errclass.Error) {
		// Never include content or unknown field values in errors.
		return nil, errclass.Translation("invalid Codex V2 agent message: " + reason)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return fail("malformed envelope")
	}
	for key := range item {
		switch key {
		case "type", "author", "recipient", "content", "internal_chat_message_metadata_passthrough", "id", "status":
		default:
			return fail("unsupported envelope field")
		}
	}
	source := make(map[string]string)
	for _, key := range []string{"author", "recipient"} {
		if value, ok := item[key]; ok {
			var text string
			if string(value) == "null" || json.Unmarshal(value, &text) != nil {
				return fail("source fields must be strings")
			}
			source[key] = text
		}
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(item["content"], &parts) != nil || len(parts) == 0 {
		return fail("content must be a nonempty text part array")
	}
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	header, _ := json.Marshal(source)
	outParts := []textPart{{Type: "input_text", Text: "[Codex agent message]\n" + string(header)}}
	for _, part := range parts {
		var typ string
		if json.Unmarshal(part["type"], &typ) != nil {
			return fail("content part type must be a string")
		}
		field := "text"
		switch typ {
		case "input_text":
		case "encrypted_content":
			field = "encrypted_content"
		default:
			return fail("unsupported content part")
		}
		for key := range part {
			if key != "type" && key != field {
				return fail("unsupported content part field")
			}
		}
		var text string
		if string(part[field]) == "null" || json.Unmarshal(part[field], &text) != nil {
			return fail("content values must be strings")
		}
		if typ == "encrypted_content" && isCodexFernetToken(text) {
			return fail("opaque encrypted task; enable producer optimization and resend in a new session")
		}
		outParts = append(outParts, textPart{Type: "input_text", Text: text})
	}
	out, err := json.Marshal(struct {
		Type    string     `json:"type"`
		Role    string     `json:"role"`
		Content []textPart `json:"content"`
	}{Type: "message", Role: "user", Content: outParts})
	if err != nil {
		return fail("cannot encode message")
	}
	return out, nil
}

// Recognizes the documented Fernet framing (version + timestamp + IV + AES
// blocks + HMAC). This is only a known-opaque guard, not a plaintext detector:
// arbitrary strings cannot be authenticated as plaintext without a trusted
// producer marker. A printable/base64-looking plaintext is otherwise preserved.
func isCodexFernetToken(text string) bool {
	token := strings.TrimRight(text, "=")
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) >= 73 && decoded[0] == 0x80 && (len(decoded)-57)%16 == 0
}
