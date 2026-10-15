package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// keySource identifies which credential slot produced the selected API key.
// The value is deliberately non-secret so it can travel into logs and errors.
type keySource string

const (
	keySourceAuthAttributes keySource = "auth_attributes"
	keySourceAuthMetadata   keySource = "auth_metadata"
	keySourceStorageJSON    keySource = "storage_json"
)

// errSelectedCredentialNoAPIKey is returned when the selected credential
// carries no usable api_key. It never embeds key material.
var errSelectedCredentialNoAPIKey = errors.New("selected credential has no api key")

// selectedCredentialAPIKey extracts the api_key of the ONE credential CPA
// selected for this executor request.
//
// Source precedence is deterministic:
//  1. AuthAttributes["api_key"] (immutable routing attributes; historical shape)
//  2. AuthMetadata["api_key"]   (host-managed metadata, e.g. SynthesizeAuthFile)
//  3. StorageJSON {"api_key": ...} (legacy persisted auth file written by the
//     plugin materializeAuthRecords helper)
//
// It intentionally never consults Manager.cfg.APIKeys: falling back to a
// configured key would silently run the request under a different credential
// than CPA scheduled (breaking rotation, cooldown, and per-key accounting) and
// would reintroduce the cross-provider key mixing this path guards against.
// A candidate that is present but blank, wrong-typed, or unparsable is
// rejected instead of skipped: a malformed higher-priority source is a defect,
// not a reason to serve a lower-priority key. Cross-provider selections are
// rejected before any lookup. Returned errors contain no key material.
func selectedCredentialAPIKey(req executorRequest) (string, keySource, error) {
	if req.AuthProvider != ProviderID {
		return "", "", fmt.Errorf("selected auth provider is not %s", ProviderID)
	}
	if raw, ok := req.AuthAttributes["api_key"]; ok {
		key, err := usableAPIKey(raw)
		if err != nil {
			return "", keySourceAuthAttributes, err
		}
		return key, keySourceAuthAttributes, nil
	}
	if raw, ok := req.AuthMetadata["api_key"]; ok {
		text, ok := raw.(string)
		if !ok {
			return "", keySourceAuthMetadata, fmt.Errorf("selected credential %s api_key has invalid type", keySourceAuthMetadata)
		}
		key, err := usableAPIKey(text)
		if err != nil {
			return "", keySourceAuthMetadata, err
		}
		return key, keySourceAuthMetadata, nil
	}
	return apiKeyFromStorageJSON(req.StorageJSON)
}

// apiKeyFromStorageJSON is the legacy fallback for credential files that only
// carry raw persisted JSON. Only a well-formed JSON object with a non-empty
// string api_key is accepted; any other payload fails explicitly so callers
// cannot mistake missing key material for a usable empty key.
func apiKeyFromStorageJSON(raw []byte) (string, keySource, error) {
	if len(raw) == 0 {
		return "", "", errSelectedCredentialNoAPIKey
	}
	var record struct {
		APIKey json.RawMessage `json:"api_key"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", keySourceStorageJSON, fmt.Errorf("selected credential %s is not valid JSON", keySourceStorageJSON)
	}
	if len(record.APIKey) == 0 {
		return "", keySourceStorageJSON, errSelectedCredentialNoAPIKey
	}
	var value string
	if err := json.Unmarshal(record.APIKey, &value); err != nil {
		return "", keySourceStorageJSON, fmt.Errorf("selected credential %s api_key has invalid type", keySourceStorageJSON)
	}
	key, err := usableAPIKey(value)
	if err != nil {
		return "", keySourceStorageJSON, err
	}
	return key, keySourceStorageJSON, nil
}

// usableAPIKey trims surrounding whitespace and rejects empty results.
func usableAPIKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errSelectedCredentialNoAPIKey
	}
	return key, nil
}
