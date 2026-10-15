package plugin

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Client model IDs can include a provider prefix that must never be sent
// upstream. Restore that ID only when the reported model exactly matches the
// actual upstream target; a different reported model remains visible to clients.
// Restrict changes to protocol metadata, never text, tool arguments or signatures.
func clientResponseModel(res *resolvedExecution, format string, payload []byte) []byte {
	if res.clientModel == "" || res.model == "" || res.clientModel == res.model || format == "gemini" {
		return payload
	}
	var paths []string
	switch format {
	case "openai":
		paths = []string{"model"}
	case "openai-response":
		paths = []string{"model", "response.model"}
	case "claude":
		paths = []string{"model", "message.model"}
	default:
		return payload
	}
	rewrite := func(body []byte) []byte {
		if !json.Valid(body) {
			return body
		}
		for _, path := range paths {
			value := gjson.GetBytes(body, path)
			if value.Type == gjson.String && value.Str == res.model {
				if updated, err := sjson.SetBytes(body, path, res.clientModel); err == nil {
					body = updated
				}
			}
		}
		return body
	}
	if json.Valid(payload) {
		return rewrite(payload)
	}

	// Adapters emit complete SSE frames, sometimes with several data lines.
	// Preserve comments, event IDs, retry fields, CRLF, and untouched frames.
	lines := bytes.SplitAfter(payload, []byte("\n"))
	var result bytes.Buffer
	start := 0
	changed := false
	for i, line := range lines {
		if len(line) == 0 || len(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))) != 0 {
			continue
		}
		frame := lines[start : i+1]
		var data bytes.Buffer
		dataLines := 0
		for _, raw := range frame {
			content := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
			if bytes.HasPrefix(content, []byte("data:")) {
				if dataLines > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(content[5:], []byte(" ")))
				dataLines++
			}
		}
		original := data.Bytes()
		updated := rewrite(original)
		if !bytes.Equal(updated, original) {
			// Collapse multiline JSON into one valid data line without a
			// float64 round trip (usage counters can exceed 2^53).
			var compact bytes.Buffer
			if json.Compact(&compact, updated) == nil {
				updated = compact.Bytes()
			}
			written := false
			for _, raw := range frame {
				content := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
				if bytes.HasPrefix(content, []byte("data:")) {
					if !written {
						result.WriteString("data: ")
						result.Write(updated)
						result.Write(raw[len(content):])
						written = true
					}
				} else {
					result.Write(raw)
				}
			}
			changed = true
		} else {
			for _, raw := range frame {
				result.Write(raw)
			}
		}
		start = i + 1
	}
	if !changed {
		return payload
	}
	for _, raw := range lines[start:] {
		result.Write(raw)
	}
	return result.Bytes()
}
