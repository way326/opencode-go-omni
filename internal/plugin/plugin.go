package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/config"
	"github.com/way326/opencode-go-omni/internal/errclass"
)

// ProviderID is the single provider key served by this plugin (FR-001).
const ProviderID = "opencode-go"

// pluginName / pluginVersion are reported in registration metadata.
const (
	pluginName    = "opencode-go-omni"
	pluginVersion = "0.2.3"
)

// githubRepoURL satisfies the host's validPlugin gate (host.go
// validPlugin rejects empty Metadata.GitHubRepository).
const githubRepoURL = "https://github.com/way326/opencode-go-omni"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 15m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager
	// authSync serializes catalog revision writes across lifecycle and ticker
	// callbacks owned by this Manager.
	authSync *catalogRevisionSyncer

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	// rpcMu gates methods that can use the host while shutdown drains. Lifecycle
	// methods are serialized separately by lifeMu; ordinary HandleCall paths do
	// not acquire lifeMu, so shutdown never waits on a call that is waiting for
	// the lifecycle lock it holds.
	rpcMu               sync.Mutex
	rpcIdle             chan struct{}
	rpcActive           int
	rpcClosing          bool
	rpcShutdownComplete bool
	rpcEpoch            uint64 // invalidates lifecycle calls queued before shutdown

	mu  sync.RWMutex
	cfg config.Config
	mgr *catalog.Manager
	// credentialDirectory maps only active, configured reference IDs. Staged
	// entries are used only while CPA may already see a file from an in-flight
	// lifecycle save; each binding carries its matching config/catalog snapshot.
	credentialDirectory       map[string]credentialBinding
	stagedCredentialDirectory map[string]credentialBinding
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge, authSync: newCatalogRevisionSyncer(bridge)}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	if method != pluginabi.MethodPluginRegister &&
		method != pluginabi.MethodPluginReconfigure &&
		method != pluginabi.MethodPluginShutdown {
		release, ok := m.enterRPC()
		if !ok {
			return ErrEnvelope("plugin_shutting_down", "plugin is draining active work"), nil
		}
		defer release()
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return m.handleModels()
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		resp, err := (authProvider{}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth parse response provider=%s id=%s credential_ref_present=%t credential_ref_bytes=%d storage_json_bytes=%d", resp.Auth.Provider, resp.Auth.ID, isCanonicalCredentialID(resp.Auth.ID), len(resp.Auth.ID), len(resp.Auth.StorageJSON))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		_, err := (authProvider{}).StartLogin(context.Background(), req.AuthLoginStartRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		_, err := (authProvider{}).PollLogin(context.Background(), req.AuthLoginPollRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		resp, err := (authProvider{}).RefreshAuth(context.Background(), req.AuthRefreshRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{
			SupportedProviders: []string{ProviderID},
			DisplayName:        "OpenCode Go",
			SupportsReset:      false,
		}), nil
	case pluginabi.MethodQuotaFetch:
		return m.handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return ErrEnvelope("unsupported", "OpenCode Go quota reset is not supported"), nil
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "executor.count_tokens has no OpenCode Go equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by opencode-go", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func pluginConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "api-keys",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "List of OpenCode Go API keys (`- value: ...`). Supports ${ENV_VAR}; prefer environment references over literal values.",
		},
		{
			Name:        "base-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Upstream base URL (default: https://opencode.ai/inference/go/v1).",
		},
		{
			Name:        "catalog-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional catalog discovery URL (default: {base-url}/models).",
		},
		{
			Name:        "model-prefix",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Client-facing model ID prefix configuration (`enabled: bool`, `value: string`).",
		},
		{
			Name:        "catalog",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Catalog discovery settings (`refresh-interval: string`, `stale-while-unavailable: bool`).",
		},
		{
			Name:        "protocols",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Protocol switches (`chat-completions`, `messages`, `responses`, `gemini`).",
		},
		{
			Name:        "route-overrides",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: "Explicit route overrides per model (`<model-id>: {protocol: string, endpoint: string}`).",
		},
		{
			Name:        "request-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Upstream HTTP request timeout (default: 5m).",
		},
		{
			Name:        "max-response-bytes",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Description: "Max non-streaming response body size in bytes (default: 67108864).",
		},
		{
			Name:        "allow-http",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "Allow plain http:// URLs for local testing/mocking (default: false).",
		},
	}
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "claude", "openai-response", "gemini"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			ConfigFields:     pluginConfigFields(),
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          false,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
			QuotaProvider:         true,
		},
	})
}

