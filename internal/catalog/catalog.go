// Package catalog fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; M3 wires the refresh cadence.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through (FR-004).
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
	// RouteGemini is the native Gemini protocol (spec 04 §4): models are
	// served through /v1/models/<model>:<action>, so a record carries a
	// model-scoped endpoint instead of one from the static table below. The
	// value is pinned to config.ProtocolGemini, so the protocols.gemini switch
	// and this route name cannot drift apart.
	RouteGemini Route = Route(config.ProtocolGemini)
)

// The routable Gemini actions come from the config vocabulary (spec 04 §4/§5):
// a record publishes the non-stream generateContent path, and the executor
// swaps in the stream action for a streaming request instead of accepting a
// free-form action here.
const (
	geminiActionGenerate = config.GeminiActionGenerate
	geminiActionStream   = config.GeminiActionStream
)

// geminiPathReason is the exclusion diagnostic for entries whose resolved
// Gemini endpoint is not the native shape (arch §5).
const geminiPathReason = "resolved endpoint is not a native gemini generateContent or streamGenerateContent path"

// geminiPathPattern pins the native Gemini path shape a gemini-route record
// may carry: /v1/models/<model>:<action>. The model segment is restricted to
// [A-Za-z0-9._-], so a catalog entry cannot smuggle a second path segment, a
// query, or an authority rewrite into the upstream URL, and the action is one
// of the two routable actions built from the config vocabulary.
var geminiPathPattern = regexp.MustCompile(fmt.Sprintf(`^/v1/models/([A-Za-z0-9._-]+):(%s|%s)$`,
	regexp.QuoteMeta(geminiActionGenerate), regexp.QuoteMeta(geminiActionStream)))

// EndpointPath returns the upstream endpoint path for the route (FR-004).
// routeEndpoints maps each route to its upstream endpoint path.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
}

// EndpointPath returns the route's upstream endpoint path. Gemini paths are
// model-scoped and built per record by geminiEndpoint; unknown routes have no
// path at all.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

// geminiEndpoint builds the native Gemini endpoint path for one upstream model
// ID and action.
func geminiEndpoint(model, action string) string {
	return "/v1/models/" + model + ":" + action
}

// validGeminiEndpoint reports whether path has the pinned Gemini shape with a
// usable model segment. Dot-only names are rejected so the path cannot
// collapse onto a parent segment; geminiPathPattern itself already excludes
// '/', ':', '?', '#', '@', '%', and whitespace.
func validGeminiEndpoint(path string) bool {
	m := geminiPathPattern.FindStringSubmatch(path)
	return m != nil && strings.Trim(m[1], ".") != ""
}

