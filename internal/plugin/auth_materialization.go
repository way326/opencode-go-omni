package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
)

func (m *Manager) materializeAuthRecordsFromList(ctx context.Context, cfg config.Config, entries []pluginapi.HostAuthFileEntry, candidates map[string]credentialBinding) error {
	if m.bridge == nil {
		return nil
	}
	for _, key := range cfg.APIKeys {
		id := key.CredentialID
		if id == "" {
			id = config.APIKeyCredentialID(key.Value)
		}
		binding, ok := candidates[id]
		if !ok {
			return fmt.Errorf("auth materialization failed: credential directory is inconsistent")
		}
		// Stage each exact mapping immediately before a read/write can expose
		// this AuthID. IDs not yet processed in this batch are never staged.
		m.stageCredentialBinding(id, binding)
		name := id + ".json"
		entry, found, err := findMaterializationTarget(entries, id, name)
		if err != nil {
			return err
		}
		if found {
			if err := m.migrateOrValidateAuthRecord(ctx, entry, key, id, name); err != nil {
				return err
			}
			continue
		}
		record, err := json.Marshal(struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Label string `json:"label"`
		}{
			Type: ProviderID, ID: id, Label: quotaCredentialLabel(id),
		})
		if err != nil {
			return fmt.Errorf("build auth record")
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
			return err
		}
		debugTrace("auth materialized id=%s file=%s credential_ref_present=true", id, name)
	}
	return nil
}

func quotaCredentialLabel(id string) string {
	const prefix = credentialIDPrefix
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+64 {
		return "OpenCode Go credential"
	}
	return "OpenCode Go credential " + strings.TrimPrefix(id, prefix)[:12]
}

func findMaterializationTarget(entries []pluginapi.HostAuthFileEntry, id, name string) (pluginapi.HostAuthFileEntry, bool, error) {
	var target *pluginapi.HostAuthFileEntry
	for i := range entries {
		entry := entries[i]
		matchID := strings.TrimSpace(entry.ID) == id
		matchName := strings.TrimSpace(entry.Name) == name
		if !matchID && !matchName {
			continue
		}
		if !matchID || !matchName {
			return pluginapi.HostAuthFileEntry{}, false, fmt.Errorf("auth materialization refused: credential id/name collision")
		}
		if strings.TrimSpace(entry.AuthIndex) == "" || strings.TrimSpace(entry.Path) == "" || entry.RuntimeOnly || !isOpenCodeGoAuth(entry) {
			return pluginapi.HostAuthFileEntry{}, false, fmt.Errorf("auth materialization refused: existing credential ownership is ambiguous")
		}
		if target != nil && normalizeAuthPath(target.Path) != normalizeAuthPath(entry.Path) {
			return pluginapi.HostAuthFileEntry{}, false, fmt.Errorf("auth materialization refused: duplicate credential paths")
		}
		if target == nil {
			copy := entry
			target = &copy
		}
	}
	if target == nil {
		return pluginapi.HostAuthFileEntry{}, false, nil
	}
	return *target, true, nil
}

func (m *Manager) migrateOrValidateAuthRecord(ctx context.Context, entry pluginapi.HostAuthFileEntry, key config.APIKey, id, name string) error {
	getCtx, cancel := context.WithTimeout(ctx, catalogAuthGetTimeout)
	defer cancel()
	got, err := m.bridge.AuthGet(getCtx, entry.AuthIndex)
	if err != nil {
		return fmt.Errorf("auth materialization failed: cannot read existing credential")
	}
	if got.AuthIndex != entry.AuthIndex || strings.TrimSpace(got.Name) != name || (strings.TrimSpace(got.Path) != "" && normalizeAuthPath(got.Path) != normalizeAuthPath(entry.Path)) {
		return fmt.Errorf("auth materialization refused: auth file identity changed")
	}
	merged, changed, err := referenceOnlyAuthJSON(got.JSON, key.Value, id)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: merged}); err != nil {
		return err
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, catalogAuthGetTimeout)
	defer verifyCancel()
	verified, err := m.bridge.AuthGet(verifyCtx, entry.AuthIndex)
	if err != nil {
		return fmt.Errorf("auth migration verification failed: cannot read saved credential")
	}
	if verified.AuthIndex != entry.AuthIndex || strings.TrimSpace(verified.Name) != name || (strings.TrimSpace(verified.Path) != "" && normalizeAuthPath(verified.Path) != normalizeAuthPath(entry.Path)) {
		return fmt.Errorf("auth migration verification failed: auth file identity changed")
	}
	_, remainsLegacy, verifyErr := referenceOnlyAuthJSON(verified.JSON, key.Value, id)
	if verifyErr != nil || remainsLegacy {
		return fmt.Errorf("auth migration verification failed: credential is not reference-only")
	}
	return nil
}

