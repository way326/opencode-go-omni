package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// withKey satisfies the at-least-one-key rule (spec 04 §3) for fixtures
// that target a later validation check.
const withKey = "api-keys:\n  - value: sk-dummy\n"

func TestLoadMinimalAppliesAllDefaults(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal) failed: %v", err)
	}
	if c.BaseURL != "https://opencode.ai/inference/go/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.CatalogURL != "https://opencode.ai/inference/go/v1/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "opencode-go" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "sk-dummy" {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if c.Catalog.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if !c.Catalog.StaleWhileUnavailable {
		t.Errorf("StaleWhileUnavailable = false")
	}
	if !c.Protocols.ChatCompletions || !c.Protocols.Messages || !c.Protocols.Responses {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	if !c.Protocols.Gemini {
		t.Errorf("Protocols.Gemini = false")
	}
	if c.AllowHTTP {
		t.Errorf("AllowHTTP = true")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 67108864 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
}

func TestLoadFullConfigHonorsEveryField(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "test-secret-value")
	doc := `
enabled: true
future-key: 1
base-url: http://localhost:9090/v1
catalog-url: https://example.com/models
model-prefix:
  enabled: true
  value: myprefix
api-keys:
  - value: ${TEST_OG_KEY}
catalog:
  refresh-interval: 1h
  stale-while-unavailable: false
protocols:
  chat-completions: false
  messages: false
  responses: false
route-overrides:
  gpt-5.6-luna:
    protocol: responses
    endpoint: /v1/responses
allow-http: true
request-timeout: 5m
max-response-bytes: 1024
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load(full) failed: %v", err)
	}
	if c.BaseURL != "http://localhost:9090/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.CatalogURL != "https://example.com/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "myprefix" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "test-secret-value" {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if c.Catalog.RefreshInterval != time.Hour {
		t.Errorf("RefreshInterval = %v", c.Catalog.RefreshInterval)
	}
	if c.Catalog.StaleWhileUnavailable {
		t.Errorf("StaleWhileUnavailable = true")
	}
	if c.Protocols.ChatCompletions || c.Protocols.Messages || c.Protocols.Responses {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	o, ok := c.RouteOverrides["gpt-5.6-luna"]
	if !ok || o.Protocol != "responses" || o.Endpoint != "/v1/responses" {
		t.Errorf("RouteOverrides = %+v", c.RouteOverrides)
	}
	if !c.AllowHTTP {
		t.Errorf("AllowHTTP = false")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 1024 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("TEST_OG_KEY", "expanded-secret")
	c, err := Load([]byte("api-keys:\n  - value: ${TEST_OG_KEY}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "expanded-secret" {
		t.Fatalf("APIKeys = %+v", c.APIKeys)
	}
	if c.APIKeys[0].Source != APIKeySourceEnvironment || c.APIKeys[0].Ref != "TEST_OG_KEY" {
		t.Fatalf("environment source metadata = %+v", c.APIKeys[0])
	}
}

func TestLoadRejectsWhitespaceOnlyAPIKeyWithoutEcho(t *testing.T) {
	_, err := Load([]byte("api-keys:\n  - value: \" \\t  \"\n"))
	if err == nil || err.Error() != "api-keys[0].value: expanded to empty" {
		t.Fatalf("whitespace-only API key error = %v", err)
	}
}

func TestLoadRejectsKeysThatNormalizeToTheSameExecutionValue(t *testing.T) {
	_, err := Load([]byte("api-keys:\n  - value: api-key\n  - value: ' api-key '\n"))
	if err == nil || err.Error() != "api-keys: duplicate key values after whitespace normalization are not allowed" {
		t.Fatalf("normalized duplicate error = %v", err)
	}
}

func TestAPIKeyMetadataAndCredentialID(t *testing.T) {
	t.Setenv("TEST_OG_ENV_KEY", "expanded-secret")
	doc := "api-keys:\n  - value: sk-dummy\n  - value: ${TEST_OG_ENV_KEY}\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.APIKeys) != 2 {
		t.Fatalf("APIKeys length = %d, want 2", len(c.APIKeys))
	}
	literal, env := c.APIKeys[0], c.APIKeys[1]
	if literal.Source != APIKeySourceLiteral || literal.Ref != "" {
		t.Fatalf("literal source metadata = %+v", literal)
	}
	if got, want := literal.CredentialID, "opencode-go-key-1eaa840f0577b0dbef7d091e41827c315610880057abaffd31736cac13356010"; got != want {
		t.Fatalf("literal credential ID = %q, want %q", got, want)
	}
	if env.Source != APIKeySourceEnvironment || env.Ref != "TEST_OG_ENV_KEY" {
		t.Fatalf("environment source metadata = %+v", env)
	}
	if got, want := env.CredentialID, "opencode-go-key-e97dffda87a118344e253cb4d25ef3e0df682d786e6c5461469c396cdc5237a6"; got != want {
		t.Fatalf("environment credential ID = %q, want %q", got, want)
	}
	if got, want := env.CredentialID, APIKeyCredentialID(env.Value); got != want {
		t.Fatalf("credential ID helper mismatch: got %q, want %q", got, want)
	}
}

func TestAPIKeyMixedSourceRetainsNoConfiguredText(t *testing.T) {
	t.Setenv("TEST_OG_MIX_A", "alpha")
	t.Setenv("TEST_OG_MIX_B", "beta")
	const literalSegment = "prefix-route-secret-"
	doc := "api-keys:\n  - value: " + literalSegment + "${TEST_OG_MIX_A}-${TEST_OG_MIX_B}\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	key := c.APIKeys[0]
	if key.Value != literalSegment+"alpha-beta" {
		t.Fatalf("expanded value = %q", key.Value)
	}
	if key.Source != APIKeySourceMixed || key.Ref != "" {
		t.Fatalf("mixed source metadata = %+v", key)
	}
	formatted := key.String()
	if strings.Contains(formatted, key.Value) || strings.Contains(formatted, "alpha") || strings.Contains(formatted, "route-secret") || strings.Contains(formatted, "TEST_OG_MIX") {
		t.Fatalf("formatted APIKey leaked source material: %q", formatted)
	}
}

func TestPureEnvReferenceOnlyRetainsVariableName(t *testing.T) {
	tests := []struct {
		raw  string
		name string
		ok   bool
	}{
		{"${TEST_OG_NAME}", "TEST_OG_NAME", true},
		{"${_TEST_OG_NAME2}", "_TEST_OG_NAME2", true},
		{"${}", "", false},
		{"${1BAD}", "", false},
		{"prefix-${TEST_OG_NAME}", "", false},
		{"${TEST_OG_NAME}${OTHER}", "", false},
		{"$TEST_OG_NAME", "", false},
		{"${TEST_OG_NAME:-fallback}", "", false},
		{"literal", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			name, ok := pureEnvReference(tt.raw)
			if name != tt.name || ok != tt.ok {
				t.Fatalf("pureEnvReference(%q) = (%q, %t), want (%q, %t)", tt.raw, name, ok, tt.name, tt.ok)
			}
		})
	}
}

func TestAPIKeyNeverSerializesOrFormatsResolvedValue(t *testing.T) {
	t.Setenv("TEST_OG_SERIALIZE_KEY", "serialized-secret")
	c, err := Load([]byte("api-keys:\n  - value: ${TEST_OG_SERIALIZE_KEY}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	raw, err := json.Marshal(c.APIKeys)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	formatted := fmt.Sprintf("value=%v plus=%+v direct=%#v slice=%+v", c.APIKeys[0], c.APIKeys[0], c.APIKeys[0], c.APIKeys)
	for label, text := range map[string]string{"json": string(raw), "format": formatted} {
		if strings.Contains(text, "serialized-secret") {
			t.Fatalf("%s leaked resolved key: %q", label, text)
		}
	}
	if !strings.Contains(string(raw), `"credential_id":"opencode-go-key-`) {
		t.Fatalf("JSON lacks non-secret credential ID: %s", raw)
	}
}

