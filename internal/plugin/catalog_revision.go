package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
)

// servedModelInfos returns the exact projection published by model.static and
// model.for_auth. Callers receive a fresh slice; the catalog records remain
// read-only and are never exposed across the Manager boundary.
func (m *Manager) servedModelInfos() []pluginapi.ModelInfo {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	if mgr == nil {
		return []pluginapi.ModelInfo{}
	}
	return publishedModelInfos(mgr.Models())
}

// publishedModelInfos is the single catalog-record to pluginapi.ModelInfo
// mapping used by both discovery responses and registry revisions.
func publishedModelInfos(records []catalog.ModelRecord) []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(records))
	for _, rec := range records {
		models = append(models, pluginapi.ModelInfo{
			ID:                        rec.PublicID,
			Object:                    "model",
			OwnedBy:                   ProviderID,
			DisplayName:               rec.DisplayName,
			ContextLength:             rec.ContextLimit,
			MaxCompletionTokens:       rec.OutputLimit,
			SupportedInputModalities:  rec.InputModes,
			SupportedOutputModalities: rec.OutputModes,
			Thinking:                  rec.Thinking,
		})
	}
	return models
}

// catalogRevision hashes the fields CPA observes in the served projection.
// Executor-only catalog fields such as protocol, endpoint and upstream ID are
// deliberately absent. The canonical encoding is sorted by model ID; modality
// slices are deduplicated and sorted, while thinking levels retain their
// declared order. Nil and empty modality/level slices both encode as []; a nil
// Thinking pointer is omitted, while a non-nil zero-valued thinking object is
// retained.
func catalogRevision(models []pluginapi.ModelInfo) (string, error) {
	ordered := append([]pluginapi.ModelInfo(nil), models...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].ID < ordered[j].ID
	})

	payload := catalogRevisionPayload{Models: make([]catalogRevisionModel, 0, len(ordered))}
	for _, model := range ordered {
		var thinking *catalogRevisionThinking
		if model.Thinking != nil {
			thinking = &catalogRevisionThinking{
				Min:            model.Thinking.Min,
				Max:            model.Thinking.Max,
				ZeroAllowed:    model.Thinking.ZeroAllowed,
				DynamicAllowed: model.Thinking.DynamicAllowed,
				Levels:         canonicalOrderedStrings(model.Thinking.Levels),
			}
		}
		payload.Models = append(payload.Models, catalogRevisionModel{
			ID:                        model.ID,
			DisplayName:               model.DisplayName,
			ContextLength:             model.ContextLength,
			MaxCompletionTokens:       model.MaxCompletionTokens,
			SupportedInputModalities:  canonicalStringSet(model.SupportedInputModalities),
			SupportedOutputModalities: canonicalStringSet(model.SupportedOutputModalities),
			Thinking:                  thinking,
		})
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:v1:" + hex.EncodeToString(digest[:]), nil
}

type catalogRevisionPayload struct {
	Models []catalogRevisionModel `json:"models"`
}

type catalogRevisionModel struct {
	ID                        string                   `json:"id"`
	DisplayName               string                   `json:"display_name"`
	ContextLength             int64                    `json:"context_length"`
	MaxCompletionTokens       int64                    `json:"max_completion_tokens"`
	SupportedInputModalities  []string                 `json:"supported_input_modalities"`
	SupportedOutputModalities []string                 `json:"supported_output_modalities"`
	Thinking                  *catalogRevisionThinking `json:"thinking,omitempty"`
}

type catalogRevisionThinking struct {
	Min            int      `json:"min"`
	Max            int      `json:"max"`
	ZeroAllowed    bool     `json:"zero_allowed"`
	DynamicAllowed bool     `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

func canonicalStringSet(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func canonicalOrderedStrings(values []string) []string {
	return append([]string{}, values...)
}