func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	return okEnvelope(struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Resources []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		} `json:"resources"`
	}{
		Routes: []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}{{Method: "POST", Path: "/plugins/" + pluginName + "/quota-usage"}},
		Resources: []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		}{{Path: "/quota", Menu: "OpenCode Go Quota", Description: "View OpenCode Go quota windows."}},
	}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	epoch, admitted := m.lifecycleAdmission()
	if !admitted {
		return ErrEnvelope("plugin_shutting_down", "plugin is draining active work"), nil
	}
	return m.handleLifecycleAdmitted(request, epoch)
}

// handleLifecycleAdmitted rechecks the admission generation after lifeMu is
// acquired: a register queued before shutdown must not reopen the bridge once
// that shutdown finishes. Only a fresh post-shutdown request may reopen it.
func (m *Manager) handleLifecycleAdmitted(request []byte, epoch uint64) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if !m.reopenAfterShutdown(epoch) {
		return ErrEnvelope("plugin_shutting_down", "lifecycle request preceded plugin shutdown"), nil
	}
	literalKeys := 0
	for _, key := range cfg.APIKeys {
		if key.Source != config.APIKeySourceEnvironment {
			literalKeys++
		}
	}
	if literalKeys > 0 {
		if m.bridge != nil {
			_ = m.bridge.Log("warn", "OpenCode Go API key configuration contains literal or mixed values; prefer environment references", map[string]any{
				"affected_keys": literalKeys,
			})
		} else {
			debugTrace("lifecycle literal_api_key_warning affected_keys=%d", literalKeys)
		}
	}
	debugTrace("lifecycle config_loaded key_count=%d prefix_enabled=%t prefix=%s", len(cfg.APIKeys), cfg.ModelPrefix.Enabled, cfg.ModelPrefix.Value)
	var entries []pluginapi.HostAuthFileEntry
	if m.bridge != nil {
		listCtx, listCancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
		entries, err = m.bridge.AuthList(listCtx)
		listCancel()
		if err != nil {
			return ErrEnvelope("auth_materialization_failed", err.Error()), nil
		}
	}
	// A nil *HostBridge must not enter the interface as a typed nil, or
	// catalog's nil-client guard never fires and Refresh panics inside Do.
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	m.mu.RLock()
	previousMgr := m.mgr
	m.mu.RUnlock()
	if cfg.Catalog.StaleWhileUnavailable && previousMgr != nil && len(previousMgr.Models()) > 0 {
		mgr.SeedFrom(previousMgr)
	}
	candidateCredentials, err := buildCredentialDirectory(cfg, mgr)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.resetStagedCredentialDirectory()
	if m.authSync != nil {
		m.authSync.mu.Lock()
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	if err := m.materializeAuthRecordsFromList(saveCtx, cfg, entries, candidateCredentials); err != nil {
		m.retainOnlyPersistedStagedCredentials(saveCtx)
		saveCancel()
		if m.authSync != nil {
			m.authSync.mu.Unlock()
		}
		return ErrEnvelope("auth_materialization_failed", err.Error()), nil
	}
	saveCancel()
	if m.authSync != nil {
		m.authSync.mu.Unlock()
	}
	refreshErr := refreshOnce(context.Background(), mgr, m.bridge, registerRefreshTimeout, cfg)
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t", len(mgr.Models()), refreshErr != nil)

	// Retire any running loop and wait for its exit outside m.mu: a mid-refresh
	// tick must never stall readers holding RLock (F4). lifeMu keeps the
	// stop-wait-install sequence atomic against other lifecycles.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	// FR-002 stale-while-unavailable: a failed refresh must neither wipe a
	// non-empty previous snapshot nor keep the OLD manager, whose stored cfg
	// would pin the old base-url/catalog-url/protocols/prefix so ticks
	// fetched the old URL with the new config's keys forever (F5). The NEW
	// manager is adopted unconditionally; when a previous snapshot exists
	// AND the NEW config keeps stale-while-unavailable enabled, it is
	// seeded into the new one, so serving is unchanged until the next
	// successful tick refreshes against the NEW config. Under fail-closed
	// (stale-while-unavailable:false) no seed happens — same as the ticker
	// failure path — so a reconfigure during an outage serves nothing until
	// a refresh succeeds, honoring the operator's policy on BOTH paths.
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	m.credentialDirectory = candidateCredentials
	m.stagedCredentialDirectory = nil
	m.mu.Unlock()

	// Publish the new Manager before asking CPA to re-read auth files: the
	// watcher may call model.for_auth while this lifecycle call is still active.
	// The next ticker generation is installed only after this batch finishes.
	syncCtx, syncCancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	m.syncCatalogRevision(syncCtx, mgr)
	syncCancel()

	stop, done := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	return registrationEnvelope(), nil
}

