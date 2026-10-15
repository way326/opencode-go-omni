package plugin

import (
	"context"
	"encoding/json"
	"math"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The native ABI carries the host request context separately from the public
// SDK request. Preserve it on the HTTP callback for cancellation and transport.
type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (m *Manager) handleQuotaFetch(request []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed quota fetch request"), nil
	}
	// Only use the credential selected by CPA. Never fall back to the first
	// configured key: imported auth records need not occur in the plugin config.
	// Share inference's source precedence and invalid-source rejection rules,
	// including the metadata shape produced by CPA's generic credential loader.
	key, _, err := selectedCredentialAPIKey(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: req.Provider, AuthAttributes: req.Attributes,
		AuthMetadata: req.Metadata, StorageJSON: req.StorageJSON,
	}})
	if err != nil {
		return ErrEnvelope("invalid_request", err.Error()), nil
	}
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	usage, err := fetchQuotaWithCallback(context.Background(), m.bridge, baseURL, timeout, key, req.HostCallbackID)
	if err != nil {
		return ErrEnvelope("quota_failure", err.Error()), nil
	}
	return okEnvelope(normalizeQuota(usage)), nil
}

func normalizeQuota(usage quotaUsage) pluginapi.QuotaFetchResponse {
	buckets := make([]pluginapi.QuotaBucket, 0, 3)
	for _, entry := range []struct {
		name   string
		window quotaWindow
	}{
		{"rolling", usage.Rolling},
		{"weekly", usage.Weekly},
		{"monthly", usage.Monthly},
	} {
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            entry.name,
			RemainingFraction: math.Max(0, math.Min(1, 1-entry.window.Percent/100)),
			ResetTime:         entry.window.ResetsAt,
			Description:       entry.name + " (" + entry.window.Status + ")",
		})
	}
	// These are the windows returned by /usage. They are not independently
	// measured per-model limits, nor enough to infer a plan or dollar balance.
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{
		DisplayName: "OpenCode Go", Buckets: buckets,
	}}}
}
