package plugin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
	"github.com/way326/opencode-go-omni/resources"
)

type quotaWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
}

type quotaUsage struct {
	Rolling quotaWindow `json:"rolling"`
	Weekly  quotaWindow `json:"weekly"`
	Monthly quotaWindow `json:"monthly"`
}

type quotaCard struct {
	KeyID string      `json:"key_id"`
	Label string      `json:"label"`
	Usage *quotaUsage `json:"usage,omitempty"`
}

type quotaRequest struct {
	KeyID string `json:"key_id"`
}

type usageResponse struct {
	Usage struct {
		Rolling *quotaUpstreamWindow `json:"rolling"`
		Weekly  *quotaUpstreamWindow `json:"weekly"`
		Monthly *quotaUpstreamWindow `json:"monthly"`
	} `json:"usage"`
}

type quotaUpstreamWindow struct {
	Status   string   `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

func quotaIdentity(key string) (id, label string) {
	id = config.APIKeyCredentialID(key)
	hash := strings.TrimPrefix(id, config.APIKeyCredentialPrefix)
	return id, "OpenCode Go credential " + hash[:12]
}

func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	// CPA v8 exposes versioned management routes; keep the legacy v0 alias
	// usable for existing management clients and the inherited tests.
	version := "/v8"
	if strings.HasPrefix(req.Path, "/v0/") {
		version = "/v0"
	}
	if req.Method == http.MethodGet && req.Path == version+"/resource/plugins/"+pluginName+"/quota" {
		return quotaPageResponse(version)
	}
	if req.Method != http.MethodPost || req.Path != version+"/management/plugins/"+pluginName+"/quota-usage" {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}, nil
	}
	var body quotaRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid request"}`)}, nil
		}
	}
	m.mu.RLock()
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	directory := make(map[string]credentialBinding, len(m.credentialDirectory))
	for id, binding := range m.credentialDirectory {
		directory[id] = binding
	}
	m.mu.RUnlock()
	if body.KeyID == "" {
		cards := make([]quotaCard, 0, len(keys))
		for _, key := range keys {
			id := key.CredentialID
			if id == "" {
				id = config.APIKeyCredentialID(key.Value)
			}
			binding, ok := directory[id]
			if !ok {
				continue
			}
			_, label := quotaIdentity(binding.key)
			cards = append(cards, quotaCard{KeyID: id, Label: label})
		}
		return quotaJSON(quotaList{Cards: cards})
	}
	for _, key := range keys {
		id := key.CredentialID
		if id == "" {
			id = config.APIKeyCredentialID(key.Value)
		}
		if id != body.KeyID {
			continue
		}
		binding, ok := directory[id]
		if !ok {
			return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"unknown quota key"}`)}, nil
		}
		_, label := quotaIdentity(binding.key)
		usage, err := fetchQuota(ctx, m.bridge, binding.cfg.BaseURL, binding.cfg.RequestTimeout, binding.key)
		if err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":"quota refresh failed"}`)}, nil
		}
		return quotaJSON(quotaCard{KeyID: id, Label: label, Usage: &usage})
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"unknown quota key"}`)}, nil
}

func quotaPageResponse(version string) (pluginapi.ManagementResponse, error) {
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("generate quota page nonce")
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	page := strings.ReplaceAll(resources.QuotaPage, "__QUOTA_ENDPOINT__", version+"/management/plugins/"+pluginName+"/quota-usage")
	if strings.Count(page, "<script>") != 2 || strings.Count(page, "<style>") != 1 {
		return pluginapi.ManagementResponse{}, fmt.Errorf("quota page inline resource shape changed")
	}
	page = strings.ReplaceAll(page, "<script>", `<script nonce="`+nonce+`">`)
	page = strings.ReplaceAll(page, "<style>", `<style nonce="`+nonce+`">`)

	csp := "default-src 'none'; " +
		"script-src 'nonce-" + nonce + "'; " +
		"style-src 'nonce-" + nonce + "'; " +
		"connect-src 'self'; base-uri 'none'; form-action 'none'; " +
		"frame-ancestors 'self'; object-src 'none'"
	return pluginapi.ManagementResponse{Headers: http.Header{
		"Content-Type":            []string{"text/html; charset=utf-8"},
		"Content-Security-Policy": []string{csp},
		"Cache-Control":           []string{"no-store"},
		"X-Content-Type-Options":  []string{"nosniff"},
		"X-Frame-Options":         []string{"SAMEORIGIN"},
		"Referrer-Policy":         []string{"no-referrer"},
	}, Body: []byte(page)}, nil
}

type quotaList struct {
	Cards []quotaCard `json:"cards"`
}

func quotaJSON(v any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("quota response encoding failed")
	}
	return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
}

func fetchQuota(ctx context.Context, bridge *HostBridge, baseURL string, timeout time.Duration, key string) (quotaUsage, error) {
	return fetchQuotaWithCallback(ctx, bridge, baseURL, timeout, key, "")
}

func fetchQuotaWithCallback(ctx context.Context, bridge *HostBridge, baseURL string, timeout time.Duration, key, callbackID string) (quotaUsage, error) {
	if bridge == nil {
		return quotaUsage{}, fmt.Errorf("quota bridge unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := bridge.DoWithCallback(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: strings.TrimRight(baseURL, "/") + "/usage", Headers: http.Header{"Authorization": []string{"Bearer " + key}, "Accept": []string{"application/json"}}}, callbackID)
	if err != nil || resp.StatusCode != http.StatusOK {
		return quotaUsage{}, fmt.Errorf("quota upstream request failed")
	}
	var decoded usageResponse
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		return quotaUsage{}, fmt.Errorf("quota response invalid")
	}
	for _, window := range []*quotaUpstreamWindow{decoded.Usage.Rolling, decoded.Usage.Weekly, decoded.Usage.Monthly} {
		if window == nil || strings.TrimSpace(window.Status) == "" || window.Percent == nil || math.IsNaN(*window.Percent) || math.IsInf(*window.Percent, 0) || *window.Percent < 0 {
			return quotaUsage{}, fmt.Errorf("quota response has invalid or missing windows")
		}
		if window.ResetsAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, window.ResetsAt); err != nil {
				return quotaUsage{}, fmt.Errorf("quota response has invalid reset time")
			}
		}
	}
	return quotaUsage{
		Rolling: quotaWindow{Status: decoded.Usage.Rolling.Status, Percent: *decoded.Usage.Rolling.Percent, ResetsAt: decoded.Usage.Rolling.ResetsAt},
		Weekly:  quotaWindow{Status: decoded.Usage.Weekly.Status, Percent: *decoded.Usage.Weekly.Percent, ResetsAt: decoded.Usage.Weekly.ResetsAt},
		Monthly: quotaWindow{Status: decoded.Usage.Monthly.Status, Percent: *decoded.Usage.Monthly.Percent, ResetsAt: decoded.Usage.Monthly.ResetsAt},
	}, nil
}
