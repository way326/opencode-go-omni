package catalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// currentCatalogModel is one ID from the current OpenCode Go catalog with the
// route its upstream protocol metadata documents.
//
// Evidence: the models.dev opencode-go snapshot (work/models-dev-api.json)
// declares provider default npm @ai-sdk/openai-compatible (Chat Completions)
// and overrides each model through provider.npm — @ai-sdk/google -> Gemini,
// @ai-sdk/anthropic -> Messages, @ai-sdk/openai -> Responses, absent ->
// inherit the Chat default. The Go model list in work/official-go-doc.mdx
// agrees with that snapshot on every documented name. omen-alpha is the one ID
// the snapshot does not contain at all; it is included here as the exact,
// separately-justified compatibility entry (see omenAlphaNote) and not as a
// family prefix.
// currentCatalogModels is the live OpenCode Go catalog as returned by a
// read-only GET of /inference/go/v1/models: 40 model IDs, each paired with the
// route its upstream protocol metadata documents.
//
// Evidence: the live GET is the primary source for membership. The models.dev
// opencode-go snapshot (work/models-dev-api.json) supplies the protocol
// evidence and covers 36 of the 40 IDs: provider default npm
// @ai-sdk/openai-compatible is Chat Completions, and a per-model provider.npm
// override selects @ai-sdk/google -> Gemini, @ai-sdk/anthropic -> Messages, or
// @ai-sdk/openai -> Responses. The snapshot also lists grok-4.5 and
// space-bunny-free, which the live catalog does not serve; those two are
// deliberately kept out of this fixture.
//
// Three IDs carry no protocol metadata: glm-5.1, minimax-m2.5 and
// deepseek-flash inherit the plugin's documented family compatibility
// (glm/deepseek -> Chat Completions, minimax -> Messages). omen-alpha is the
// single entry whose upstream behavior is not yet confirmed and is routed
// through the exact compatibility table with a pending-acceptance diagnostic
// (see omenAlphaNote).
var currentCatalogModels = []struct {
	id    string
	route Route
}{
	{"deepseek-v4-flash", RouteChatCompletions},
	{"deepseek-v4-flash-vision-exp", RouteChatCompletions},
	{"deepseek-flash", RouteChatCompletions},
	{"deepseek-v4.1-flash", RouteChatCompletions},
	{"deepseek-v4-pro", RouteChatCompletions},
	{"glm-5.1", RouteChatCompletions},
	{"glm-5.2", RouteChatCompletions},
	{"glm-5.3", RouteChatCompletions},
	{"grok-4.6", RouteResponses},
	{"grok-4.7", RouteResponses},
	{"muse-spark-1.2-contributor", RouteResponses},
	{"muse-spark-1.3-contributor", RouteResponses},
	{"glm-5.3-flash", RouteChatCompletions},
	{"omen-alpha", RouteChatCompletions},
	{"gpt-5.6-luna", RouteResponses},
	{"gpt-6-luna", RouteResponses},
	{"claude-haiku-5-5", RouteMessages},
	{"hy3", RouteChatCompletions},
	{"hy4-preview", RouteChatCompletions},
	{"kimi-k2.6", RouteChatCompletions},
	{"kimi-k2.7-code", RouteChatCompletions},
	{"kimi-k3", RouteChatCompletions},
	{"mimo-v2.5", RouteChatCompletions},
	{"mimo-v2.6-flash", RouteChatCompletions},
	{"mimo-v2.5-pro", RouteChatCompletions},
	{"mimo-v2.6-pro", RouteChatCompletions},
	{"minimax-m2.5", RouteMessages},
	{"minimax-m2.7", RouteMessages},
	{"gemini-4-argon-contributor", RouteGemini},
	{"minimax-m3", RouteMessages},
	{"space-bunny", RouteChatCompletions},
	{"longcat-2.0", RouteChatCompletions},
	{"longcat-2.5-preview-free", RouteChatCompletions},
	{"step-5-preview-free", RouteChatCompletions},
	{"cat-jam-free", RouteChatCompletions},
	{"qwen3.6-plus", RouteChatCompletions},
	{"qwen3.7-max", RouteChatCompletions},
	{"qwen3.8-max", RouteMessages},
	{"qwen3.8-flash", RouteMessages},
	{"qwen3.7-plus", RouteMessages},
}

