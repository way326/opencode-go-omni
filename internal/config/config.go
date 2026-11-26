// Package config loads and validates the opencode-go plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL          = "https://opencode.ai/inference/go/v1"
	DefaultModelPrefix      = "opencode-go"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

// ProtocolGemini is the native Gemini protocol route name (spec 04 §4). The
// catalog package derives its RouteGemini identifier from this value, so the
// protocols.gemini switch and the routed records cannot drift apart.
const (
	ProtocolGemini = "gemini"
	// GeminiModelPlaceholder keeps a route override's model template tied to
	// the cataloged upstream ID instead of pinning a literal model name.
	GeminiModelPlaceholder = "{model}"
	// GeminiActionGenerate and GeminiActionStream are the only routable Gemini
	// actions: the request's stream flag picks between the two, so no other
	// action (countTokens, interactions, or a later PCS action) is a valid
	// override value.
	GeminiActionGenerate = "generateContent"
	GeminiActionStream   = "streamGenerateContent"
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

// APIKeySource is a non-secret classification of how an API key value was
// loaded. SourceMixed deliberately records no source text because composite
// config values may contain literal key material.
type APIKeySource string

const (
	APIKeySourceLiteral     APIKeySource = "literal"
	APIKeySourceEnvironment APIKeySource = "environment"
	APIKeySourceMixed       APIKeySource = "mixed"

	// APIKeyCredentialPrefix is shared with the plugin's materialized auth IDs.
	APIKeyCredentialPrefix = "opencode-go-key-"
)

type APIKey struct {
	// Value is the resolved runtime key. It must never be serialized or logged.
	Value string `json:"-" yaml:"-"`
	// Remark is a user-supplied display name and is safe to persist in the
	// plugin configuration. It is intentionally omitted from runtime JSON.
	Remark string `json:"-" yaml:"-"`
	// Source and Ref are safe diagnostics. Ref is populated only when the
	// entire configured value is exactly ${ENV_VAR}; mixed values retain no text.
	Source       APIKeySource `json:"source" yaml:"-"`
	Ref          string       `json:"ref,omitempty" yaml:"-"`
	CredentialID string       `json:"credential_id" yaml:"-"`
}

// APIKeyCredentialID returns the stable, non-secret fingerprint used by auth
// records and quota cards. It hashes exactly the resolved value bytes, matching
// materializeAuthRecords and quotaIdentity; it does not trim the value.
func APIKeyCredentialID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return APIKeyCredentialPrefix + hex.EncodeToString(digest[:])
}

// String keeps diagnostic formatting from printing Value.
func (k APIKey) String() string {
	return fmt.Sprintf("{Value:<redacted> Source:%s Ref:%s CredentialID:%s}", k.Source, k.Ref, k.CredentialID)
}

// GoString keeps %#v diagnostics from printing Value.
func (k APIKey) GoString() string {
	return fmt.Sprintf("config.APIKey{Value:<redacted> Source:%q Ref:%q CredentialID:%q}", k.Source, k.Ref, k.CredentialID)
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
	Gemini          bool
}

// RouteOverride pins one model to an explicit route (spec 04 §5). Endpoint is
// required for the static routes; the Gemini route may instead build its
// native path from the Model and Action templates.
type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
	Model    string `yaml:"model"`
	Action   string `yaml:"action"`
}

