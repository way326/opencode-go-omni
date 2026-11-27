// Package catalog fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// unavailable-model block > built-in compatibility table > upstream protocol
// metadata > family prefix > unsupported (excluded from the routable set,
// retained in diagnostics).
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
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

type refreshError struct {
	category   string
	statusCode int
}

func (e *refreshError) Error() string { return "catalog refresh failed: " + e.category }

func (e *refreshError) isAuthenticationFailure() bool {
	return e.statusCode == http.StatusUnauthorized || e.statusCode == http.StatusForbidden
}

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
	RouteSource  string
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
	UpstreamID string `json:"upstream_id,omitempty"`
	Reason     string `json:"reason"`
}

// Diagnostics is a redacted view of the shared catalog snapshot.
type Diagnostics struct {
	LastSuccess         *time.Time         `json:"last_success,omitempty"`
	LastFailure         *time.Time         `json:"last_failure,omitempty"`
	LastFailureCategory string             `json:"last_failure_category,omitempty"`
	HasSnapshot         bool               `json:"has_snapshot"`
	Stale               bool               `json:"stale"`
	RoutableCount       int                `json:"routable_count"`
	Models              []ModelDiagnostic  `json:"models"`
	Excluded            []UnsupportedModel `json:"excluded"`
	Warnings            []string           `json:"warnings"`
}

type ModelDiagnostic struct {
	ClientID    string `json:"client_id"`
	UpstreamID  string `json:"upstream_id"`
	Name        string `json:"name"`
	Protocol    Route  `json:"protocol"`
	RouteSource string `json:"route_source"`
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry. The current OpenCode Go
// catalog exposes display metadata under name/limit/modalities and protocol
// metadata under recommended_api/api; the older flat fields remain supported
// so last-good snapshots and older catalog responses keep decoding.
type rawModel struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	DisplayName      string                     `json:"display_name"`
	ContextLength    int64                      `json:"context_length"`
	MaxOutputTokens  int64                      `json:"max_output_tokens"`
	InputModes       []string                   `json:"input_modes"`
	OutputModes      []string                   `json:"output_modes"`
	InputModalities  []string                   `json:"input_modalities"`
	OutputModalities []string                   `json:"output_modalities"`
	Limit            *rawLimit                  `json:"limit"`
	Modalities       *rawModalSet               `json:"modalities"`
	RecommendedAPI   string                     `json:"recommended_api"`
	API              map[string]json.RawMessage `json:"api"`
	Thinking         *rawThinking               `json:"thinking"`
}

type rawLimit struct {
	Context int64 `json:"context"`
	Input   int64 `json:"input"`
	Output  int64 `json:"output"`
}

type rawModalSet struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
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

// upstreamAPIRoutes is the complete allowlist for protocol metadata accepted
// from the catalog. Unknown values (for example embeddings, interactions or
// systemone) stay unsupported; metadata never supplies an endpoint or URL.
var upstreamAPIRoutes = map[string]Route{
	"chat-completions": RouteChatCompletions,
	"messages":         RouteMessages,
	"responses":        RouteResponses,
	"generate-content": RouteGemini,
}

// unavailableModels keeps known upstream outages pinned out of the routable
// set even when catalog metadata advertises a supported protocol. A user route
// override still wins, allowing an explicitly verified retry without a code
// change.
var unavailableModels = map[string]string{
	"iceman-contributor-free": "upstream endpoint unavailable (HTTP 503 observed 2026-10-27)",
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

// exactRoutes pins models that need an exact route: their documented upstream
// protocol differs from what their family prefix gives them, or they have no
// safely supported family prefix at all (arch §5 route priority). The table is
// consulted after user overrides and before upstream metadata/prefixRoutes, so
// an override still wins and an unknown ID still falls through safely.
//
// Evidence: the models.dev opencode-go snapshot lists qwen3.6-plus and
// qwen3.7-max with no provider.npm override, so they inherit the provider
// default @ai-sdk/openai-compatible (Chat Completions), while every other qwen
// ID carries @ai-sdk/anthropic (Messages) — only those two are corrected.
// omen-alpha is the single ID absent from metadata (see omenAlphaNote).
// beam-free appears in the 2026-10-17 live catalog and models.dev snapshot;
// its absent npm override inherits the provider's Chat Completions default.
// Pin this exact ID rather than assuming a protocol for future beam variants.
// hungerg-free was added to the live catalog on 2026-11-24 and declares Chat
// Completions. Keep the exact entry as a fallback if that metadata disappears;
// newer unseen families are routed from upstream metadata by resolve.
//
// iceman-contributor-free is intentionally absent while its upstream endpoint
// is unavailable (503 observed 2026-10-27). OpenCode's Go documentation and
// models.dev declare Chat Completions, but no route is pinned until inference
// can be accepted; the catalog therefore reports it as unsupported.
var exactRoutes = []struct {
	id    string
	route Route
	note  string
}{
	{"beam-free", RouteChatCompletions, ""},
	{"hungerg-free", RouteChatCompletions, ""},
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

	mu          sync.Mutex
	snapshot    catalogSnapshot
	lastSuccess time.Time
	lastFailure time.Time
	lastError   string
	stale       bool
}

type catalogSnapshot struct {
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1). A nil index denotes no snapshot.
	index       map[string]ModelRecord
	unsup       []UnsupportedModel
	warns       []string
	decodeWarns []string
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
	snapshot := prev.snapshot
	lastSuccess, lastFailure, lastError := prev.lastSuccess, prev.lastFailure, prev.lastError
	sourceStale := prev.stale
	prev.mu.Unlock()
	if snapshot.index == nil {
		return
	}
	rebuilt := m.buildSnapshot(snapshot.raw, snapshot.decodeWarns...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = rebuilt
	// Keep this configuration's failed refresh if it has one. The retiring
	// manager may have completed a later attempt under the previous config.
	m.stale = m.lastFailure.After(m.lastSuccess) || sourceStale
	if m.lastFailure.IsZero() {
		m.lastFailure, m.lastError = lastFailure, lastError
	}
	m.lastSuccess = lastSuccess
}

// Refresh refreshes the catalog using a single discovery key.
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	_, err := m.refresh(ctx, []config.APIKey{{Value: apiKey}})
	return err
}