// currentCatalogIDs returns the fixture IDs in declaration order.
func currentCatalogIDs() []string {
	ids := make([]string, 0, len(currentCatalogModels))
	for _, m := range currentCatalogModels {
		ids = append(ids, m.id)
	}
	return ids
}

// currentCatalogManager serves the fixture catalog through a manager built
// from cfg.
func currentCatalogManager(t *testing.T, cfg config.Config) *Manager {
	t.Helper()
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(catalogBody(currentCatalogIDs()...))}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	return m
}

// wantEndpoint is the endpoint path a fixture model must publish: the static
// table path for the three static routes, the native model-scoped path for
// Gemini.
func wantEndpoint(id string, route Route) string {
	if route == RouteGemini {
		return "/v1/models/" + id + ":generateContent"
	}
	return route.EndpointPath()
}

// TestCurrentCatalogKnownModelsRouteAsDocumented pins every model of the live
// 40-model catalog to the protocol its upstream metadata documents, so the
// catalog's dynamic discovery cannot silently mis-route a model the way the
// qwen family prefix did for qwen3.6-plus and qwen3.7-max.
func TestCurrentCatalogKnownModelsRouteAsDocumented(t *testing.T) {
	m := currentCatalogManager(t, testCfg())

	models := m.Models()
	if len(models) != 40 {
		t.Fatalf("routable = %d models, want the 40 live catalog IDs", len(models))
	}
	if len(currentCatalogModels) != 40 {
		t.Fatalf("fixture lists %d IDs, want 40", len(currentCatalogModels))
	}
	for _, want := range currentCatalogModels {
		got := findModel(t, models, want.id)
		if got.Protocol != want.route {
			t.Errorf("%s: Protocol = %q, want %q", want.id, got.Protocol, want.route)
		}
		if wantPath := wantEndpoint(want.id, want.route); got.EndpointPath != wantPath {
			t.Errorf("%s: EndpointPath = %q, want %q", want.id, got.EndpointPath, wantPath)
		}
		if got.PublicID != "opencode-go/"+want.id {
			t.Errorf("%s: PublicID = %q", want.id, got.PublicID)
		}
		if rec, ok := m.Lookup("opencode-go/" + want.id); !ok || rec.UpstreamID != want.id {
			t.Errorf("Lookup(%q) = %+v %v", want.id, rec, ok)
		}
	}
	if unsup := m.Unsupported(); len(unsup) != 0 {
		t.Fatalf("unsupported = %+v, want none of the known models excluded", unsup)
	}
	// omen-alpha is the only entry that carries a diagnostic.
	warns := m.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], "omen-alpha") {
		t.Fatalf("warnings = %v, want only the omen-alpha compatibility note", warns)
	}
}

// TestCurrentCatalogProtocolFamiliesCoverEveryRoute pins that the fixture
// spans all four routes, so the assertions above cannot pass by leaving a
// protocol empty.
func TestCurrentCatalogProtocolFamiliesCoverEveryRoute(t *testing.T) {
	counts := map[Route]int{}
	for _, m := range currentCatalogModels {
		counts[m.route]++
	}
	for _, r := range []Route{RouteChatCompletions, RouteMessages, RouteResponses, RouteGemini} {
		if counts[r] == 0 {
			t.Errorf("fixture has no %q model", r)
		}
	}
	if got := counts[RouteGemini]; got != 1 {
		t.Errorf("gemini models = %d, want the one listed contributor model", got)
	}
}

