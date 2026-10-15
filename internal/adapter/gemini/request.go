// Request translation for the native Gemini route (FR-005): every client
// protocol is converted by the registered gemini request transformer, then
// the resolved upstream model ID is enforced on the translated body.
package gemini

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
	"opencode-go-cliproxyapi/internal/thinking"
)

// BuildRequest translates an inbound request body from sourceFormat
// ("gemini" native passthrough, "openai" Chat Completions, "claude"
// Anthropic Messages, "openai-response" Responses) into a Gemini
// generateContent body for upstreamModel (FR-005). upstreamModel replaces
// whatever client-facing model ID the request carried (FR-003): the
// gemini→gemini transformer leaves `model` untouched and the
// responses→gemini leg omits it, so the rewrite runs after translation and
// is a byte-exact no-op when the field already matches.
//
// `ts` is the target model's declared reasoning capability. A client-declared
// reasoning control is rejected descriptively (ClassUnsupported) only when
// the model *explicitly declares* a capability that excludes it — a declared
// level set that omits the requested level, or a declared maximum budget the
// request exceeds. Capability-free models (ts nil, or no declared levels and
// no maximum) forward the control exactly as CPA's own executors do, so this
// guard can never reject a request the native path would have accepted.
//
// Malformed input is ClassTranslation: CPA's converters build a fresh Gemini
// envelope from scratch and would otherwise turn an undecodable body into an
// empty request instead of an error. Unknown formats are ClassUnsupported.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	return BuildRequestWithContext(context.Background(), upstreamModel, sourceFormat, sourceBody, false, ts, tools...)
}

// BuildRequestWithContext is the context-aware BuildRequest variant. `stream`
// is carried in the translator envelope for registry parity; the gemini
// request transformers ignore it (they are identical for both actions,
// matching CPA's native executors, which do not branch on it either).
//
// `tools` is accepted for adapter uniformity but is not consumed on this
// route: CPA's gemini↔Responses transformers own tool identity themselves
// (namespace unrolling, custom/apply_patch/tool_search carriers, and the
// reverse identity map they rebuild from the client's original body).
func BuildRequestWithContext(ctx context.Context, upstreamModel, sourceFormat string, sourceBody []byte, stream bool, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	from, eErr := sourceFormatOf(sourceFormat)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := validateRequestJSON(sourceFormat, sourceBody); eErr != nil {
		return nil, eErr
	}
	if eErr := reasoningGuard(sourceFormat, sourceBody, ts); eErr != nil {
		return nil, eErr
	}
	// Native GenerateContent is already the upstream protocol. Keep opaque
	// signatures and schemas intact, including formats introduced after the
	// pinned SDK. Signature verification belongs to the upstream model.
	if from == sdktranslator.FormatGemini {
		return shared.RewriteModelID(upstreamModel, sourceBody, "gemini")
	}
	env := sdktranslator.RequestEnvelope{
		Format: from,
		Model:  upstreamModel,
		Stream: stream,
		Body:   sourceBody,
	}
	translated := sdktranslator.TranslateRequestEnvelope(ctx, from, sdktranslator.FormatGemini, env)
	if translated.Err != nil {
		return nil, errclass.Translation("gemini request translation failed: " + translated.Err.Error())
	}
	if len(translated.Body) == 0 {
		return nil, errclass.Translation("gemini request translation produced an empty body")
	}
	return shared.RewriteModelID(upstreamModel, translated.Body, "gemini")
}

// validateRequestJSON rejects bodies CPA's from-scratch converters would
// otherwise accept as an empty request. A JSON null decodes to a nil map and
// is rejected for the same reason the shared model rewriter rejects it.
func validateRequestJSON(sourceFormat string, body []byte) *errclass.Error {
	if len(body) == 0 {
		return errclass.Translation("malformed " + sourceFormat + " request JSON: empty body")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		return errclass.Translation("malformed " + sourceFormat + " request JSON: " + err.Error())
	}
	if decoded == nil {
		return errclass.Translation("malformed " + sourceFormat + " request JSON: JSON null is not a valid request")
	}
	return nil
}

