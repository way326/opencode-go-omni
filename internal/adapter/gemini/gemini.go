// Package gemini implements the native Gemini protocol adapter for the
// OpenCode Go plugin (spec 04 §4): the upstream
// `…/models/{model}:generateContent` and `:streamGenerateContent` routes.
//
// Cross-protocol translation is delegated to CPA v8.0.28's built-in
// translator registry (`sdk/translator/builtin`). Every client protocol this
// route serves — native `gemini`, `openai` (Chat Completions), `claude`
// (Messages) and `openai-response` (Responses) — already has a registered
// gemini pair in both directions, including the conversions this adapter
// must not re-implement: Gemini `contents`/`functionDeclarations`
// construction, thought-signature handling, Responses namespace-tool
// unrolling (`namespace__child`) and its reverse identity map,
// apply_patch/tool_search/custom-tool carriers, and the SSE renderers each
// target protocol expects. `sdktranslator.TranslateRequestEnvelope` is used
// (never the legacy `TranslateRequest`, which discards the request-scoped
// error) and its `.Err` is always checked.
//
// Native Gemini requests preserve opaque signatures and schemas without
// applying the SDK's Gemini request normalizer. The upstream validates them.
// The transport contract is handled locally.
//
//  1. Request model identity. BuildRequest rewrites the top-level model to
//     the resolved upstream ID after translation, or directly for a native
//     Gemini request. The rewrite is a byte-exact no-op when it already matches.
//
//  2. Stream framing per target protocol. Upstream `alt=sse` delivers
//     `data: {…}` frames whose payload is a Gemini GenerateContentResponse
//     chunk. The registered transformers return whatever their target's HTTP
//     handler expects and this adapter forwards it untouched, so the four
//     targets differ:
//
//     - gemini: `PassthroughGeminiResponseStream` strips the `data:` prefix
//     and returns the raw JSON chunk; CPA's Gemini handler re-adds the
//     framing (`"data: "` + chunk + `"\n\n"`).
//     - openai: the transformer returns a raw `chat.completion.chunk` JSON
//     object (no `data:` prefix); CPA's OpenAI handler adds the framing
//     (`"data: %s\n\n"`).
//     - claude: the transformer renders *complete* SSE blocks
//     (`event: …\ndata: …\n\n`), which CPA's Claude handler writes verbatim.
//     - openai-response: the transformer renders complete SSE blocks, which
//     CPA's Responses handler consumes through its SSE framer.
//
//     Adding or stripping framing here would therefore double-frame the
//     gemini/openai legs or corrupt the claude/openai-response legs.
//
//  3. Stream termination. Gemini emits `finishReason` before trailing
//     `usageMetadata` and thought-signature frames, so Feed never reports
//     completion on a finish-reason sighting. The upstream is read to EOF
//     and the client's terminal events (claude `message_stop`,
//     openai-response `response.completed`) are flushed exactly once, from
//     Finish, which also validates that the upstream reached a terminal
//     state (`finishReason`, `promptFeedback`) and reports a truncation when
//     it did not.
//
// Reasoning controls are capability-checked against the target model's
// declared ThinkingSupport and rejected descriptively (ClassUnsupported)
// when the model cannot represent them, matching the sibling routes; they
// are never dropped silently. Host-side post-processing (for example
// `EnsureResponsesUsageDetails` on the openai-response path) is applied by
// the executor, not here.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"

	"github.com/way326/opencode-go-omni/internal/adapter/shared"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// Format labels of the client protocols served by this route. They match the
// registry's own format identifiers, so registration and routing cannot
// drift.
const (
	FormatGemini     = "gemini"
	FormatOpenAI     = "openai"
	FormatClaude     = "claude"
	FormatOpenAIResp = "openai-response"
	targetLabel      = "the native Gemini route"
)

// sourceFormat validates a client protocol label and the registry pairs this
// route needs in both directions. A pair missing from the registry is an
// explicit ClassUnsupported failure, never a silent raw pass-through.
//
// Direction convention: Registry.Has* predicates are keyed by the *request*
// direction (sourceFormat→gemini), because Register stores the response
// transforms under the source format's row, while TranslateNonStream and
// TranslateStream resolve by the run-time direction (gemini→sourceFormat).
// The two conventions are reversed, so every lookup here names its direction
// explicitly.
func sourceFormatOf(sourceFormat string) (sdktranslator.Format, *errclass.Error) {
	switch sourceFormat {
	case FormatGemini, FormatOpenAI, FormatClaude, FormatOpenAIResp:
	default:
		return "", shared.UnsupportedFormat(sourceFormat, targetLabel)
	}
	from := sdktranslator.FromString(sourceFormat)
	requestPair := sdktranslator.HasRequestTransformer(from, sdktranslator.FormatGemini)
	streamPair := sdktranslator.HasStreamResponseTransformer(from, sdktranslator.FormatGemini)
	nonStreamPair := sdktranslator.HasNonStreamResponseTransformer(from, sdktranslator.FormatGemini)
	if !requestPair || !streamPair || !nonStreamPair {
		return "", shared.UnsupportedFormat(sourceFormat, targetLabel)
	}
	return from, nil
}

