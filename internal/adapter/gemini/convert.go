// Non-stream response conversion for the native Gemini route (FR-006): a
// Gemini generateContent result is translated back into the client protocol.
package gemini

import (
	"context"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// ConvertNonStreamResponse translates an upstream Gemini generateContent
// HTTP result back into the client protocol sourceFormat (FR-006). Statuses
// >= 400 become classified redacted errors (§7). "gemini" clients receive
// the upstream body byte-for-byte — thoughtSignature, functionCall and
// functionResponse survive verbatim, which is what the native proto-JSON
// client decodes. The other three formats are rendered by the registered
// gemini→target transformers.
//
// Unknown formats are ClassUnsupported; a non-JSON body (or an empty
// translation on 200) is ClassTranslation.
func ConvertNonStreamResponse(sourceFormat string, status int, upstreamBody []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	return ConvertNonStreamResponseWithContext(sourceFormat, status, upstreamBody, "", nil, nil, tools...)
}

// ConvertNonStreamResponseWithContext is the context-aware variant the
// executor wires: `model` is the resolved upstream model ID, `original` the
// client's own request body, and `translated` the Gemini body this adapter
// built. CPA's gemini→target transformers derive tool identity and model
// metadata from `original` (falling back to `translated` when it is absent
// or invalid), so both are forwarded unchanged.
//
// `tools` is accepted for adapter uniformity but not consumed here: on this
// route CPA's transformers own Responses tool identity, including the
// namespace/apply_patch/custom restoration that the sibling routes
// implement locally with the shared registry.
func ConvertNonStreamResponseWithContext(sourceFormat string, status int, upstreamBody []byte, model string, original, translated []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, upstreamBody)
	}
	if _, eErr := sourceFormatOf(sourceFormat); eErr != nil {
		return nil, eErr
	}
	// Gemini answers with a JSON object. A bare `null` decodes into a struct
	// without error, so the shape is checked rather than just the JSON
	// validity.
	if _, ok := decodeJSONObject(upstreamBody); !ok {
		return nil, errclass.Translation("malformed gemini response JSON: not a JSON object")
	}
	// A body-level error under HTTP 200 is an upstream failure, never a
	// normal result to hand the client.
	if status, message, ok := upstreamErrorBody(upstreamBody); ok {
		return nil, shared.UpstreamStatusError(status, []byte(message))
	}
	if sourceFormat == FormatGemini {
		// Verbatim native passthrough: no re-encoding, so no field can be
		// reordered, reformatted, or dropped on the way to the client.
		return upstreamBody, nil
	}
	var param any
	out := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatGemini,
		geminiToFormat(sourceFormat), model, original, translated, upstreamBody, &param)
	if len(out) == 0 {
		return nil, errclass.Translation("gemini response translation produced an empty body")
	}
	if _, ok := decodeJSONObject(out); !ok {
		return nil, errclass.Translation("gemini response translation produced a non-JSON-object body")
	}
	return out, nil
}
