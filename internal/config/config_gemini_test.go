package config

import "testing"

// TestLoadGeminiProtocolDefaults pins the protocols.gemini switch: like the
// three existing flags it defaults to enabled, is disabled only by an explicit
// false, and stays independent of the other protocol flags (spec 04 §4).
func TestLoadGeminiProtocolDefaults(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{"unset defaults to enabled", withKey, true},
		{"explicitly enabled", "protocols:\n  gemini: true\n" + withKey, true},
		{"explicitly disabled", "protocols:\n  gemini: false\n" + withKey, false},
		{"disabled beside other flags", "protocols:\n  gemini: false\n  responses: false\n" + withKey, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if c.Protocols.Gemini != tt.want {
				t.Errorf("Protocols.Gemini = %v, want %v", c.Protocols.Gemini, tt.want)
			}
		})
	}
}

// TestLoadGeminiKillSwitchKeepsStaticRoutes: disabling gemini must not disturb
// the pre-existing chat-completions/messages/responses flags.
func TestLoadGeminiKillSwitchKeepsStaticRoutes(t *testing.T) {
	c, err := Load([]byte("protocols:\n  gemini: false\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !c.Protocols.ChatCompletions || !c.Protocols.Messages || !c.Protocols.Responses {
		t.Errorf("Protocols = %+v, want the three static routes still enabled", c.Protocols)
	}
	if c.Protocols.Gemini {
		t.Errorf("Protocols.Gemini = true, want false")
	}
}

// TestLoadUnifiedBaseURLAndStableCatalogDerivation pins the unified upstream
// base URL, the catalog URL derived from it (including the trailing-slash
// trim), and the fact that an explicit base-url still overrides both.
func TestLoadUnifiedBaseURLAndStableCatalogDerivation(t *testing.T) {
	const wantBase = "https://opencode.ai/inference/go/v1"
	if DefaultBaseURL != wantBase {
		t.Fatalf("DefaultBaseURL = %q, want %q", DefaultBaseURL, wantBase)
	}
	tests := []struct {
		name        string
		yaml        string
		wantBase    string
		wantCatalog string
	}{
		{"minimal", withKey, wantBase, wantBase + "/models"},
		{"default base with trailing slash", "base-url: " + wantBase + "/\n" + withKey, wantBase + "/", wantBase + "/models"},
		{
			"explicit base still wins",
			"base-url: https://mirror.example.com/inference/go/v1\n" + withKey,
			"https://mirror.example.com/inference/go/v1",
			"https://mirror.example.com/inference/go/v1/models",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if c.BaseURL != tt.wantBase {
				t.Errorf("BaseURL = %q, want %q", c.BaseURL, tt.wantBase)
			}
			if c.CatalogURL != tt.wantCatalog {
				t.Errorf("CatalogURL = %q, want %q", c.CatalogURL, tt.wantCatalog)
			}
		})
	}
}

// TestLoadGeminiOverrideTemplates pins the gemini-only override surface
// (spec 04 §5): a model template (placeholder or pinned literal), an action
// template, both omitted, or an explicit native endpoint. Static-route
// overrides keep loading exactly as before.
func TestLoadGeminiOverrideTemplates(t *testing.T) {
	doc := `
route-overrides:
  gemini-3-pro-preview:
    protocol: gemini
    model: "{model}"
    action: generateContent
  gemini-pinned-alias:
    protocol: gemini
    model: gemini-3-pro
  gemini-action-only:
    protocol: gemini
    action: streamGenerateContent
  gemini-defaults:
    protocol: gemini
  gemini-endpoint:
    protocol: gemini
    endpoint: "/v1/models/gemini-2.5-flash:generateContent"
  gemini-endpoint-stream:
    protocol: gemini
    endpoint: "/v1/models/gemini-3-pro:streamGenerateContent"
  glm-5.2:
    protocol: chat-completions
    endpoint: /v1/chat/completions
` + withKey
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !c.Protocols.ChatCompletions || !c.Protocols.Messages || !c.Protocols.Responses || !c.Protocols.Gemini {
		t.Errorf("Protocols = %+v, want every flag still defaulted on", c.Protocols)
	}
	want := map[string]RouteOverride{
		"gemini-3-pro-preview":   {Protocol: ProtocolGemini, Model: GeminiModelPlaceholder, Action: "generateContent"},
		"gemini-pinned-alias":    {Protocol: ProtocolGemini, Model: "gemini-3-pro"},
		"gemini-action-only":     {Protocol: ProtocolGemini, Action: "streamGenerateContent"},
		"gemini-defaults":        {Protocol: ProtocolGemini},
		"gemini-endpoint":        {Protocol: ProtocolGemini, Endpoint: "/v1/models/gemini-2.5-flash:generateContent"},
		"gemini-endpoint-stream": {Protocol: ProtocolGemini, Endpoint: "/v1/models/gemini-3-pro:streamGenerateContent"},
		"glm-5.2":                {Protocol: "chat-completions", Endpoint: "/v1/chat/completions"},
	}
	if len(c.RouteOverrides) != len(want) {
		t.Fatalf("RouteOverrides = %+v, want %d entries", c.RouteOverrides, len(want))
	}
	for name, w := range want {
		if got := c.RouteOverrides[name]; got != w {
			t.Errorf("RouteOverrides[%q] = %+v, want %+v", name, got, w)
		}
	}
}