type Config struct {
	BaseURL          string
	CatalogURL       string
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Value  string `yaml:"value"`
	Remark string `yaml:"remark"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
	Gemini          *bool `yaml:"gemini"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true, ProtocolGemini: true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = loadAPIKey(k.Value)
		keys[i].Remark = k.Remark
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		BaseURL: orDefault(raw.BaseURL, DefaultBaseURL),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
			Gemini:          orDefault(raw.Protocols.Gemini, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadAPIKey preserves os.ExpandEnv semantics while recording only safe
// provenance. A pure ${NAME} reference stores NAME; mixed values store no
// source text because that text could contain literal secret material.
func loadAPIKey(configured string) APIKey {
	referenced := false
	value := os.Expand(configured, func(name string) string {
		referenced = true
		return os.Getenv(name)
	})
	key := APIKey{Value: value, Source: APIKeySourceLiteral, CredentialID: APIKeyCredentialID(value)}
	if name, ok := pureEnvReference(configured); ok {
		key.Source = APIKeySourceEnvironment
		key.Ref = name
	} else if referenced {
		key.Source = APIKeySourceMixed
	}
	return key
}

// pureEnvReference accepts only the documented ${NAME} form. Alternate shell
// expansion syntax and composite values are intentionally not retained.
func pureEnvReference(raw string) (string, bool) {
	if len(raw) < 4 || !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
		return "", false
	}
	name := raw[2 : len(raw)-1]
	if !validEnvName(name) {
		return "", false
	}
	return name, true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
		if i == 0 && !letter {
			return false
		}
		if !letter && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	seenValues := make(map[string]bool, len(c.APIKeys))
	seenEffectiveValues := make(map[string]bool, len(c.APIKeys))
	seenIDs := make(map[string]bool, len(c.APIKeys))
	for i, k := range c.APIKeys {
		if strings.TrimSpace(k.Value) == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
		if seenValues[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seenValues[k.Value] = true
		effectiveValue := strings.TrimSpace(k.Value)
		if seenEffectiveValues[effectiveValue] {
			return fmt.Errorf("api-keys: duplicate key values after whitespace normalization are not allowed")
		}
		seenEffectiveValues[effectiveValue] = true

		expectedID := APIKeyCredentialID(k.Value)
		if k.CredentialID != "" && k.CredentialID != expectedID {
			return fmt.Errorf("api-keys[%d].credential_id: does not match resolved value", i)
		}
		credentialID := k.CredentialID
		if credentialID == "" {
			credentialID = expectedID
		}
		if seenIDs[credentialID] {
			return fmt.Errorf("api-keys: duplicate credential ids are not allowed")
		}
		seenIDs[credentialID] = true

		if err := validateAPIKeySource(i, k); err != nil {
			return err
		}
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Protocol == ProtocolGemini {
			if err := validateGeminiOverride(name, o); err != nil {
				return err
			}
			continue
		}
		if o.Model != "" {
			return fmt.Errorf("route-overrides[%s].model: only supported with protocol %q", name, ProtocolGemini)
		}
		if o.Action != "" {
			return fmt.Errorf("route-overrides[%s].action: only supported with protocol %q", name, ProtocolGemini)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateAPIKeySource(index int, key APIKey) error {
	switch key.Source {
	case "":
		if key.Ref != "" {
			return fmt.Errorf("api-keys[%d].ref: requires a credential source", index)
		}
	case APIKeySourceLiteral, APIKeySourceMixed:
		if key.Ref != "" {
			return fmt.Errorf("api-keys[%d].ref: only valid for an environment reference", index)
		}
	case APIKeySourceEnvironment:
		if !validEnvName(key.Ref) {
			return fmt.Errorf("api-keys[%d].ref: invalid environment variable name", index)
		}
	default:
		return fmt.Errorf("api-keys[%d].source: unsupported credential source", index)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// validateGeminiOverride checks the Gemini route's override shape (spec 04
// §5). An override either names the path outright (endpoint, which the
// catalog pins to the native /v1/models/<model>:<action> shape) or builds it
// from the model and action templates, never both; the action template, when
// present, must be one of the two routable actions. Every accepted form keeps
// the request on the configured upstream authority — the catalog re-checks the
// joined URL before a record becomes routable.
func validateGeminiOverride(name string, o RouteOverride) error {
	if o.Endpoint != "" {
		if o.Model != "" || o.Action != "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be combined with model/action templates", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
		if strings.ContainsAny(o.Endpoint, "?#") {
			return fmt.Errorf("route-overrides[%s].endpoint: must not contain query or fragment separators", name)
		}
		return nil
	}
	if o.Model != "" && o.Model != GeminiModelPlaceholder && !validGeminiModelID(o.Model) {
		return fmt.Errorf("route-overrides[%s].model: invalid gemini model id %q", name, o.Model)
	}
	if o.Action != "" && !IsGeminiAction(o.Action) {
		return fmt.Errorf("route-overrides[%s].action: unsupported gemini action %q (use %s or %s)",
			name, o.Action, GeminiActionGenerate, GeminiActionStream)
	}
	return nil
}

// validGeminiModelID accepts the model segment of the native Gemini path:
// letters, digits, dot, underscore and hyphen only. Separators that would add
// a path segment ('/', ':'), a query or fragment ('?', '#'), an authority
// ('@'), or a percent escape ('%') are rejected, as are the dot-only
// traversal names, so a model ID cannot rewrite the upstream URL.
func validGeminiModelID(s string) bool {
	if s == "" || strings.Trim(s, ".") == "" {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// IsGeminiAction reports whether name is one of the two routable Gemini
// actions. The executor picks the effective action from the request's stream
// flag, so an override may only name one of these two; anything else would
// have to be routed by the adapter layer.
func IsGeminiAction(name string) bool {
	return name == GeminiActionGenerate || name == GeminiActionStream
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