func routeFromString(s string) (Route, bool) {
	if r := Route(s); r == RouteGemini {
		return r, true
	}
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry (arch §7, FR-003).
type ModelRecord struct {
	PublicID     string
	UpstreamID   string
	DisplayName  string
	Protocol     Route
	EndpointPath string
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
}

// StreamEndpointPath returns the endpoint path a streaming request must use.
// Every route except Gemini serves both modes through one path; Gemini's
// native API separates the actions, so a record publishes the non-stream
// generateContent path and switches to streamGenerateContent here. A record
// whose override pinned another action (or a path without the native action)
// keeps that value unchanged.
func (r ModelRecord) StreamEndpointPath() string {
	if r.Protocol != RouteGemini {
		return r.EndpointPath
	}
	return strings.Replace(r.EndpointPath, ":"+geminiActionGenerate, ":"+geminiActionStream, 1)
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics (FR-004, open questions §10).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry; unknown fields —
// including protocol/endpoint fields the catalog does not provide and
// reasoning/tool_calling flags the plugin does not act on — are ignored via
// json tags. Modality metadata accepts both spellings.
type rawModel struct {
	ID               string       `json:"id"`
	DisplayName      string       `json:"display_name"`
	ContextLength    int64        `json:"context_length"`
	MaxOutputTokens  int64        `json:"max_output_tokens"`
	InputModes       []string     `json:"input_modes"`
	OutputModes      []string     `json:"output_modes"`
	InputModalities  []string     `json:"input_modalities"`
	OutputModalities []string     `json:"output_modalities"`
	Thinking         *rawThinking `json:"thinking"`
}

// rawThinking tolerantly decodes the optional per-model thinking object;
// pluginapi.ThinkingSupport itself is untagged, so snake_case upstream keys
// are decoded here and mapped field-by-field (partial objects decode to a
// partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// prefixRoutes covers unknown variants of known families; longest prefix wins
// (list is ordered longest-first).
//
// Evidence (models.dev opencode-go snapshot: provider default npm
// @ai-sdk/openai-compatible, per-model provider.npm override
// @ai-sdk/google -> Gemini, @ai-sdk/anthropic -> Messages,
// @ai-sdk/openai -> Responses):
//   - claude*          -> Messages  (claude-haiku-5-5 carries @ai-sdk/anthropic)
//   - space-bunny*     -> Chat      (no override; provider default)
//   - step-5-preview-free -> Chat   (no override; provider default)
//   - cat-jam*         -> Chat      (no override; provider default)
//
// A family whose documented protocol differs from its prefix is corrected in
// exactRoutes below rather than by narrowing these prefixes.
var prefixRoutes = []struct {
	prefix string
	route  Route
}{
	{"muse-spark", RouteResponses},
	{"space-bunny", RouteChatCompletions},
	{"deepseek", RouteChatCompletions},
	{"minimax", RouteMessages},
	{"longcat", RouteChatCompletions},
	{"gemini", RouteGemini},
	{"claude", RouteMessages},
	{"grok", RouteResponses},
	{"gpt", RouteResponses},
	{"kimi", RouteChatCompletions},
	{"qwen", RouteMessages},
	{"glm", RouteChatCompletions},
	{"mimo", RouteChatCompletions},
	{"cat-jam", RouteChatCompletions},
	{"step", RouteChatCompletions},
	{"hy", RouteChatCompletions},
}

// omenAlphaNote is the diagnostics line for the one catalog model that upstream
// protocol metadata does not list. Evidence: the models.dev opencode-go
// snapshot contains no omen* ID at all, and a read-only probe of the live
// catalog's minimal chat request returned HTTP 200 but produced no SSE data
// event within 25s (curl exit 28 after two direct TLS EOFs, so no inference
// ran). The model is therefore routed as Chat Completions on compatibility
// grounds only: upstream stream acceptance is still unconfirmed, and this note
// is a pending-acceptance marker, not a claim that streaming works.
const omenAlphaNote = `model "omen-alpha" routed via compatibility fallback (chat-completions); upstream stream acceptance pending`

// exactRoutes pins the models whose documented upstream protocol differs from
// what their family prefix would give them (arch §5 route priority). The table
// is consulted after user overrides and before prefixRoutes, so an override
// still wins and an unknown ID still falls through to the prefix table.
//
// Evidence: the models.dev opencode-go snapshot lists qwen3.6-plus and
// qwen3.7-max with no provider.npm override, so they inherit the provider
// default @ai-sdk/openai-compatible (Chat Completions), while every other qwen
// ID carries @ai-sdk/anthropic (Messages) — only those two are corrected.
// omen-alpha is the single ID absent from metadata (see omenAlphaNote).
var exactRoutes = []struct {
	id    string
	route Route
	note  string
}{
	{"qwen3.6-plus", RouteChatCompletions, ""},
	{"qwen3.7-max", RouteChatCompletions, ""},
	{"omen-alpha", RouteChatCompletions, omenAlphaNote},
}

// exactRoute looks an ID up in the exact compatibility table, folding case the
// same way prefixRoutes does.
func exactRoute(id string) (routeDecision, bool) {
	id = strings.ToLower(id)
	for _, r := range exactRoutes {
		if r.id == id {
			return routeDecision{route: r.route, warn: r.note}, true
		}
	}
	return routeDecision{}, false
}

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client.
func New(cfg config.Config, client HostClient) *Manager {
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate (arch §5).
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-url/
// protocols/prefix (F5: a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success (FR-002).
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    m.cfg.CatalogURL,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	}
	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// clear like an empty plan (swap(nil)) but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return m.fail("invalid json")
	}
	m.swap(entries, warns...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone (FR-002). Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns = nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded entries and installs it
// atomically under the mutex (FR-010 dedup, arch §5 route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) swap(entries []rawModel, extraWarns ...string) {
	models := make([]ModelRecord, 0, len(entries))
	index := make(map[string]ModelRecord, len(entries)*2)
	var unsup []UnsupportedModel
	var warns []string
	warns = append(warns, extraWarns...)
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
			continue
		}
		if seen[e.ID] {
			warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
			continue
		}
		seen[e.ID] = true
		d := m.resolve(e)
		if d.warn != "" {
			warns = append(warns, d.warn)
		}
		if d.route == "" {
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: "no route determined"})
			continue
		}
		route, endpoint := d.route, d.endpoint
		if !m.protocolEnabled(route) {
			// Flags apply at snapshot build; a stale snapshot kept per
			// FR-002 reflects the flags it was built with until the next
			// successful refresh.
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     fmt.Sprintf("protocol %s disabled by config", route),
			})
			continue
		}
		if endpoint != "" && endpointEscapesBase(m.cfg.BaseURL, endpoint) {
			// The joined URL must stay on the configured upstream
			// authority. Override endpoints flow through here, so they
			// cannot rewrite the URL host — e.g. "/v1@evil.com/x"
			// against a bare-host base-url turns the base into userinfo
			// and would send credentials/prompts to an attacker host
			// (spec 05 §2 trust boundary).
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     "resolved endpoint escapes the configured upstream host",
			})
			continue
		}
		if endpoint == "" {
			endpoint = route.EndpointPath()
		}
		if route == RouteGemini && !validGeminiEndpoint(endpoint) {
			// The native Gemini path carries the model ID, so an entry whose
			// ID (or an override template) is not a plain model segment could
			// add a path segment or a query string to the upstream URL.
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: geminiPathReason})
			continue
		}
		inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
		outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
		display := e.DisplayName
		if display == "" {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:     config.PublicID(m.cfg, e.ID),
			UpstreamID:   e.ID,
			DisplayName:  display,
			Protocol:     route,
			EndpointPath: endpoint,
			ContextLimit: e.ContextLength,
			OutputLimit:  e.MaxOutputTokens,
			InputModes:   inputModes,
			OutputModes:  outputModes,
			Thinking:     normalizeThinking(e.Thinking),
		}
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID (upstream "foo" and "opencode-go/foo" both
		// claim index key "<prefix>/foo"); last-write-wins would silently
		// misroute. First in catalog order wins (same dedup rule as
		// duplicate IDs); the later record is excluded. Same-role clashes
		// are impossible — distinct upstream IDs and an injective public
		// mapping — so the slot owner always holds the key via its other
		// role, and a record's own public==upstream rewrite is not a clash.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = entries, models, index, unsup, warns
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// (spec 04 §4); flags default to true.
func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	case RouteGemini:
		return m.cfg.Protocols.Gemini
	}
	return true
}