// handleModels implements model.static / model.for_auth (FR-003): the last
// good catalog snapshot mapped to wire ModelInfos; empty catalog yields an
// empty slice, not an error.
func (m *Manager) handleModels() ([]byte, error) {
	models := m.servedModelInfos()
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// shutdownDrainTimeout bounds how long handleShutdown waits before writing a
// diagnostic that draining is slow. It never permits shutdown to return while
// a host callback or stream cleanup is still in flight. Package var so tests
// can shrink it.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.closeRPCGate()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	if m.bridge != nil {
		m.bridge.BeginShutdown()
	}

	deadline := time.Now().Add(shutdownDrainTimeout)
	warned := false
	warnStillDraining := func() {
		if warned {
			return
		}
		warned = true
		message := "shutdown drain exceeded its diagnostic timeout; work is still in flight"
		debugTrace("%s", message)
	}

	if !m.waitRPCs(time.Until(deadline)) {
		warnStillDraining()
		m.waitRPCsForever()
	}
	if m.bridge != nil {
		remaining := time.Until(deadline)
		if remaining <= 0 || !m.bridge.WaitForInFlight(remaining) {
			warnStillDraining()
			m.bridge.WaitForInFlightForever()
		}
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	m.markShutdownComplete()
	return okEnvelope(struct{}{}), nil
}

// enterRPC admits one ordinary RPC and returns its release function. The
// generation channel tracks the transition to zero without adding to a
// WaitGroup, so shutdown can safely close admission before waiting.
func (m *Manager) enterRPC() (func(), bool) {
	m.rpcMu.Lock()
	m.initRPCIdleLocked()
	if m.rpcClosing {
		m.rpcMu.Unlock()
		return nil, false
	}
	if m.rpcActive == 0 {
		m.rpcIdle = make(chan struct{})
	}
	m.rpcActive++
	m.rpcMu.Unlock()
	return m.leaveRPC, true
}

func (m *Manager) leaveRPC() {
	m.rpcMu.Lock()
	defer m.rpcMu.Unlock()
	if m.rpcActive == 0 {
		panic("plugin RPC gate underflow")
	}
	m.rpcActive--
	if m.rpcActive == 0 {
		close(m.rpcIdle)
	}
}

func (m *Manager) initRPCIdleLocked() {
	if m.rpcIdle != nil {
		return
	}
	m.rpcIdle = make(chan struct{})
	close(m.rpcIdle)
}