// reasoningSource is the client-declared reasoning intent extracted from one
// source protocol, normalized to a level or a budget.
type reasoningSource struct {
	level  string
	budget *int64
}

// decodeReasoning extracts the reasoning control the client declared. A
// missing or unparsable control yields the zero value, which the guard treats
// as "nothing declared" — the translators, not this adapter, own decoding.
func decodeReasoning(sourceFormat string, body []byte) reasoningSource {
	switch sourceFormat {
	case FormatOpenAI:
		var src struct {
			ReasoningEffort string `json:"reasoning_effort"`
			Reasoning       *struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if json.Unmarshal(body, &src) != nil {
			return reasoningSource{}
		}
		if src.ReasoningEffort != "" {
			return reasoningSource{level: src.ReasoningEffort}
		}
		if src.Reasoning != nil {
			return reasoningSource{level: src.Reasoning.Effort}
		}
	case FormatOpenAIResp:
		var src struct {
			Reasoning *struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if json.Unmarshal(body, &src) != nil {
			return reasoningSource{}
		}
		if src.Reasoning != nil && src.Reasoning.Effort != "" {
			return reasoningSource{level: src.Reasoning.Effort}
		}
		return reasoningSource{level: src.ReasoningEffort}
	case FormatClaude:
		var src struct {
			Thinking *struct {
				Type         string `json:"type"`
				BudgetTokens *int64 `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if json.Unmarshal(body, &src) != nil || src.Thinking == nil {
			return reasoningSource{}
		}
		if src.Thinking.BudgetTokens == nil {
			return reasoningSource{}
		}
		if strings.EqualFold(src.Thinking.Type, "disabled") {
			// An explicitly disabled control is an off state, not a level.
			off := int64(0)
			return reasoningSource{budget: &off}
		}
		return reasoningSource{budget: src.Thinking.BudgetTokens}
	case FormatGemini:
		var src struct {
			GenerationConfig *struct {
				ThinkingConfig *struct {
					ThinkingLevel  string `json:"thinkingLevel"`
					ThinkingBudget *int64 `json:"thinkingBudget"`
				} `json:"thinkingConfig"`
			} `json:"generationConfig"`
		}
		if json.Unmarshal(body, &src) != nil || src.GenerationConfig == nil || src.GenerationConfig.ThinkingConfig == nil {
			return reasoningSource{}
		}
		return reasoningSource{
			level:  src.GenerationConfig.ThinkingConfig.ThinkingLevel,
			budget: src.GenerationConfig.ThinkingConfig.ThinkingBudget,
		}
	}
	return reasoningSource{}
}

// reasoningGuard rejects a declared reasoning control the target model's
// explicitly declared capability excludes. It stays silent whenever the
// catalog provides no capability to judge against, so capability-free models
// keep CPA's native forward-as-is behavior.
func reasoningGuard(sourceFormat string, body []byte, ts *pluginapi.ThinkingSupport) *errclass.Error {
	src := decodeReasoning(sourceFormat, body)
	if ts == nil {
		return nil
	}
	if level := strings.ToLower(strings.TrimSpace(src.level)); level != "" && !strings.EqualFold(level, "auto") {
		// Only an explicitly declared level set can exclude a level: the
		// shared canonical fallback would otherwise reject levels a model
		// never spoke about (for example "xhigh" on a model with no
		// declared levels at all).
		if len(ts.Levels) == 0 {
			return nil
		}
		return thinking.ValidateEffort(level, ts)
	}
	if src.budget == nil {
		return nil
	}
	budget := *src.budget
	if ts.Max > 0 && budget > int64(ts.Max) {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "reasoning budget " + strconv.FormatInt(budget, 10) + " exceeds the model maximum of " + strconv.FormatInt(int64(ts.Max), 10),
		}
	}
	if budget < 0 && !ts.DynamicAllowed {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "this model does not support a dynamic reasoning budget",
		}
	}
	if budget == 0 && ts.Min > 0 {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "this model cannot disable reasoning",
		}
	}
	return nil
}
