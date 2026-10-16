package catalog

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
)

// geminiBaseURL is the unified upstream base URL every gemini URL assertion
// below is measured against.
const geminiBaseURL = "https://opencode.ai/inference/go/v1"

// geminiCfg is testCfg with the unified base URL and the gemini protocol
// switch on, so gemini entries resolve the way production resolves them.
func geminiCfg() config.Config {
	cfg := testCfg()
	cfg.BaseURL = geminiBaseURL
	cfg.CatalogURL = geminiBaseURL + "/models"
	cfg.Protocols.Gemini = true
	return cfg
}

// catalogBody renders the upstream catalog envelope for the given IDs.
func catalogBody(ids ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q}`, id)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// refreshWith serves one canned catalog response through a fresh manager.
func refreshWith(t *testing.T, cfg config.Config, ids ...string) *Manager {
	t.Helper()
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(catalogBody(ids...))}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	return m
}

// TestGeminiRouteHasNoStaticEndpoint pins that the gemini route is registered
// under the config protocol name and carries no table path: every gemini URL
// is model-scoped.
func TestGeminiRouteHasNoStaticEndpoint(t *testing.T) {
	if RouteGemini != Route("gemini") {
		t.Errorf("RouteGemini = %q, want the protocols.gemini name", RouteGemini)
	}
	if got := RouteGemini.EndpointPath(); got != "" {
		t.Errorf("RouteGemini.EndpointPath() = %q, want no static path", got)
	}
	if got := RouteChatCompletions.EndpointPath(); got != "/v1/chat/completions" {
		t.Errorf("static routes must keep their table paths, got %q", got)
	}
	if r, ok := routeFromString("gemini"); !ok || r != RouteGemini {
		t.Errorf(`routeFromString("gemini") = %q %v, want the gemini route`, r, ok)
	}
	if _, ok := routeFromString("bogus"); ok {
		t.Error(`routeFromString("bogus") must not resolve`)
	}
}

// TestGeminiCatalogModelsRouteNatively pins the unified gemini handling: every
// model the catalog publishes under a gemini prefix is routed through the
// native /v1/models/<model>:generateContent path, stream or not, without a
// hardcoded allowlist of a single variant.
func TestGeminiCatalogModelsRouteNatively(t *testing.T) {
	ids := []string{"gemini-3-pro-preview", "gemini-3-flash", "gemini-2.5-flash", "GEMINI-3-PRO"}
	m := refreshWith(t, geminiCfg(), ids...)

	models := m.Models()
	if len(models) != len(ids) {
		t.Fatalf("routable = %+v, want all %d gemini variants", models, len(ids))
	}
	for _, mo := range models {
		if mo.Protocol != RouteGemini {
			t.Errorf("%s: Protocol = %q, want %q", mo.UpstreamID, mo.Protocol, RouteGemini)
		}
		wantEndpoint := "/v1/models/" + mo.UpstreamID + ":generateContent"
		if mo.EndpointPath != wantEndpoint {
			t.Errorf("%s: EndpointPath = %q, want %q", mo.UpstreamID, mo.EndpointPath, wantEndpoint)
		}
		wantStream := "/v1/models/" + mo.UpstreamID + ":streamGenerateContent"
		if got := mo.StreamEndpointPath(); got != wantStream {
			t.Errorf("%s: StreamEndpointPath = %q, want %q", mo.UpstreamID, got, wantStream)
		}
		if mo.PublicID != "opencode-go/"+mo.UpstreamID {
			t.Errorf("%s: PublicID = %q", mo.UpstreamID, mo.PublicID)
		}
		wantURL := geminiBaseURL + "/models/" + mo.UpstreamID + ":generateContent"
		if got := JoinUpstreamURL(geminiBaseURL, mo.EndpointPath); got != wantURL {
			t.Errorf("%s: URL = %q, want %q", mo.UpstreamID, got, wantURL)
		}
	}
	for _, id := range ids {
		if rec, ok := m.Lookup(id); !ok || rec.Protocol != RouteGemini {
			t.Errorf("Lookup(%q) = %+v %v", id, rec, ok)
		}
		if rec, ok := m.Lookup("opencode-go/" + id); !ok || rec.UpstreamID != id {
			t.Errorf("Lookup(prefixed %q) = %+v %v", id, rec, ok)
		}
	}
	if got := m.Unsupported(); len(got) != 0 {
		t.Fatalf("unsupported = %+v, want none", got)
	}
}

// TestGeminiProtocolFlagExcludesOnlyGeminiModels pins protocols.gemini as a
// kill switch: disabled, gemini entries leave the routable set with a
// diagnostic while the static routes keep serving.
func TestGeminiProtocolFlagExcludesOnlyGeminiModels(t *testing.T) {
	cfg := geminiCfg()
	cfg.Protocols.Gemini = false
	m := refreshWith(t, cfg, "gemini-3-pro-preview", "gemini-3-flash", "glm-5.2", "qwen3.7-max", "gpt-5.6-luna")

	models := m.Models()
	if len(models) != 3 {
		t.Fatalf("routable = %+v, want only the three static-route models", models)
	}
	for _, mo := range models {
		if mo.Protocol == RouteGemini {
			t.Errorf("gemini record still routable: %+v", mo)
		}
	}
	findModel(t, models, "glm-5.2")
	findModel(t, models, "qwen3.7-max")
	findModel(t, models, "gpt-5.6-luna")

	unsup := m.Unsupported()
	if len(unsup) != 2 {
		t.Fatalf("unsupported = %+v, want both gemini entries excluded", unsup)
	}
	for _, u := range unsup {
		if u.Reason != "protocol gemini disabled by config" {
			t.Errorf("%s: reason = %q", u.UpstreamID, u.Reason)
		}
	}
	if _, ok := m.Lookup("gemini-3-pro-preview"); ok {
		t.Error("disabled-protocol gemini entry must not resolve through Lookup")
	}
}

// TestLoadedConfigGeminiSwitchDrivesCatalog pins the two packages together: a
// YAML document with protocols.gemini disabled demotes gemini entries on the
// next snapshot.
func TestLoadedConfigGeminiSwitchDrivesCatalog(t *testing.T) {
	cfg, err := config.Load([]byte("base-url: " + geminiBaseURL + "\nprotocols:\n  gemini: false\napi-keys:\n  - value: sk-test\n"))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}
	m := refreshWith(t, cfg, "gemini-3-flash", "glm-5.2")
	if got := findModel(t, m.Models(), "glm-5.2").Protocol; got != RouteChatCompletions {
		t.Errorf("glm-5.2 Protocol = %q", got)
	}
	unsup := m.Unsupported()
	if len(unsup) != 1 || unsup[0].UpstreamID != "gemini-3-flash" || unsup[0].Reason != "protocol gemini disabled by config" {
		t.Fatalf("unsupported = %+v, want gemini-3-flash demoted", unsup)
	}
}

// TestGeminiOverrideTemplates pins the gemini override surface end to end: the
// model placeholder, a pinned literal model, an action template, defaults, and
// an explicit native endpoint all produce the pinned path shape, and every
// applied override stays visible in diagnostics (spec 04 §5).
func TestGeminiOverrideTemplates(t *testing.T) {
	cfg := geminiCfg()
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"gemini-3-pro-preview": {Protocol: "gemini", Model: config.GeminiModelPlaceholder, Action: "generateContent"},
		"gemini-alias":         {Protocol: "gemini", Model: "gemini-3-pro"},
		"gemini-action-only":   {Protocol: "gemini", Action: "generateContent"},
		"gemini-defaults":      {Protocol: "gemini"},
		"gemini-pinned":        {Protocol: "gemini", Endpoint: "/v1/models/gemini-2.5-flash:generateContent"},
	}
	ids := []string{"gemini-3-pro-preview", "gemini-alias", "gemini-action-only", "gemini-defaults", "gemini-pinned"}
	m := refreshWith(t, cfg, ids...)
	if got := m.Unsupported(); len(got) != 0 {
		t.Fatalf("unsupported = %+v, want none", got)
	}

	want := map[string]string{
		"gemini-3-pro-preview": "/v1/models/gemini-3-pro-preview:generateContent",
		"gemini-alias":         "/v1/models/gemini-3-pro:generateContent",
		"gemini-action-only":   "/v1/models/gemini-action-only:generateContent",
		"gemini-defaults":      "/v1/models/gemini-defaults:generateContent",
		"gemini-pinned":        "/v1/models/gemini-2.5-flash:generateContent",
	}
	for id, wantPath := range want {
		rec := findModel(t, m.Models(), id)
		if rec.Protocol != RouteGemini || rec.EndpointPath != wantPath {
			t.Errorf("%s: %+v, want endpoint %q", id, rec, wantPath)
		}
	}
	warns := strings.Join(m.Warnings(), "\n")
	for id := range want {
		if !strings.Contains(warns, fmt.Sprintf("model %q routed via user override (gemini", id)) {
			t.Errorf("override for %q missing from warnings: %v", id, m.Warnings())
		}
	}
	// The alias pins a different upstream model, so its exact URL must carry
	// that model rather than the public ID.
	alias := findModel(t, m.Models(), "gemini-alias")
	if got := JoinUpstreamURL(cfg.BaseURL, alias.EndpointPath); got != geminiBaseURL+"/models/gemini-3-pro:generateContent" {
		t.Errorf("alias URL = %q", got)
	}
}

// TestGeminiOverrideEndpointsAreValidated pins the two gates an explicitly
// pinned gemini endpoint passes: it must stay on the configured upstream
// authority, and it must have the native path shape the executor switches
// actions on.
func TestGeminiOverrideEndpointsAreValidated(t *testing.T) {
	t.Run("authority rewrite excluded", func(t *testing.T) {
		cfg := geminiCfg()
		cfg.BaseURL = "https://opencode.ai"
		cfg.RouteOverrides = map[string]config.RouteOverride{
			"gemini-evil": {Protocol: "gemini", Endpoint: "/v1@evil.com/x"},
		}
		m := refreshWith(t, cfg, "gemini-evil")
		if got := m.Models(); len(got) != 0 {
			t.Fatalf("escaping override still routable: %+v", got)
		}
		unsup := m.Unsupported()
		if len(unsup) != 1 || unsup[0].Reason != "resolved endpoint escapes the configured upstream host" {
			t.Fatalf("unsupported = %+v, want the authority-escape reason", unsup)
		}
	})

	t.Run("non-native shape excluded", func(t *testing.T) {
		cfg := geminiCfg()
		cfg.RouteOverrides = map[string]config.RouteOverride{
			"gemini-wrong-shape": {Protocol: "gemini", Endpoint: "/v1/responses"},
		}
		m := refreshWith(t, cfg, "gemini-wrong-shape")
		if got := m.Models(); len(got) != 0 {
			t.Fatalf("non-native gemini endpoint still routable: %+v", got)
		}
		unsup := m.Unsupported()
		if len(unsup) != 1 || unsup[0].Reason != geminiPathReason {
			t.Fatalf("unsupported = %+v, want %q", unsup, geminiPathReason)
		}
	})

	t.Run("unsupported actions excluded", func(t *testing.T) {
		for _, endpoint := range []string{"/v1/models/gemini-3-pro:countTokens", "/v1/models/gemini-3-pro:interactions", "/v1/models/gemini-3-pro:generatecontent"} {
			cfg := geminiCfg()
			cfg.RouteOverrides = map[string]config.RouteOverride{
				"gemini-pinned": {Protocol: "gemini", Endpoint: endpoint},
			}
			m := refreshWith(t, cfg, "gemini-pinned")
			if got := m.Models(); len(got) != 0 {
				t.Errorf("%s: still routable: %+v", endpoint, got)
			}
			unsup := m.Unsupported()
			if len(unsup) != 1 || unsup[0].Reason != geminiPathReason {
				t.Errorf("%s: unsupported = %+v, want %q", endpoint, unsup, geminiPathReason)
			}
		}
	})

	t.Run("pinned stream action stays routable", func(t *testing.T) {
		cfg := geminiCfg()
		cfg.RouteOverrides = map[string]config.RouteOverride{
			"gemini-pinned": {Protocol: "gemini", Endpoint: "/v1/models/gemini-3-pro:streamGenerateContent"},
		}
		m := refreshWith(t, cfg, "gemini-pinned")
		rec := findModel(t, m.Models(), "gemini-pinned")
		if rec.EndpointPath != "/v1/models/gemini-3-pro:streamGenerateContent" {
			t.Errorf("EndpointPath = %q", rec.EndpointPath)
		}
	})
	t.Run("native endpoint stays routable", func(t *testing.T) {
		cfg := geminiCfg()
		cfg.RouteOverrides = map[string]config.RouteOverride{
			"gemini-pinned": {Protocol: "gemini", Endpoint: "/v1/models/gemini-3-pro-preview:generateContent"},
		}
		m := refreshWith(t, cfg, "gemini-pinned")
		rec := findModel(t, m.Models(), "gemini-pinned")
		if rec.EndpointPath != "/v1/models/gemini-3-pro-preview:generateContent" {
			t.Errorf("EndpointPath = %q", rec.EndpointPath)
		}
	})
}

// TestGeminiModelIDsCannotInjectPathOrQuery pins the security boundary: a
// catalog ID that is not a plain model segment cannot turn into a routable
// gemini URL, so no entry can add a path segment, a query, or a fragment to
// the request the executor sends.
func TestGeminiModelIDsCannotInjectPathOrQuery(t *testing.T) {
	hostile := []string{
		"gemini-3-pro/extra",
		"gemini?alt=sse",
		"gemini#frag",
		"gemini@evil.com",
		"gemini%2F..%2Fx",
		"gemini-3-pro:",
		"gemini:countTokens",
	}
	m := refreshWith(t, geminiCfg(), append(append([]string{}, hostile...), "gemini-3-pro-preview")...)

	models := m.Models()
	if len(models) != 1 || models[0].UpstreamID != "gemini-3-pro-preview" {
		t.Fatalf("routable = %+v, want only the safe gemini model", models)
	}
	unsup := m.Unsupported()
	if len(unsup) != len(hostile) {
		t.Fatalf("unsupported = %+v, want %d excluded entries", unsup, len(hostile))
	}
	for _, u := range unsup {
		if u.Reason != geminiPathReason {
			t.Errorf("%s: reason = %q, want %q", u.UpstreamID, u.Reason, geminiPathReason)
		}
	}
	for _, id := range hostile {
		if _, ok := m.Lookup(id); ok {
			t.Errorf("%q resolved although it cannot form a safe gemini path", id)
		}
	}
	for _, mo := range models {
		for _, endpoint := range []string{mo.EndpointPath, mo.StreamEndpointPath()} {
			joined, err := url.Parse(JoinUpstreamURL(geminiBaseURL, endpoint))
			if err != nil || joined.Scheme != "https" || joined.Host != "opencode.ai" ||
				joined.RawQuery != "" || joined.Fragment != "" || joined.User != nil {
				t.Errorf("%s: joined endpoint %q parses to %+v (err %v)", mo.UpstreamID, endpoint, joined, err)
			}
		}
	}
}

// TestGeminiStreamEndpointPath pins the action switch the executor relies on:
// only gemini records change path for streaming, and only the native action
// token is rewritten.
func TestGeminiStreamEndpointPath(t *testing.T) {
	cases := []struct {
		name string
		rec  ModelRecord
		want string
	}{
		{
			"static route serves one path",
			ModelRecord{Protocol: RouteChatCompletions, EndpointPath: "/v1/chat/completions"},
			"/v1/chat/completions",
		},
		{
			"gemini switches to streamGenerateContent",
			ModelRecord{Protocol: RouteGemini, EndpointPath: "/v1/models/gemini-3-pro:generateContent"},
			"/v1/models/gemini-3-pro:streamGenerateContent",
		},
		{
			"pinned stream action is already the stream action",
			ModelRecord{Protocol: RouteGemini, EndpointPath: "/v1/models/gemini-3-pro:streamGenerateContent"},
			"/v1/models/gemini-3-pro:streamGenerateContent",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.rec.StreamEndpointPath()
			if got != c.want {
				t.Errorf("StreamEndpointPath() = %q, want %q", got, c.want)
			}
			// The executor strips a known action suffix and appends the request's
			// action, so a doubled suffix here would produce a doubled upstream URL.
			if n := strings.Count(got, "GenerateContent"); n > 1 {
				t.Errorf("StreamEndpointPath() = %q, carries %d action tokens", got, n)
			}
			if strings.Contains(got, "GenerateContent:") {
				t.Errorf("StreamEndpointPath() = %q, action suffix is doubled", got)
			}
		})
	}
}

// TestGeminiExactUpstreamURL pins the byte-exact URLs the unified base URL
// produces, including the trailing-slash base and the stream action.
func TestGeminiExactUpstreamURL(t *testing.T) {
	cases := []struct {
		base     string
		endpoint string
		want     string
	}{
		{
			geminiBaseURL,
			"/v1/models/gemini-3-pro-preview:generateContent",
			"https://opencode.ai/inference/go/v1/models/gemini-3-pro-preview:generateContent",
		},
		{
			geminiBaseURL + "/",
			"/v1/models/gemini-3-pro-preview:generateContent",
			"https://opencode.ai/inference/go/v1/models/gemini-3-pro-preview:generateContent",
		},
		{
			geminiBaseURL,
			"/v1/models/gemini-3-pro-preview:streamGenerateContent",
			"https://opencode.ai/inference/go/v1/models/gemini-3-pro-preview:streamGenerateContent",
		},
		{
			geminiBaseURL,
			"/v1/models/gemini-2.5-flash:generateContent",
			"https://opencode.ai/inference/go/v1/models/gemini-2.5-flash:generateContent",
		},
		{
			config.DefaultBaseURL,
			"/v1/models/gemini-3-flash:generateContent",
			"https://opencode.ai/inference/go/v1/models/gemini-3-flash:generateContent",
		},
	}
	for _, c := range cases {
		if got := JoinUpstreamURL(c.base, c.endpoint); got != c.want {
			t.Errorf("JoinUpstreamURL(%q, %q) = %q, want %q", c.base, c.endpoint, got, c.want)
		}
	}
	if config.DefaultBaseURL != geminiBaseURL {
		t.Errorf("DefaultBaseURL = %q, want %q", config.DefaultBaseURL, geminiBaseURL)
	}
}

// TestSeedFromAppliesGeminiKillSwitch pins that a gemini kill switch flipped
// between snapshots demotes carried-over records at seed time, the same way
// the static protocols do.
func TestSeedFromAppliesGeminiKillSwitch(t *testing.T) {
	prev := refreshWith(t, geminiCfg(), "gemini-3-pro-preview", "glm-5.2")
	findModel(t, prev.Models(), "gemini-3-pro-preview") // precondition

	cfg := geminiCfg()
	cfg.Protocols.Gemini = false
	m := newManager(cfg, &fakeClient{})
	m.SeedFrom(prev)

	for _, mo := range m.Models() {
		if mo.Protocol == RouteGemini {
			t.Fatalf("gemini record still routable after seed: %+v", mo)
		}
	}
	findModel(t, m.Models(), "glm-5.2")
	if _, ok := m.Lookup("gemini-3-pro-preview"); ok {
		t.Error("demoted gemini record must not resolve through Lookup")
	}
	var found bool
	for _, u := range m.Unsupported() {
		if u.UpstreamID == "gemini-3-pro-preview" && u.Reason == "protocol gemini disabled by config" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unsupported = %+v, want the gemini demotion diagnostic", m.Unsupported())
	}
	findModel(t, prev.Models(), "gemini-3-pro-preview") // seeding must not mutate prev
}

// TestGeminiEndpointPatternAcceptsOnlyNativeActions pins the endpoint guard to
// the two routable actions: the executor swaps the suffix itself, so an
// override may not introduce any other action (countTokens, interactions, or a
// later PCS action), and no other path shape may become a gemini endpoint.
func TestGeminiEndpointPatternAcceptsOnlyNativeActions(t *testing.T) {
	for _, action := range []string{config.GeminiActionGenerate, config.GeminiActionStream} {
		if !validGeminiEndpoint("/v1/models/gemini-3-pro:" + action) {
			t.Errorf("validGeminiEndpoint(%q) = false, want true", action)
		}
	}
	for _, action := range []string{
		"", "countTokens", "interactions", "Interactions", "generatecontent",
		"streamgeneratecontent", "generateContent2", "generateContent:streamGenerateContent",
	} {
		if validGeminiEndpoint("/v1/models/gemini-3-pro:" + action) {
			t.Errorf("validGeminiEndpoint(action %q) = true, want false", action)
		}
	}
	for _, path := range []string{
		"/v1/models/gemini-3-pro",
		"/v1/models/:generateContent",
		"/v1/models/..:generateContent",
		"/v1/models/gemini-3-pro/:generateContent",
		"/v1/models/gemini-3-pro:generateContent?alt=sse",
		"/v1/models/gemini-3-pro:generateContent#alt",
		"/v1/chat/completions",
		"v1/models/gemini-3-pro:generateContent",
	} {
		if validGeminiEndpoint(path) {
			t.Errorf("validGeminiEndpoint(%q) = true, want false", path)
		}
	}
}