// RefreshConfigured refreshes the shared snapshot with the configured keys
// in order. Only HTTP 401/403 allow another attempt, under the same context.
// The result and stale policy are published once, after the whole round.
func (m *Manager) RefreshConfigured(ctx context.Context) (attempts int, err error) {
	return m.refresh(ctx, m.cfg.APIKeys)
}

func (m *Manager) refresh(ctx context.Context, keys []config.APIKey) (int, error) {
	failure := &refreshError{category: "no configured api key"}
	attempts := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			failure = &refreshError{category: "request canceled or timed out"}
			break
		}
		attempts++
		entries, warns, err := m.fetch(ctx, key.Value)
		if err == nil {
			snapshot := m.buildSnapshot(entries, warns...)
			m.mu.Lock()
			m.snapshot = snapshot
			m.lastSuccess = time.Now()
			m.stale = false
			m.mu.Unlock()
			return attempts, nil
		}
		failure = err
		if !err.isAuthenticationFailure() || ctx.Err() != nil {
			break
		}
	}
	m.recordFailure(failure)
	return attempts, failure
}

func (m *Manager) fetch(ctx context.Context, apiKey string) ([]rawModel, []string, *refreshError) {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return nil, nil, &refreshError{category: "host client unavailable"}
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
		return nil, nil, &refreshError{category: "network error"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &refreshError{category: fmt.Sprintf("http %d", resp.StatusCode), statusCode: resp.StatusCode}
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return nil, nil, &refreshError{category: "response exceeds max-response-bytes"}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, nil, &refreshError{category: "invalid json"}
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// publish an empty catalog but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return nil, nil, &refreshError{category: "invalid json"}
	}
	return entries, warns, nil
}

// recordFailure applies the stale policy once all fetch attempts have ended.
func (m *Manager) recordFailure(failure *refreshError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastFailure = time.Now()
	m.lastError = failure.category
	if !m.cfg.Catalog.StaleWhileUnavailable {
		m.snapshot = catalogSnapshot{}
	}
	m.stale = m.snapshot.index != nil
}

// buildSnapshot resolves decoded entries (FR-010 dedup, arch §5 route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) buildSnapshot(entries []rawModel, extraWarns ...string) catalogSnapshot {
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
			reason := d.reason
			if reason == "" {
				reason = "no route determined"
			}
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
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
		if e.Modalities != nil {
			inputModes = firstNonEmpty(e.Modalities.Input, inputModes)
			outputModes = firstNonEmpty(e.Modalities.Output, outputModes)
		}
		contextLength := e.ContextLength
		outputLimit := e.MaxOutputTokens
		if e.Limit != nil {
			if e.Limit.Context > 0 {
				contextLength = e.Limit.Context
			}
			if e.Limit.Output > 0 {
				outputLimit = e.Limit.Output
			}
		}
		display := e.Name
		if display == "" {
			display = e.DisplayName
		}
		if display == "" {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:     config.PublicID(m.cfg, e.ID),
			UpstreamID:   e.ID,
			DisplayName:  display,
			Protocol:     route,
			RouteSource:  d.source,
			EndpointPath: endpoint,
			ContextLimit: contextLength,
			OutputLimit:  outputLimit,
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
	return catalogSnapshot{raw: entries, models: models, index: index, unsup: unsup, warns: warns, decodeWarns: extraWarns}
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
// warn carries override or route-conflict diagnostics; reason explains a
// metadata-bearing entry that could not be mapped safely. source identifies
// the winning routing tier for routable models.
type routeDecision struct {
	route    Route
	endpoint string
	warn     string
	reason   string
	source   string
}

// resolve applies route priority strictly (arch §5): user override >
// unavailable-model block > exact compatibility table > upstream metadata >
// family prefix table > unsupported.
func (m *Manager) resolve(e rawModel) routeDecision {
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// Spec 04 §5: an override MUST be visible in diagnostics. Load
		// validation (spec 04 §6) guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		return resolveOverride(e.ID, o)
	}
	id := strings.ToLower(e.ID)
	if reason, blocked := unavailableModels[id]; blocked {
		return routeDecision{reason: reason}
	}
	if d, ok := exactRoute(id); ok {
		d.source = "exact_compatibility"
		if metadata, claimed := resolveUpstreamMetadata(e); claimed && metadata.route != d.route {
			upstreamDecision := string(metadata.route)
			if upstreamDecision == "" {
				upstreamDecision = metadata.reason
			}
			if d.warn != "" {
				d.warn += "; "
			}
			d.warn += fmt.Sprintf("route conflict for %q: exact compatibility rule selects %s but upstream metadata reports %s", e.ID, d.route, upstreamDecision)
		}
		return d
	}
	if d, claimed := resolveUpstreamMetadata(e); claimed {
		if d.route != "" {
			d.source = "upstream_metadata"
		}
		return d
	}
	for _, p := range prefixRoutes {
		if strings.HasPrefix(id, p.prefix) {
			if p.route == RouteGemini {
				return routeDecision{route: p.route, endpoint: geminiEndpoint(e.ID, geminiActionGenerate), source: "family_fallback"}
			}
			return routeDecision{route: p.route, source: "family_fallback"}
		}
	}
	return routeDecision{}
}