func TestCredentialIDUsesExactResolvedBytes(t *testing.T) {
	if got, want := APIKeyCredentialID("alpha"), "opencode-go-key-8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8"; got != want {
		t.Fatalf("APIKeyCredentialID(alpha) = %q, want %q", got, want)
	}
	if APIKeyCredentialID("alpha") == APIKeyCredentialID(" alpha ") {
		t.Fatal("credential ID incorrectly trims resolved bytes")
	}
	if APIKeyCredentialID("alpha") == APIKeyCredentialID("beta") {
		t.Fatal("different values produced the same credential ID")
	}
}

func TestValidateAPIKeyCredentialMetadata(t *testing.T) {
	c, err := Load([]byte("api-keys:\n  - value: alpha\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	wrongID := c
	wrongID.APIKeys = append([]APIKey(nil), c.APIKeys...)
	wrongID.APIKeys[0].CredentialID = APIKeyCredentialID("different-value")
	if err := wrongID.validate(); err == nil || !strings.Contains(err.Error(), "credential_id: does not match resolved value") {
		t.Fatalf("wrong credential ID error = %v", err)
	}

	badRef := c
	badRef.APIKeys = append([]APIKey(nil), c.APIKeys...)
	badRef.APIKeys[0].Ref = "SHOULD_NOT_BE_STORED"
	if err := badRef.validate(); err == nil || !strings.Contains(err.Error(), "ref: only valid for an environment reference") {
		t.Fatalf("literal ref error = %v", err)
	}

	badSource := c
	badSource.APIKeys = append([]APIKey(nil), c.APIKeys...)
	badSource.APIKeys[0].Source = APIKeySource("raw")
	if err := badSource.validate(); err == nil || !strings.Contains(err.Error(), "source: unsupported credential source") {
		t.Fatalf("unsupported source error = %v", err)
	}
}

func TestLoadIgnoresRemovedSchedulerFields(t *testing.T) {
	doc := "routing:\n  strategy: round-robin\n  max-retries-per-request: 9\n  transient-retry-interval: 1s\n  fallback-cooldown: 1m\n" +
		"api-keys:\n  - value: sk-dummy\n    priority: 7\n"
	if _, err := Load([]byte(doc)); err != nil {
		t.Fatalf("removed scheduler fields must remain unknown and ignored: %v", err)
	}
}

func TestLoadDerivedCatalogURLFromCustomBase(t *testing.T) {
	c, err := Load([]byte("base-url: https://upstream.example.com/api\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://upstream.example.com/api/models" {
		t.Errorf("CatalogURL = %q", c.CatalogURL)
	}
}

func TestLoadDerivedCatalogURLTrimsTrailingSlash(t *testing.T) {
	c, err := Load([]byte("base-url: https://opencode.ai/zen/go/v1/\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://opencode.ai/zen/go/v1/models" {
		t.Errorf("CatalogURL = %q, want no double slash", c.CatalogURL)
	}
}

// An EXPLICIT catalog-url ending in "/" must normalize the same way as the
// derived default, so the stored endpoint never carries a trailing slash.
func TestLoadExplicitCatalogURLTrimsTrailingSlash(t *testing.T) {
	c, err := Load([]byte("catalog-url: https://mirror.example.com/api/models///\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if c.CatalogURL != "https://mirror.example.com/api/models" {
		t.Errorf("CatalogURL = %q, want trailing slashes trimmed", c.CatalogURL)
	}
}

func TestPrefixDisabledSkipsValueValidation(t *testing.T) {
	doc := `
model-prefix:
  enabled: false
  value: "has space"
` + withKey
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := PublicID(c, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID = %q", got)
	}
}

func TestPublicID(t *testing.T) {
	enabled := Config{ModelPrefix: ModelPrefix{Enabled: true, Value: "opencode-go"}}
	if got := PublicID(enabled, "glm-5.2"); got != "opencode-go/glm-5.2" {
		t.Errorf("PublicID(enabled) = %q", got)
	}
	disabled := Config{ModelPrefix: ModelPrefix{Enabled: false}}
	if got := PublicID(disabled, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("PublicID(disabled) = %q", got)
	}
}

func TestLoadRejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"invalid yaml syntax", "[unclosed", "decode config"},
		{"unknown anchor has no line info", "request-timeout: *nope\n", "decode config: invalid YAML structure"},
		{"bare-scalar api key never leaks", "api-keys:\n  - sk-live-secret-123\n", "decode config: invalid YAML structure"},
		{"keyless config rejected", "", "api-keys: at least one key is required"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{
			"duplicate route-override keys",
			"route-overrides:\n" +
				"  gpt-5.6-luna:\n    protocol: responses\n    endpoint: /v1/responses\n" +
				"  gpt-5.6-luna:\n    protocol: messages\n    endpoint: /v1/messages\n",
			"decode config: invalid YAML structure near line",
		},
		{
			"key expands to empty",
			"api-keys:\n  - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[0].value: expanded to empty",
		},
		{
			"second key expands to empty",
			"api-keys:\n  - value: literal-ok\n  - value: ${TEST_OG_DEFINITELY_UNSET_VAR}\n",
			"api-keys[1].value: expanded to empty",
		},
		{"explicitly empty base-url", `base-url: ""`, "base-url: must not be empty"},
		{"explicitly empty catalog-url", `catalog-url: ""`, "catalog-url: must not be empty"},
		{"malformed base-url", `base-url: "::::"`, "base-url: invalid URL"},
		{"scheme-less base-url", "base-url: opencode.ai/zen/go/v1", "base-url: invalid URL"},
		{"malformed catalog-url", `catalog-url: "http://exa mple.com"`, "catalog-url: invalid URL"},
		{"http base-url without allow-http", "base-url: http://localhost:9090/v1", "base-url: http scheme requires allow-http"},
		{"http catalog-url without allow-http", "catalog-url: http://localhost:9090/models", "catalog-url: http scheme requires allow-http"},
		{"ftp base-url", "base-url: ftp://files.example.com/v1", `base-url: unsupported scheme "ftp"`},
		{"file catalog-url", "catalog-url: file://host/share/models", `catalog-url: unsupported scheme "file"`},
		{"ftp base-url with allow-http", "allow-http: true\nbase-url: ftp://files.example.com/v1", `base-url: unsupported scheme "ftp"`},
		{"file catalog-url with allow-http", "allow-http: true\ncatalog-url: file://host/share/models", `catalog-url: unsupported scheme "file"`},
		{"refresh-interval missing unit", "catalog:\n  refresh-interval: 5\n", "catalog.refresh-interval: invalid duration"},
		{"refresh-interval negative", "catalog:\n  refresh-interval: -5m\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"refresh-interval below floor", "catalog:\n  refresh-interval: 1ms\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"request-timeout unparsable", "request-timeout: bogus\n", "request-timeout: invalid duration"},
		{"request-timeout explicitly empty", `request-timeout: ""`, "request-timeout: invalid duration"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{"userinfo credentials never echoed", "base-url: https://user:sekret@/v1", "base-url: invalid URL https://"},
		{"max-response-bytes zero rejected", "max-response-bytes: 0\n" + withKey, "max-response-bytes: must be positive"},
		{"max-response-bytes negative rejected", "max-response-bytes: -5\n" + withKey, "max-response-bytes: must be positive"},
		{
			"override unsupported protocol",
			"route-overrides:\n  bad-model:\n    protocol: grpc\n    endpoint: /v1/x\n" + withKey,
			"route-overrides[bad-model].protocol: unsupported protocol",
		},
		{
			"override empty endpoint",
			"route-overrides:\n  bad-model:\n    protocol: responses\n    endpoint: \"\"\n" + withKey,
			"route-overrides[bad-model].endpoint: must not be empty",
		},
		{
			"override endpoint without leading slash",
			"route-overrides:\n  bad-model:\n    protocol: responses\n    endpoint: v1/x\n" + withKey,
			"route-overrides[bad-model].endpoint: must start with /",
		},
		{"prefix enabled empty value", "model-prefix:\n  enabled: true\n  value: \"\"\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix with space", "model-prefix:\n  value: has space\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix leading dash", "model-prefix:\n  value: -lead\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			requireErrContains(t, err, tt.wantErr)
			if tt.name == "key expands to empty" && strings.Contains(err.Error(), "${") {
				t.Errorf("error leaks environment reference: %q", err.Error())
			}
		})
	}
}

// TestLoadErrorsNeverLeakDecodedValues pins the security property: decode
// and validation failures must not echo raw scalars (a bare-scalar API key)
// or embedded userinfo credentials into the error the host logs.
func TestLoadErrorsNeverLeakDecodedValues(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		secret string
	}{
		{"bare-scalar api key", "api-keys:\n  - sk-live-supersecret-42\n", "sk-live-supersecret-42"},
		{"duplicate api key values", "api-keys:\n  - value: sk-live-dupsecret-7\n  - value: sk-live-dupsecret-7\n", "sk-live-dupsecret-7"},
		{"userinfo credentials", "base-url: https://admin:p4ssw0rd@/v1\n", "p4ssw0rd"},
	}
	for _, tc := range cases {
		_, err := Load([]byte(tc.yaml))
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("%s: error leaks secret %q: %v", tc.name, tc.secret, err)
		}
	}
}

// TestValidateURLRejectsQueryFragmentUserinfo pins the fix for bases that
// parse fine but break JoinUpstreamURL: a query or fragment truncates the
// joined path (…/v1?tenant=x/chat/completions), and userinfo leaks
// credentials into every upstream request URL.
func TestValidateURLRejectsQueryFragmentUserinfo(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		httpOK  bool
		wantErr string
	}{
		{"query rejected naming setting", "https://host/v1?tenant=x", false, "base-url: must not contain query"},
		{"fragment rejected", "https://host/v1#frag", false, "must not contain query, fragment, or userinfo"},
		{"userinfo rejected without echoing credentials", "https://user:key@host/v1", false, "must not contain query, fragment, or userinfo"},
		{"clean https passes", "https://host/v1", false, ""},
		{"http with allow-http passes", "http://localhost:9090/v1", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateURL("base-url", tt.raw, tt.httpOK)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateURL(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			requireErrContains(t, err, tt.wantErr)
			if strings.Contains(err.Error(), "key") && strings.Contains(tt.raw, ":key@") {
				t.Errorf("error echoes credentials: %q", err.Error())
			}
		})
	}
}
