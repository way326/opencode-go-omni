package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/config"
)

const (
	cooledRefreshKey  = "sk-refresh-cooled-key"
	goodRefreshKey    = "sk-refresh-good-key"
	refreshTwoKeyYAML = "api-keys:\n" +
		"  - value: " + cooledRefreshKey + "\n" +
		"  - value: " + goodRefreshKey + "\n" +
		"catalog-url: https://catalog.test/models\n"
)

// authRecorder answers host.http.do, records every Authorization header, and
// rejects the cooled first key 401-style while other keys get a good catalog
// body unless statusCode overrides it.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
	times []time.Time
	logs  [][]byte
	// body, when non-empty, replaces testCatalogJSON as the served catalog.
	body        []byte
	statusCode  int
	statusCodes []int
}

func (r *authRecorder) call(method string, payload []byte) ([]byte, error) {
	if method == pluginabi.MethodHostLog {
		r.mu.Lock()
		r.logs = append(r.logs, append([]byte(nil), payload...))
		r.mu.Unlock()
		return hostOK(map[string]any{}), nil
	}
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	var wire struct {
		Headers http.Header `json:"headers"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return hostErr("test", "undecodable payload"), nil
	}
	auth := wire.Headers.Get("Authorization")
	r.mu.Lock()
	callIndex := len(r.auths)
	r.auths = append(r.auths, auth)
	r.times = append(r.times, time.Now())
	statusCode := r.statusCode
	if callIndex < len(r.statusCodes) {
		statusCode = r.statusCodes[callIndex]
	}
	r.mu.Unlock()
	if auth == "Bearer "+cooledRefreshKey {
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
	}
	if statusCode != 0 {
		return hostOK(pluginapi.HTTPResponse{StatusCode: statusCode}), nil
	}
	body := testCatalogJSON
	if len(r.body) > 0 {
		body = string(r.body)
	}
	return hostOK(pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}), nil
}

func (r *authRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *authRecorder) logCalls() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.logs...)
}

func (r *authRecorder) callTimes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.times...)
}

func twoKeyFixture(t *testing.T) (config.Config, *catalog.Manager, *HostBridge, *authRecorder) {
	t.Helper()
	cfg, err := config.Load([]byte(refreshTwoKeyYAML))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	bridge := NewHostBridge(rec.call)
	return cfg, catalog.New(cfg, bridge), bridge, rec
}

// TestRefreshTriesAnotherConfiguredKeyAfterAuthenticationFailure covers the
// shared catalog-discovery failover. It must not expose key material in logs.
func TestRefreshTriesAnotherConfiguredKeyAfterAuthenticationFailure(t *testing.T) {
	_, mgr, bridge, rec := twoKeyFixture(t)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second); err != nil {
		t.Fatalf("refresh after trying next key: %v", err)
	}
	auths := rec.seen()
	if len(auths) != 2 || auths[0] != "Bearer "+cooledRefreshKey || auths[1] != "Bearer "+goodRefreshKey {
		t.Fatalf("refresh auths = %v; want first-key rejection followed by second-key success", auths)
	}
	if len(mgr.Models()) == 0 {
		t.Fatal("successful fallback did not publish catalog models")
	}
	sawRedactedRecovery := false
	for _, log := range rec.logCalls() {
		if strings.Contains(string(log), cooledRefreshKey) || strings.Contains(string(log), goodRefreshKey) {
			t.Fatalf("catalog refresh log leaked key material: %s", log)
		}
		if strings.Contains(string(log), "catalog refresh succeeded with a later configured key") {
			sawRedactedRecovery = true
		}
	}
	if !sawRedactedRecovery {
		t.Fatal("catalog failover logs did not report recovery")
	}
}

func TestRefreshDoesNotTryAnotherKeyAfterNonAuthenticationFailure(t *testing.T) {
	const yaml = "api-keys:\n  - value: sk-refresh-first\n  - value: sk-refresh-second\n" +
		"catalog-url: https://catalog.test/models\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{statusCode: http.StatusServiceUnavailable}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second); err == nil || !strings.Contains(err.Error(), "http 503") {
		t.Fatalf("refresh error = %v, want HTTP 503", err)
	}
	if auths := rec.seen(); len(auths) != 1 || auths[0] != "Bearer sk-refresh-first" {
		t.Fatalf("refresh auths = %v; non-auth failure must not fan out to later keys", auths)
	}
	if len(mgr.Models()) != 0 {
		t.Fatalf("failed refresh changed catalog: %+v", mgr.Models())
	}
	logs := rec.logCalls()
	if len(logs) != 1 {
		t.Fatalf("host.log calls = %d, want one empty-catalog diagnostic", len(logs))
	}
	var log struct {
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(logs[0], &log); err != nil {
		t.Fatalf("decode host.log payload: %v", err)
	}
	if log.Message != "catalog refresh failed; no routable models available" || log.Fields["stale_snapshot"] != false {
		t.Fatalf("empty-catalog diagnostic = %+v, want explicit no-models/stale=false state", log)
	}
}

func TestFailedInitialRefreshSchedulesEarlyRetryAndRestoresNormalInterval(t *testing.T) {
	oldInitial, oldMax := catalogRetryInitialDelay, catalogRetryMaxDelay
	catalogRetryInitialDelay, catalogRetryMaxDelay = 5*time.Millisecond, 20*time.Millisecond
	defer func() { catalogRetryInitialDelay, catalogRetryMaxDelay = oldInitial, oldMax }()

	const yaml = "api-keys:\n  - value: sk-refresh-retry-success\n" +
		"catalog-url: https://catalog.test/models\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	bridge := NewHostBridge(rec.call)
	m := NewManager(bridge)
	mgr := catalog.New(cfg, bridge)
	stop, done := make(chan struct{}), make(chan struct{})
	m.startRefreshLoop(cfg, mgr, time.Hour, stop, done, true)
	defer func() {
		close(stop)
		<-done
	}()
	waitFor(t, "early retry after failed initial refresh", func() bool { return len(mgr.Models()) > 0 })
	if len(rec.seen()) != 1 {
		t.Fatalf("early retry count = %d, want exactly one refresh", len(rec.seen()))
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(rec.seen()); got != 1 {
		t.Fatalf("refresh calls after successful retry = %d, want 1 before normal interval", got)
	}
}

func TestBackgroundRefreshFailureRetriesBeforeNormalInterval(t *testing.T) {
	oldInitial, oldMax := catalogRetryInitialDelay, catalogRetryMaxDelay
	catalogRetryInitialDelay, catalogRetryMaxDelay = 5*time.Millisecond, 20*time.Millisecond
	defer func() { catalogRetryInitialDelay, catalogRetryMaxDelay = oldInitial, oldMax }()

	const yaml = "api-keys:\n  - value: sk-refresh-retry-after-tick\n" +
		"catalog-url: https://catalog.test/models\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{statusCodes: []int{http.StatusServiceUnavailable, 0}}
	bridge := NewHostBridge(rec.call)
	m := NewManager(bridge)
	mgr := catalog.New(cfg, bridge)
	stop, done := make(chan struct{}), make(chan struct{})
	m.startRefreshLoop(cfg, mgr, time.Second, stop, done, false)
	defer func() {
		close(stop)
		<-done
	}()
	waitFor(t, "successful retry after a background refresh failure", func() bool {
		return len(rec.seen()) == 2 && len(mgr.Models()) > 0
	})
	callTimes := rec.callTimes()
	if len(callTimes) < 2 {
		t.Fatalf("refresh call timestamps = %v, want two calls", callTimes)
	}
	if delay := callTimes[1].Sub(callTimes[0]); delay >= 500*time.Millisecond {
		t.Fatalf("retry delay after refresh failure = %s, want less than configured 1s interval", delay)
	}
}

func TestNextCatalogRetryDelayUsesExponentialBackoffAndCap(t *testing.T) {
	oldInitial, oldMax := catalogRetryInitialDelay, catalogRetryMaxDelay
	catalogRetryInitialDelay, catalogRetryMaxDelay = 5*time.Second, 30*time.Second
	defer func() { catalogRetryInitialDelay, catalogRetryMaxDelay = oldInitial, oldMax }()

	for _, tt := range []struct {
		current time.Duration
		want    time.Duration
	}{
		{current: 0, want: 5 * time.Second},
		{current: 5 * time.Second, want: 10 * time.Second},
		{current: 10 * time.Second, want: 20 * time.Second},
		{current: 20 * time.Second, want: 30 * time.Second},
		{current: 30 * time.Second, want: 30 * time.Second},
	} {
		if got := nextCatalogRetryDelay(tt.current); got != tt.want {
			t.Errorf("nextCatalogRetryDelay(%s) = %s, want %s", tt.current, got, tt.want)
		}
	}
}

// TestRefreshUnsupportedLogQuotesNewlineIDs pins F2: a catalog entry whose
// upstream ID carries control characters must reach the host.log "models"
// field quoted (%q), so no raw newline can forge log lines (CWE-117).
func TestRefreshUnsupportedLogQuotesNewlineIDs(t *testing.T) {
	const evilID = "evil\n2027-01-01 ERROR forged host.log line"
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-refresh-evil-key\n" +
		"catalog-url: https://catalog.test/models\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(`{"data":[{"id":"evil\n2027-01-01 ERROR forged host.log line"}]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logCalls := rec.logCalls()
	if len(logCalls) != 1 {
		t.Fatalf("host.log calls = %d, want exactly one unsupported-models warn", len(logCalls))
	}
	var req struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(logCalls[0], &req); err != nil {
		t.Fatalf("decode host.log payload: %v", err)
	}
	if req.Message != "unsupported models excluded from routable catalog" {
		t.Fatalf("unexpected log message: %q", req.Message)
	}
	modelsField, _ := req.Fields["models"].(string)
	if strings.ContainsRune(modelsField, '\n') {
		t.Fatalf("models log field contains raw newline (log injection): %q", modelsField)
	}
	if want := fmt.Sprintf("%q: ", evilID); !strings.Contains(modelsField, want) {
		t.Fatalf("models field must quote the upstream ID; got %q, want prefix %q", modelsField, want)
	}
}