// routeDecision is the outcome of resolving one catalog entry (arch §5): route
// is "" when no priority tier matched, endpoint is the model-scoped path for a
// route without a static one (Gemini) or the path a user override pinned, and
// warn is the spec 04 §5 diagnostics line for an applied override.
type routeDecision struct {
	route    Route
	endpoint string
	warn     string
}

// resolve applies route priority strictly (arch §5): user override > exact
// compatibility table > family prefix table > unsupported.
func (m *Manager) resolve(e rawModel) routeDecision {
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// Spec 04 §5: an override MUST be visible in diagnostics. Load
		// validation (spec 04 §6) guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		return resolveOverride(e.ID, o)
	}
	id := strings.ToLower(e.ID)
	if d, ok := exactRoute(id); ok {
		return d
	}
	for _, p := range prefixRoutes {
		if strings.HasPrefix(id, p.prefix) {
			if p.route == RouteGemini {
				return routeDecision{route: p.route, endpoint: geminiEndpoint(e.ID, geminiActionGenerate)}
			}
			return routeDecision{route: p.route}
		}
	}
	return routeDecision{}
}

// resolveOverride turns one validated user override into a decision. The
// Gemini route has no static endpoint: an override that does not name one
// supplies the model and action templates (defaulting to the cataloged ID and
// generateContent) that make up the native path.
func resolveOverride(id string, o config.RouteOverride) routeDecision {
	route := Route(o.Protocol)
	endpoint := o.Endpoint
	if route == RouteGemini && endpoint == "" {
		model := o.Model
		if model == "" {
			model = config.GeminiModelPlaceholder
		}
		action := o.Action
		if action == "" {
			action = geminiActionGenerate
		}
		endpoint = geminiEndpoint(strings.ReplaceAll(model, config.GeminiModelPlaceholder, id), action)
	}
	return routeDecision{route: route, endpoint: endpoint, warn: fmt.Sprintf(
		"model %q routed via user override (%s %s)", id, o.Protocol, endpoint)}
}

// JoinUpstreamURL joins the configured base-url onto an endpoint path and
// is the single source of truth for that join (spec 05 §2): base-url
// already carries the version segment (default .../zen/go/v1, catalog at
// {base}/models), so a leading "/v1" on the endpoint is dropped instead of
// doubled. The authority-escape check (endpointEscapesBase) must parse
// exactly the URL this produces — two independent join formulas would be
// free to drift apart and validate one URL while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	return strings.TrimSuffix(baseURL, "/") + strings.TrimPrefix(endpoint, "/v1")
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority or scheme
// than the configured base-url. Any parse failure counts as escaping (fail
// closed).
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(JoinUpstreamURL(baseURL, endpoint))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking converts the decoded raw thinking object into its
// pluginapi form; absent objects yield nil, partial ones a partial struct.
func normalizeThinking(rt *rawThinking) *pluginapi.ThinkingSupport {
	if rt == nil {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	if rt.Min != nil {
		t.Min = *rt.Min
	}
	if rt.Max != nil {
		t.Max = *rt.Max
	}
	if rt.ZeroAllowed != nil {
		t.ZeroAllowed = *rt.ZeroAllowed
	}
	if rt.DynamicAllowed != nil {
		t.DynamicAllowed = *rt.DynamicAllowed
	}
	if len(rt.Levels) > 0 {
		t.Levels = append([]string(nil), rt.Levels...)
	}
	return t
}

// Models returns the routable snapshot (FR-003 identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
