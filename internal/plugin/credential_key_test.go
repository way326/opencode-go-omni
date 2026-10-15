package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ckLegacyStorage mirrors a persisted auth file produced by the previous
// ParseAuth path (and by materializeAuthRecords): the key lives in the raw
// JSON, not in Attributes.
var ckLegacyStorage = []byte("{\"type\":\"opencode-go\",\"id\":\"legacy\",\"api_key\":\"sk-legacy-storage\"}")

func ckExecutorRequest(attrs map[string]string, metadata map[string]any, storage []byte) executorRequest {
	return executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider:   ProviderID,
		AuthID:         "opencode-go-key-selected",
		AuthAttributes: attrs,
		AuthMetadata:   metadata,
		StorageJSON:    storage,
	}}
}

func TestSelectedCredentialAPIKeySourcePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		req        executorRequest
		wantKey    string
		wantSource keySource
	}{
		{
			name:       "attributes win over metadata and storage",
			req:        ckExecutorRequest(map[string]string{"api_key": "sk-attr"}, map[string]any{"api_key": "sk-meta"}, ckLegacyStorage),
			wantKey:    "sk-attr",
			wantSource: keySourceAuthAttributes,
		},
		{
			name:       "legacy attributes shape",
			req:        ckExecutorRequest(map[string]string{"api_key": "sk-history"}, nil, nil),
			wantKey:    "sk-history",
			wantSource: keySourceAuthAttributes,
		},
		{
			name:       "metadata wins over storage",
			req:        ckExecutorRequest(nil, map[string]any{"api_key": "sk-meta"}, ckLegacyStorage),
			wantKey:    "sk-meta",
			wantSource: keySourceAuthMetadata,
		},
		{
			name:       "storage json fallback for existing key files",
			req:        ckExecutorRequest(nil, nil, ckLegacyStorage),
			wantKey:    "sk-legacy-storage",
			wantSource: keySourceStorageJSON,
		},
		{
			name:       "surrounding whitespace is trimmed",
			req:        ckExecutorRequest(map[string]string{"api_key": "  sk-trimmed  "}, nil, nil),
			wantKey:    "sk-trimmed",
			wantSource: keySourceAuthAttributes,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, source, err := selectedCredentialAPIKey(tc.req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key != tc.wantKey || source != tc.wantSource {
				t.Fatalf("key=%q source=%q, want key=%q source=%q", key, source, tc.wantKey, tc.wantSource)
			}
		})
	}
}

func TestSelectedCredentialAPIKeyRejections(t *testing.T) {
	cases := []struct {
		name       string
		req        executorRequest
		wantSource keySource
		wantNoKey  bool
		wantSubstr string
	}{
		{
			name:       "no sources at all",
			req:        ckExecutorRequest(nil, nil, nil),
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
		{
			name:       "empty storage json",
			req:        ckExecutorRequest(nil, nil, []byte("")),
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
		{
			name:       "blank attributes block fallthrough",
			req:        ckExecutorRequest(map[string]string{"api_key": "   "}, map[string]any{"api_key": "sk-meta-shadow"}, ckLegacyStorage),
			wantSource: keySourceAuthAttributes,
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
		{
			name:       "wrong typed metadata blocks fallthrough",
			req:        ckExecutorRequest(nil, map[string]any{"api_key": 42}, ckLegacyStorage),
			wantSource: keySourceAuthMetadata,
			wantSubstr: "invalid type",
		},
		{
			name:       "blank metadata blocks fallthrough",
			req:        ckExecutorRequest(nil, map[string]any{"api_key": " "}, ckLegacyStorage),
			wantSource: keySourceAuthMetadata,
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
		{
			name:       "storage json is malformed",
			req:        ckExecutorRequest(nil, nil, []byte("{\"type\":\"opencode-go\",\"api_key\":\"sk-broken\"")),
			wantSource: keySourceStorageJSON,
			wantSubstr: "not valid JSON",
		},
		{
			name:       "storage api_key wrong type",
			req:        ckExecutorRequest(nil, nil, []byte("{\"type\":\"opencode-go\",\"api_key\":12345}")),
			wantSource: keySourceStorageJSON,
			wantSubstr: "invalid type",
		},
		{
			name:       "storage api_key blank",
			req:        ckExecutorRequest(nil, nil, []byte("{\"type\":\"opencode-go\",\"api_key\":\"   \"}")),
			wantSource: keySourceStorageJSON,
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
		{
			name:       "storage has no api_key field",
			req:        ckExecutorRequest(nil, nil, []byte("{\"type\":\"opencode-go\",\"id\":\"legacy\"}")),
			wantSource: keySourceStorageJSON,
			wantSubstr: "no api key",
			wantNoKey:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, source, err := selectedCredentialAPIKey(tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			if key != "" {
				t.Fatalf("failed lookup returned key material: %q", key)
			}
			if source != tc.wantSource {
				t.Fatalf("source=%q, want %q", source, tc.wantSource)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error=%v, want substring %q", err, tc.wantSubstr)
			}
			if strings.Contains(err.Error(), "sk-") {
				t.Fatalf("error leaks key material: %v", err)
			}
			if tc.wantNoKey && !errors.Is(err, errSelectedCredentialNoAPIKey) {
				t.Fatalf("error=%v, want errSelectedCredentialNoAPIKey", err)
			}
		})
	}
}

func TestSelectedCredentialAPIKeyRejectsCrossProviderSelection(t *testing.T) {
	req := ckExecutorRequest(map[string]string{"api_key": "sk-cross-provider"}, nil, ckLegacyStorage)
	req.AuthProvider = "some-other-provider"
	key, source, err := selectedCredentialAPIKey(req)
	if err == nil || key != "" || source != "" {
		t.Fatalf("cross provider = key=%q source=%q err=%v", key, source, err)
	}
	if !strings.Contains(err.Error(), "not opencode-go") {
		t.Fatalf("cross provider error = %v", err)
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Fatalf("cross provider error leaks key material: %v", err)
	}
}