// TestCurrentCatalogRoutePriority pins the three-tier resolution order
// (arch §5): a user override beats the exact compatibility table, which beats
// the family prefix, and an ID no tier claims stays unsupported.
func TestCurrentCatalogRoutePriority(t *testing.T) {
	cfg := testCfg()
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"qwen3.7-max": {Protocol: "messages", Endpoint: "/v1/messages"},
		"omen-alpha":  {Protocol: "responses", Endpoint: "/v1/responses"},
		"space-bunny": {Protocol: "gemini", Model: config.GeminiModelPlaceholder},
	}
	fc := &fakeClient{resp: pluginapi.HTTPResponse{
		StatusCode: 200,
		Body: []byte(catalogBody(
			"qwen3.7-max",  // override wins over the exact table
			"qwen3.6-plus", // exact table wins over the qwen prefix
			"omen-alpha",   // override wins over the exact fallback entry
			"space-bunny",  // override wins over the space-bunny prefix
			"qwen3.7-plus", // prefix still routes the rest of the family
			"claude-haiku-5-5",
			"step-5-preview-free",
			"cat-jam-free",
			"mystery-model", // no tier claims it
			"omen-unknown",  // proves no bare "omen" prefix was added
		)),
	}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	cases := []struct {
		id       string
		route    Route
		endpoint string
	}{
		{"qwen3.7-max", RouteMessages, "/v1/messages"},
		{"qwen3.6-plus", RouteChatCompletions, "/v1/chat/completions"},
		{"omen-alpha", RouteResponses, "/v1/responses"},
		{"space-bunny", RouteGemini, "/v1/models/space-bunny:generateContent"},
		{"qwen3.7-plus", RouteMessages, "/v1/messages"},
		{"claude-haiku-5-5", RouteMessages, "/v1/messages"},
		{"step-5-preview-free", RouteChatCompletions, "/v1/chat/completions"},
		{"cat-jam-free", RouteChatCompletions, "/v1/chat/completions"},
	}
	models := m.Models()
	if len(models) != len(cases) {
		t.Fatalf("routable = %+v, want %d models", models, len(cases))
	}
	for _, c := range cases {
		got := findModel(t, models, c.id)
		if got.Protocol != c.route || got.EndpointPath != c.endpoint {
			t.Errorf("%s: %+v, want %q %q", c.id, got, c.route, c.endpoint)
		}
	}
	for _, id := range []string{"mystery-model", "omen-unknown"} {
		if _, ok := m.Lookup(id); ok {
			t.Errorf("%q resolved although no override, exact entry, or prefix claims it", id)
		}
	}
	unsup := m.Unsupported()
	if len(unsup) != 2 {
		t.Fatalf("unsupported = %+v, want both unknown IDs excluded", unsup)
	}
	for _, u := range unsup {
		if u.Reason != "no route determined" {
			t.Errorf("%s: reason = %q, want the unsupported diagnostic", u.UpstreamID, u.Reason)
		}
	}
}

// TestCompatPrefixFamiliesRouteVariants pins the four families added
// from the snapshot: each documented ID and an unknown variant of the same
// family resolve through the prefix table.
func TestCompatPrefixFamiliesRouteVariants(t *testing.T) {
	families := []struct {
		id    string
		route Route
	}{
		{"claude-haiku-5-5", RouteMessages},
		{"claude-opus-9", RouteMessages},
		{"space-bunny", RouteChatCompletions},
		{"space-bunny-free", RouteChatCompletions},
		{"step-5-preview-free", RouteChatCompletions},
		{"step-9", RouteChatCompletions},
		{"cat-jam-free", RouteChatCompletions},
		{"cat-jam-2", RouteChatCompletions},
	}
	ids := make([]string, 0, len(families))
	for _, f := range families {
		ids = append(ids, f.id)
	}
	cfg := testCfg()
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(catalogBody(ids...))}}
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	models := m.Models()
	if len(models) != len(families) {
		t.Fatalf("routable = %d, want %d", len(models), len(families))
	}
	for _, f := range families {
		if got := findModel(t, models, f.id).Protocol; got != f.route {
			t.Errorf("%s: Protocol = %q, want %q", f.id, got, f.route)
		}
	}
	if unsup := m.Unsupported(); len(unsup) != 0 {
		t.Fatalf("unsupported = %+v, want none", unsup)
	}
	if warns := m.Warnings(); len(warns) != 0 {
		t.Fatalf("warnings = %v, want none for prefix-routed families", warns)
	}
}