// referenceOnlyAuthJSON verifies ownership and, for legacy records, removes
// only the matching api_key while preserving every other JSON member.
func referenceOnlyAuthJSON(raw []byte, expectedKey, expectedID string) ([]byte, bool, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false, fmt.Errorf("auth migration refused: credential JSON is invalid")
	}
	var kind, provider, id string
	if value := object["type"]; len(value) > 0 {
		if json.Unmarshal(value, &kind) != nil {
			return nil, false, fmt.Errorf("auth migration refused: credential provider is invalid")
		}
	}
	if value := object["provider"]; len(value) > 0 {
		if json.Unmarshal(value, &provider) != nil {
			return nil, false, fmt.Errorf("auth migration refused: credential provider is invalid")
		}
	}
	if value := object["id"]; len(value) == 0 || json.Unmarshal(value, &id) != nil {
		return nil, false, fmt.Errorf("auth migration refused: credential id is invalid")
	}
	if (kind != "" && kind != ProviderID) || (provider != "" && provider != ProviderID) || (kind == "" && provider == "") || id != expectedID {
		return nil, false, fmt.Errorf("auth migration refused: credential ownership mismatch")
	}
	legacy, hasAPIKey := object["api_key"]
	if !hasAPIKey {
		return raw, false, nil
	}
	var legacyKey string
	if json.Unmarshal(legacy, &legacyKey) != nil || legacyKey == "" {
		return nil, false, fmt.Errorf("auth migration refused: legacy key field is invalid")
	}
	if legacyKey != expectedKey || config.APIKeyCredentialID(legacyKey) != expectedID {
		return nil, false, fmt.Errorf("auth migration refused: legacy key does not match credential id")
	}
	delete(object, "api_key")
	if kind == "" {
		encodedType, _ := json.Marshal(ProviderID)
		object["type"] = encodedType
	}
	var label string
	if len(object["label"]) > 0 && json.Unmarshal(object["label"], &label) != nil {
		return nil, false, fmt.Errorf("auth migration refused: credential label is invalid")
	}
	if strings.TrimSpace(label) == "" {
		encodedLabel, _ := json.Marshal(quotaCredentialLabel(expectedID))
		object["label"] = encodedLabel
	}
	merged, err := json.Marshal(object)
	if err != nil {
		return nil, false, fmt.Errorf("auth migration refused: credential JSON cannot be encoded")
	}
	return merged, true, nil
}

// retainOnlyPersistedStagedCredentials reconciles staged mappings after a
// failed AuthSave batch. If CPA cannot prove a save did not occur, the mapping
// is retained until a later lifecycle can safely reconcile it.
func (m *Manager) retainOnlyPersistedStagedCredentials(ctx context.Context) {
	if m.bridge == nil {
		m.mu.Lock()
		m.stagedCredentialDirectory = nil
		m.mu.Unlock()
		return
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return
	}
	m.mu.RLock()
	staged := make(map[string]credentialBinding, len(m.stagedCredentialDirectory))
	for id, binding := range m.stagedCredentialDirectory {
		staged[id] = binding
	}
	m.mu.RUnlock()
	keep := make(map[string]credentialBinding)
	for id, binding := range staged {
		name := id + ".json"
		entry, found, findErr := findMaterializationTarget(entries, id, name)
		if findErr != nil {
			keep[id] = binding
			continue
		}
		if !found {
			continue
		}
		getCtx, cancel := context.WithTimeout(ctx, catalogAuthGetTimeout)
		got, getErr := m.bridge.AuthGet(getCtx, entry.AuthIndex)
		cancel()
		if getErr != nil || got.AuthIndex != entry.AuthIndex || strings.TrimSpace(got.Name) != name || (strings.TrimSpace(got.Path) != "" && normalizeAuthPath(got.Path) != normalizeAuthPath(entry.Path)) {
			keep[id] = binding
			continue
		}
		_, _, verifyErr := referenceOnlyAuthJSON(got.JSON, binding.storedValue, id)
		if verifyErr == nil {
			keep[id] = binding
		}
	}
	m.mu.Lock()
	m.stagedCredentialDirectory = keep
	m.mu.Unlock()
}