// resolveUpstreamMetadata maps the catalog's explicit protocol declaration to
// one of the four supported wire routes. The bool reports that the entry
// carried protocol metadata, even when it was unsupported or ambiguous. That
// distinction prevents a new protocol-only family (for example a decision
// model under a legacy gpt prefix) from accidentally inheriting a family
// route. When recommended_api is unsupported, a single supported API entry may
// still identify the wire route (Gemini currently recommends interactions but
// exposes generate-content).
func resolveUpstreamMetadata(e rawModel) (routeDecision, bool) {
	recommended := strings.ToLower(strings.TrimSpace(e.RecommendedAPI))
	if recommended == "" && len(e.API) == 0 {
		return routeDecision{}, false
	}
	if route, ok := upstreamAPIRoutes[recommended]; ok {
		return metadataRouteDecision(e, route), true
	}

	routes := make(map[Route]struct{})
	for apiName := range e.API {
		if route, ok := upstreamAPIRoutes[strings.ToLower(strings.TrimSpace(apiName))]; ok {
			routes[route] = struct{}{}
		}
	}
	if len(routes) == 1 {
		for route := range routes {
			return metadataRouteDecision(e, route), true
		}
	}
	if len(routes) > 1 {
		return routeDecision{reason: "upstream metadata is ambiguous between supported protocols"}, true
	}
	return routeDecision{reason: "upstream metadata advertises no supported protocol"}, true
}

func metadataRouteDecision(e rawModel, route Route) routeDecision {
	d := routeDecision{route: route}
	if route == RouteGemini {
		d.endpoint = geminiEndpoint(e.ID, geminiActionGenerate)
	}
	return d
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
	// Do not log or publish the configured endpoint; it may contain secrets.
	return routeDecision{route: route, endpoint: endpoint, source: "user_override", warn: fmt.Sprintf(
		"model %q routed via user override (%s)", id, o.Protocol)}
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
	out := make([]ModelRecord, len(m.snapshot.models))
	copy(out, m.snapshot.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.snapshot.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.snapshot.unsup))
	copy(out, m.snapshot.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.snapshot.warns))
	copy(out, m.snapshot.warns)
	return out
}

// Diagnostics returns a copy of the redacted catalog status and routing
// projection for read-only management surfaces.
func (m *Manager) Diagnostics() Diagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Diagnostics{
		LastFailureCategory: m.lastError,
		HasSnapshot:         m.snapshot.index != nil,
		Stale:               m.stale,
		RoutableCount:       len(m.snapshot.models),
		Models:              make([]ModelDiagnostic, 0, len(m.snapshot.models)),
		Excluded:            append([]UnsupportedModel{}, m.snapshot.unsup...),
		Warnings:            append([]string{}, m.snapshot.warns...),
	}
	if !m.lastSuccess.IsZero() {
		lastSuccess := m.lastSuccess.UTC()
		result.LastSuccess = &lastSuccess
	}
	if !m.lastFailure.IsZero() {
		lastFailure := m.lastFailure.UTC()
		result.LastFailure = &lastFailure
	}
	for _, model := range m.snapshot.models {
		result.Models = append(result.Models, ModelDiagnostic{
			ClientID:    model.PublicID,
			UpstreamID:  model.UpstreamID,
			Name:        model.DisplayName,
			Protocol:    model.Protocol,
			RouteSource: model.RouteSource,
		})
	}
	return result
}