// TestOmenAlphaCompatibilityFallbackDiagnostics pins the one unverified model:
// omen-alpha is routable as Chat Completions on compatibility grounds, and the
// snapshot carries a diagnostic stating that upstream stream acceptance is
// still pending. The diagnostic must not read as a claim that streaming works,
// and no other model may inherit it.
func TestOmenAlphaCompatibilityFallbackDiagnostics(t *testing.T) {
	m := currentCatalogManager(t, testCfg())

	rec := findModel(t, m.Models(), "omen-alpha")
	if rec.Protocol != RouteChatCompletions || rec.EndpointPath != "/v1/chat/completions" {
		t.Fatalf("omen-alpha = %+v, want the chat-completions endpoint", rec)
	}
	if rec.DisplayName != "omen-alpha" {
		t.Errorf("DisplayName = %q, want the upstream id fallback", rec.DisplayName)
	}

	var notes []string
	for _, w := range m.Warnings() {
		if strings.Contains(w, "omen-alpha") {
			notes = append(notes, w)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("omen-alpha diagnostics = %v, want exactly one note", notes)
	}
	note := notes[0]
	for _, want := range []string{"compatibility fallback", "upstream stream acceptance pending", "chat-completions"} {
		if !strings.Contains(note, want) {
			t.Errorf("diagnostic %q missing %q", note, want)
		}
	}
	// Guard against the note drifting into an unverified success claim.
	for _, forbidden := range []string{"verified", "confirmed", "streaming works", "succeeded"} {
		if strings.Contains(strings.ToLower(note), forbidden) {
			t.Errorf("diagnostic %q claims %q, which the probe did not establish", note, forbidden)
		}
	}
	if note != omenAlphaNote {
		t.Errorf("diagnostic = %q, want the pinned omenAlphaNote", note)
	}
	if !strings.Contains(omenAlphaNote, fmt.Sprintf("model %q", "omen-alpha")) {
		t.Errorf("omenAlphaNote = %q, want it to name the model", omenAlphaNote)
	}
}

// TestExactCompatibilityTableStaysMinimal pins the exact table to the IDs with
// a documented reason: the two provider-default qwen IDs and the single
// unlisted omen-alpha entry. A family prefix must not be used for any of them.
func TestExactCompatibilityTableStaysMinimal(t *testing.T) {
	want := map[string]bool{"qwen3.6-plus": true, "qwen3.7-max": true, "omen-alpha": true}
	if len(exactRoutes) != len(want) {
		t.Fatalf("exactRoutes = %+v, want %d entries", exactRoutes, len(want))
	}
	for _, r := range exactRoutes {
		if !want[r.id] {
			t.Errorf("unexpected exact entry %q", r.id)
		}
		if r.route != RouteChatCompletions {
			t.Errorf("%s: route = %q, want chat-completions", r.id, r.route)
		}
	}
	for _, p := range prefixRoutes {
		if p.prefix == "omen" {
			t.Error(`prefixRoutes contains a bare "omen" prefix; omen-alpha is an exact entry`)
		}
	}
	if _, ok := exactRoute("omen-unknown"); ok {
		t.Error("omen-unknown resolved through the exact table")
	}
	if d, ok := exactRoute("OMEN-ALPHA"); !ok || d.route != RouteChatCompletions {
		t.Error("exact lookup must fold case like the prefix table")
	}
}
