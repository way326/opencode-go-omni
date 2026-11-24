package plugin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/config"
)

const keySourceCredentialID keySource = "credential_id"

var errUnknownCredentialID = errors.New("selected credential id is unknown")

// credentialBinding keeps the selected key and the immutable routing snapshot
// that must be used with it. This prevents a staged key from being combined
// with an unrelated base URL/catalog generation during lifecycle transitions.
type credentialBinding struct {
	key         string
	storedValue string
	cfg         config.Config
	mgr         *catalog.Manager
}

func buildCredentialDirectory(cfg config.Config, mgr *catalog.Manager) (map[string]credentialBinding, error) {
	directory := make(map[string]credentialBinding, len(cfg.APIKeys))
	for i, configured := range cfg.APIKeys {
		id := configured.CredentialID
		if id == "" {
			id = config.APIKeyCredentialID(configured.Value)
		}
		if !isCanonicalCredentialID(id) || id != config.APIKeyCredentialID(configured.Value) {
			return nil, fmt.Errorf("api-keys[%d].credential_id: invalid credential identity", i)
		}
		key, err := usableAPIKey(configured.Value)
		if err != nil {
			return nil, fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
		if _, exists := directory[id]; exists {
			return nil, fmt.Errorf("api-keys: duplicate credential ids are not allowed")
		}
		directory[id] = credentialBinding{key: key, storedValue: configured.Value, cfg: cfg, mgr: mgr}
	}
	return directory, nil
}

// selectedCredential resolves reference-only auth records using their CPA
// AuthID. Noncanonical AuthIDs remain an explicit compatibility path for old
// imported credentials that carry their own selected api_key.
func (m *Manager) selectedCredential(req executorRequest) (credentialBinding, keySource, error) {
	if req.AuthProvider != ProviderID {
		return credentialBinding{}, "", fmt.Errorf("selected auth provider is not %s", ProviderID)
	}
	if strings.HasPrefix(req.AuthID, credentialIDPrefix) {
		if !isCanonicalCredentialID(req.AuthID) {
			return credentialBinding{}, keySourceCredentialID, fmt.Errorf("selected credential id has invalid format")
		}
		m.mu.RLock()
		binding, ok := m.credentialDirectory[req.AuthID]
		if !ok {
			binding, ok = m.stagedCredentialDirectory[req.AuthID]
		}
		m.mu.RUnlock()
		if !ok {
			return credentialBinding{}, keySourceCredentialID, errUnknownCredentialID
		}
		if source, err := validateLegacyCredentialConsistency(req, binding.key); err != nil {
			return credentialBinding{}, source, err
		}
		return binding, keySourceCredentialID, nil
	}
	key, source, err := selectedCredentialAPIKey(req)
	if err != nil {
		return credentialBinding{}, source, err
	}
	m.mu.RLock()
	binding := credentialBinding{key: key, cfg: m.cfg, mgr: m.mgr}
	m.mu.RUnlock()
	return binding, source, nil
}

// validateLegacyCredentialConsistency checks any legacy key material still
// attached to a canonical reference. It never selects a legacy value; the
// directory mapping remains authoritative.
func validateLegacyCredentialConsistency(req executorRequest, expected string) (keySource, error) {
	if value, ok := req.AuthAttributes["api_key"]; ok {
		key, err := usableAPIKey(value)
		if err != nil {
			return keySourceAuthAttributes, err
		}
		if key != expected {
			return keySourceAuthAttributes, fmt.Errorf("selected credential legacy key conflicts with auth id")
		}
	}
	if raw, ok := req.AuthMetadata["api_key"]; ok {
		text, valid := raw.(string)
		if !valid {
			return keySourceAuthMetadata, fmt.Errorf("selected credential %s api_key has invalid type", keySourceAuthMetadata)
		}
		key, err := usableAPIKey(text)
		if err != nil {
			return keySourceAuthMetadata, err
		}
		if key != expected {
			return keySourceAuthMetadata, fmt.Errorf("selected credential legacy key conflicts with auth id")
		}
	}
	if len(req.StorageJSON) > 0 {
		key, source, err := apiKeyFromStorageJSON(req.StorageJSON)
		if err != nil && !errors.Is(err, errSelectedCredentialNoAPIKey) {
			return source, err
		}
		if err == nil && key != expected {
			return keySourceStorageJSON, fmt.Errorf("selected credential legacy key conflicts with auth id")
		}
	}
	return keySourceCredentialID, nil
}

func (m *Manager) resetStagedCredentialDirectory() {
	m.mu.Lock()
	m.stagedCredentialDirectory = make(map[string]credentialBinding)
	m.mu.Unlock()
}

func (m *Manager) stageCredentialBinding(id string, binding credentialBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, active := m.credentialDirectory[id]; active {
		return
	}
	if m.stagedCredentialDirectory == nil {
		m.stagedCredentialDirectory = make(map[string]credentialBinding)
	}
	m.stagedCredentialDirectory[id] = binding
}