// TestLoadGeminiOverrideRejections pins the validation floor for gemini
// overrides: names and templates that could add a path segment, a query, a
// fragment, or a different authority are refused at load, and the gemini-only
// templates are refused on the static routes.
func TestLoadGeminiOverrideRejections(t *testing.T) {
	const g = "route-overrides:\n  gx:\n    protocol: gemini\n"

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			"endpoint combined with templates",
			g + "    endpoint: \"/v1/models/gemini-3-pro:generateContent\"\n    model: \"{model}\"\n" + withKey,
			"route-overrides[gx].endpoint: must not be combined with model/action templates",
		},
		{
			"endpoint with query",
			g + "    endpoint: \"/v1/models/gemini-3-pro:generateContent?alt=sse\"\n" + withKey,
			"route-overrides[gx].endpoint: must not contain query or fragment separators",
		},
		{
			"endpoint with fragment",
			g + "    endpoint: \"/v1/models/gemini-3-pro:generateContent#alt\"\n" + withKey,
			"route-overrides[gx].endpoint: must not contain query or fragment separators",
		},
		{
			"endpoint without leading slash",
			g + "    endpoint: \"v1/models/gemini-3-pro:generateContent\"\n" + withKey,
			"route-overrides[gx].endpoint: must start with /",
		},
		{
			"model with path separator",
			g + "    model: \"gemini/3-pro\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"model with dot-only traversal",
			g + "    model: \"..\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"model with query separator",
			g + "    model: \"gemini?alt=sse\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"model with authority separator",
			g + "    model: \"gemini@evil.com\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"model with action separator",
			g + "    model: \"gemini:3\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"model with percent escape",
			g + "    model: \"%2e%2e\"\n" + withKey,
			"route-overrides[gx].model: invalid gemini model id",
		},
		{
			"action with separator",
			g + "    action: \"generate:Content\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action starting with a digit",
			g + "    action: \"1generate\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action with space",
			g + "    action: \"generate Content\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action countTokens rejected",
			g + "    action: \"countTokens\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action interactions rejected",
			g + "    action: \"interactions\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action vocabulary is case-sensitive",
			g + "    action: \"generatecontent\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		},
		{
			"action with stream suffix typo rejected",
			g + "    action: \"streamGenerateContentX\"\n" + withKey,
			"route-overrides[gx].action: unsupported gemini action",
		}, {
			"static route rejects model template",
			"route-overrides:\n  gx:\n    protocol: responses\n    endpoint: /v1/responses\n    model: \"{model}\"\n" + withKey,
			`route-overrides[gx].model: only supported with protocol "gemini"`,
		},
		{
			"static route rejects action template",
			"route-overrides:\n  gx:\n    protocol: chat-completions\n    endpoint: /v1/chat/completions\n    action: generateContent\n" + withKey,
			`route-overrides[gx].action: only supported with protocol "gemini"`,
		},
		{
			"unknown protocol name",
			"route-overrides:\n  gx:\n    protocol: gemini-native\n    endpoint: /v1/x\n" + withKey,
			"route-overrides[gx].protocol: unsupported protocol",
		},
		{
			"protocol name is case-sensitive",
			"route-overrides:\n  gx:\n    protocol: GEMINI\n    endpoint: \"/v1/models/gemini-3-pro:generateContent\"\n" + withKey,
			"route-overrides[gx].protocol: unsupported protocol",
		},
		{
			"static route still requires an endpoint",
			"route-overrides:\n  gx:\n    protocol: responses\n    endpoint: \"\"\n" + withKey,
			"route-overrides[gx].endpoint: must not be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestLoadGeminiOverrideEmptyEndpointAllowed pins that protocol: gemini needs
// no endpoint at all — the catalog builds the native path — while a static
// route still does.
func TestLoadGeminiOverrideEmptyEndpointAllowed(t *testing.T) {
	c, err := Load([]byte("route-overrides:\n  gemini-3-pro-preview:\n    protocol: gemini\n" + withKey))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := c.RouteOverrides["gemini-3-pro-preview"]; got.Protocol != ProtocolGemini || got.Endpoint != "" {
		t.Errorf("RouteOverrides = %+v, want a gemini override without an endpoint", got)
	}
}

// TestIsGeminiActionVocabulary pins the action vocabulary to the two native
// actions: the executor chooses between them from the request's stream flag,
// so nothing else (countTokens, interactions, or a case variant) may be
// selected through configuration.
func TestIsGeminiActionVocabulary(t *testing.T) {
	if GeminiActionGenerate != "generateContent" || GeminiActionStream != "streamGenerateContent" {
		t.Fatalf("actions = %q/%q, want the native names", GeminiActionGenerate, GeminiActionStream)
	}
	for _, ok := range []string{GeminiActionGenerate, GeminiActionStream} {
		if !IsGeminiAction(ok) {
			t.Errorf("IsGeminiAction(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "countTokens", "interactions", "Interactions", "GenerateContent",
		"streamgeneratecontent", "generateContent2", "generateContent:streamGenerateContent"} {
		if IsGeminiAction(bad) {
			t.Errorf("IsGeminiAction(%q) = true, want false", bad)
		}
	}
}