// geminiToFormat is the direction used for responses: the upstream is always
// gemini and the target is the client protocol.
func geminiToFormat(sourceFormat string) sdktranslator.Format {
	return sdktranslator.FromString(sourceFormat)
}

// chunkInfo carries the lifecycle facts Finish needs from one streamed
// GenerateContentResponse chunk. The chunk itself is never re-encoded: it is
// forwarded to the registry verbatim.
type chunkInfo struct {
	finishReason string
	blocked      bool
	hasUsage     bool
	errStatus    int
	errMessage   string
}

// geminiChunk is the decode-only projection used to classify a stream chunk.
// `response` is the Vertex-style wrapper some relays add around the payload.
type geminiChunk struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason        string `json:"blockReason"`
		BlockReasonMessage string `json:"blockReasonMessage"`
	} `json:"promptFeedback"`
	UsageMetadata json.RawMessage `json:"usageMetadata"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	Response *geminiChunk `json:"response"`
}

// decodeJSONObject requires a payload to be a JSON object. json.Unmarshal
// accepts a bare `null` into any struct without error (and a `[]`/number into
// nothing at all), so a shape check is what actually rejects a non-response
// body — `json.Valid` alone would let `null` through as an empty success.
func decodeJSONObject(body []byte) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &decoded); err != nil || decoded == nil {
		return nil, false
	}
	return decoded, true
}

// upstreamErrorBody reports the upstream error object a Gemini result carries
// even under an HTTP 200 (Gemini relays sometimes answer with a body-level
// error). ok is false for a normal result.
func upstreamErrorBody(body []byte) (status int, message string, ok bool) {
	var probe struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Error == nil {
		return 0, "", false
	}
	status = probe.Error.Code
	if status < 400 || status > 599 {
		status = 502
	}
	message = probe.Error.Message
	if message == "" {
		message = probe.Error.Status
	}
	if message == "" {
		message = string(body)
	}
	return status, message, true
}

// malformedChunkError reports a stream payload that is not a Gemini response
// object.
func malformedChunkError(payload []byte) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUpstream,
		Message: "gemini stream produced a non-JSON-object chunk: " + shared.RedactedSnippet(string(payload)),
	}
}

// classifyChunk decodes one data payload for lifecycle facts. Gemini streams
// carry JSON objects, so anything else — `null`, an array, a truncated frame —
// is reported rather than forwarded, which would corrupt client framing.
func classifyChunk(payload []byte) (chunkInfo, *errclass.Error) {
	if _, ok := decodeJSONObject(payload); !ok {
		return chunkInfo{}, malformedChunkError(payload)
	}
	var chunk geminiChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return chunkInfo{}, malformedChunkError(payload)
	}
	if chunk.Response != nil && len(chunk.Candidates) == 0 && chunk.PromptFeedback == nil &&
		len(chunk.UsageMetadata) == 0 && chunk.Error == nil {
		chunk = *chunk.Response
	}
	info := chunkInfo{hasUsage: len(chunk.UsageMetadata) > 0}
	for _, candidate := range chunk.Candidates {
		if strings.TrimSpace(candidate.FinishReason) != "" {
			info.finishReason = strings.TrimSpace(candidate.FinishReason)
			break
		}
	}
	if chunk.PromptFeedback != nil && (chunk.PromptFeedback.BlockReason != "" || chunk.PromptFeedback.BlockReasonMessage != "") {
		info.blocked = true
	}
	if chunk.Error != nil {
		info.errStatus = chunk.Error.Code
		if info.errStatus < 400 || info.errStatus > 599 {
			info.errStatus = 502
		}
		info.errMessage = chunk.Error.Message
		if info.errMessage == "" {
			info.errMessage = chunk.Error.Status
		}
		if info.errMessage == "" {
			info.errMessage = string(payload)
		}
	}
	return info, nil
}

// streamChunkError classifies an in-stream upstream error frame. Before the
// first client event the failure is still pre-emission, so it keeps the
// upstream status classification (and its retryability); after that a retry
// would duplicate a partially delivered stream, so retryability is dropped
// (§7 post-first-byte rule).
func streamChunkError(info chunkInfo, payload []byte, emitted bool) *errclass.Error {
	if !emitted {
		return shared.UpstreamStatusError(info.errStatus, []byte(info.errMessage))
	}
	return &errclass.Error{
		Class:      errclass.ClassUpstream,
		Message:    shared.RedactedSnippet(info.errMessage),
		StatusCode: info.errStatus,
		Retryable:  false,
	}
}

// translateStreamChunk runs the registered gemini→target stream transformer
// for one reassembled data payload. A nil result is a translation failure on
// a 200 stream, never a silent empty frame.
func translateStreamChunk(ctx context.Context, target sdktranslator.Format, model string, original, translated, payload []byte, param *any) ([][]byte, *errclass.Error) {
	out := sdktranslator.TranslateStream(ctx, sdktranslator.FormatGemini, target, model, original, translated, payload, param)
	if len(out) == 0 {
		return nil, nil
	}
	for _, frame := range out {
		if len(frame) == 0 {
			return nil, errclass.Translation("gemini stream translation produced an empty frame")
		}
	}
	return out, nil
}
