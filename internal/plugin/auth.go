package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authProvider struct{}

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

func (authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw struct {
		Type     string          `json:"type"`
		Provider string          `json:"provider"`
		ID       string          `json:"id"`
		Label    string          `json:"label"`
		APIKey   json.RawMessage `json:"api_key"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if raw.ID == "" {
		raw.ID = req.FileName
		if strings.HasSuffix(raw.ID, ".json") {
			fileID := strings.TrimSuffix(raw.ID, ".json")
			if isCanonicalCredentialID(fileID) {
				raw.ID = fileID
			}
		}
	}
	if strings.HasPrefix(raw.ID, credentialIDPrefix) && !isCanonicalCredentialID(raw.ID) {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid credential id")
	}
	attributes := map[string]string(nil)
	if len(raw.APIKey) == 0 {
		if !isCanonicalCredentialID(raw.ID) {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go reference auth record has invalid credential id")
		}
	} else {
		var apiKey string
		if json.Unmarshal(raw.APIKey, &apiKey) != nil || strings.TrimSpace(apiKey) == "" {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid api key")
		}
		attributes = map[string]string{"api_key": apiKey}
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s credential_ref_present=%t credential_ref_bytes=%d", req.Provider, req.FileName, raw.ID, isCanonicalCredentialID(raw.ID), len(raw.ID))
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: ProviderID, ID: raw.ID, FileName: req.FileName, Label: raw.Label, StorageJSON: req.RawJSON,
		Attributes: attributes,
	}}, nil
}

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("opencode-go login is unsupported; configure a manual api key")
}

func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("opencode-go login is unsupported; configure a manual api key")
}

func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), mapKeys(req.Attributes), mapKeys(req.Metadata))
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