func (m *Manager) closeRPCGate() {
	m.rpcMu.Lock()
	m.initRPCIdleLocked()
	m.rpcEpoch++
	m.rpcClosing = true
	m.rpcMu.Unlock()
}

func (m *Manager) waitRPCs(timeout time.Duration) bool {
	m.rpcMu.Lock()
	m.initRPCIdleLocked()
	idle := m.rpcIdle
	m.rpcMu.Unlock()
	if timeout <= 0 {
		select {
		case <-idle:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

func (m *Manager) waitRPCsForever() {
	m.rpcMu.Lock()
	m.initRPCIdleLocked()
	idle := m.rpcIdle
	m.rpcMu.Unlock()
	<-idle
}

// lifecycleAdmission captures an epoch before a lifecycle request can wait on
// lifeMu. Calls entering during a drain cannot queue a later reopening.
func (m *Manager) lifecycleAdmission() (uint64, bool) {
	m.rpcMu.Lock()
	defer m.rpcMu.Unlock()
	return m.rpcEpoch, !m.rpcClosing || m.rpcShutdownComplete
}

// reopenAfterShutdown permits only a current-generation lifecycle request to
// reopen a fully drained bridge. The caller holds lifeMu.
func (m *Manager) reopenAfterShutdown(epoch uint64) bool {
	m.rpcMu.Lock()
	defer m.rpcMu.Unlock()
	if epoch != m.rpcEpoch || (m.rpcClosing && !m.rpcShutdownComplete) {
		return false
	}
	if !m.rpcShutdownComplete {
		return true
	}
	if m.bridge != nil {
		m.bridge.ReopenAfterShutdown()
	}
	m.rpcShutdownComplete = false
	m.rpcClosing = false
	return true
}

func (m *Manager) markShutdownComplete() {
	m.rpcMu.Lock()
	m.rpcShutdownComplete = true
	m.rpcClosing = true
	m.rpcMu.Unlock()
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout, cfg)
					m.syncCatalogRevision(stopCtx, mgr)
				}()
			}
		}
	}()
}

// syncCatalogRevision derives a revision from the final served projection and
// asks the syncer to inspect every plugin Auth. Per-file revision comparison
// avoids unnecessary saves while still discovering Auth files added later.
// Host callbacks receive the captured mgr; they never read Manager state.
func (m *Manager) syncCatalogRevision(ctx context.Context, mgr *catalog.Manager) {
	if m == nil || m.bridge == nil || m.authSync == nil || mgr == nil {
		return
	}
	revision, err := catalogRevision(publishedModelInfos(mgr.Models()))
	if err != nil {
		_ = m.bridge.Log("warn", "catalog sync revision failed", nil)
		return
	}
	stats, err := m.authSync.syncRevision(ctx, revision)
	if err != nil {
		return
	}
	debugTrace("catalog sync candidates=%d saved=%d unchanged=%d failed=%d",
		stats.Candidates, stats.Saved, stats.Unchanged, stats.Failed)
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce runs one bounded catalog refresh. Catalog refresh is not a client
// request, so the fallback uses the first configured key only and has no client
// selection, rotation, cooldown, or retry state. parent bounds-and-cancels
// the attempt: the lifecycle path passes context.Background() plus
// registerRefreshTimeout; ticker ticks pass the loop's stop-derived context
// plus the full request-timeout, so close(stop) aborts an in-flight tick
// (F4). On failure it logs a warn via host.log — error text is a redacted
// category label from the catalog package, never key material (FR-011).
func refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config) error {
	// Fallback: configured materialization input is catalog-only; CPA client
	// authentication remains owned by its AuthProvider and CPA.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err := mgr.Refresh(ctx, cfg.APIKeys[0].Value)
	if err != nil {
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface normalization diagnostics with the post-refresh log.
	warnings := mgr.Warnings()
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
