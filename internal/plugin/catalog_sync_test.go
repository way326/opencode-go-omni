package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBridgeAuthGetWireAndRedactsFailures(t *testing.T) {
	secret := "credential-bridge-secret"
	var wire pluginapi.HostAuthGetRequest
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthGet {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthGet)
		}
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("payload: %v", err)
		}
		return hostOK(pluginapi.HostAuthGetResponse{
			AuthIndex: "auth-a",
			Name:      "a.json",
			Path:      `C:\auth\a.json`,
			JSON:      json.RawMessage(`{"type":"opencode-go","api_key":"` + secret + `"}`),
		}), nil
	})
	got, err := bridge.AuthGet(context.Background(), " auth-a ")
	if err != nil {
		t.Fatalf("AuthGet: %v", err)
	}
	if wire.AuthIndex != "auth-a" || got.Name != "a.json" || !strings.Contains(string(got.JSON), secret) {
		t.Fatalf("wire=%+v response=%+v", wire, got)
	}

	_, err = NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostErr("denied", "host echoed "+secret), nil
	}).AuthGet(context.Background(), "auth-a")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "host auth get failed") {
		t.Fatalf("redacted AuthGet error = %v", err)
	}

	_, err = NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: "auth-a", Name: "a.json"}), nil
	}).AuthGet(context.Background(), "auth-a")
	if err == nil || !strings.Contains(err.Error(), "missing auth json") {
		t.Fatalf("missing JSON error = %v", err)
	}
}

func TestBridgeAuthGetTimeout(t *testing.T) {
	release := make(chan struct{})
	bridge := NewHostBridge(func(string, []byte) ([]byte, error) {
		<-release
		return hostOK(pluginapi.HostAuthGetResponse{
			AuthIndex: "auth-a", Name: "a.json", JSON: json.RawMessage(`{"type":"opencode-go"}`),
		}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := bridge.AuthGet(ctx, "auth-a")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("AuthGet timeout = %v", err)
	}
	close(release)
	if !bridge.WaitForInFlight(time.Second) {
		t.Fatal("AuthGet timeout callback did not drain")
	}
}

func TestBridgeAuthListRedactsHostError(t *testing.T) {
	secret := "credential-list-secret"
	_, err := NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostErr("denied", "host echoed "+secret), nil
	}).AuthList(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "host auth list failed") {
		t.Fatalf("redacted AuthList error = %v", err)
	}
}

func TestBridgeAuthSaveWaitsAfterStart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			t.Fatalf("method = %q", method)
		}
		close(started)
		<-release
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(payload, &req)
		return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name}), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: "a.json", JSON: json.RawMessage(`{}`)})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("AuthSave returned before started callback completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AuthSave: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AuthSave did not return after callback completed")
	}
}

func TestCatalogRevisionSyncFiltersDeduplicatesAndPreservesFields(t *testing.T) {
	pathA := filepath.Join("auth", "a.json")
	duplicatePathA := filepath.Join("auth", "sub", "..", "a.json")
	if runtime.GOOS == "windows" {
		duplicatePathA = strings.ToUpper(pathA)
	}
	pathB := filepath.Join("auth", "b.json")
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "a1", Type: ProviderID, Path: pathA, Name: "a.json"},
		{AuthIndex: "a2", Type: ProviderID, Path: duplicatePathA, Name: "a2.json"},
		{AuthIndex: "runtime", Type: ProviderID, Path: filepath.Join("auth", "runtime.json"), RuntimeOnly: true},
		{AuthIndex: "other", Type: "claude", Path: filepath.Join("auth", "other.json")},
		{AuthIndex: "nopath", Type: ProviderID, Name: "nopath.json"},
		{AuthIndex: "b", Provider: "OpenCode-Go", Path: pathB, Name: "b.json"},
	}, map[string][]byte{
		pathA: []byte(`{"type":"opencode-go","api_key":"secret-a","note":"keep","weight":2,"disabled":false,"proxy":"http://local"}`),
		pathB: []byte(`{"type":"opencode-go","api_key":"secret-b"}`),
	})
	syncer := newCatalogRevisionSyncer(NewHostBridge(host.call))
	stats, err := syncer.syncRevision(context.Background(), "sha256:v1:first")
	if err != nil {
		t.Fatalf("syncRevision: %v", err)
	}
	if stats.Candidates != 2 || stats.Saved != 2 || stats.Unchanged != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := len(host.gets()); got != 2 {
		t.Fatalf("AuthGet calls = %d, want 2", got)
	}
	saves := host.saves()
	if len(saves) != 2 {
		t.Fatalf("AuthSave calls = %d, want 2", len(saves))
	}
	for _, save := range saves {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(save.JSON, &object); err != nil {
			t.Fatalf("saved JSON: %v", err)
		}
		var revision string
		if err := json.Unmarshal(object[catalogRevisionAuthField], &revision); err != nil || revision != "sha256:v1:first" {
			t.Fatalf("saved revision = %q, err=%v", revision, err)
		}
		if save.Name == "a.json" {
			for _, key := range []string{"note", "weight", "disabled", "proxy", "api_key"} {
				if _, ok := object[key]; !ok {
					t.Fatalf("saved JSON lost %q: %s", key, save.JSON)
				}
			}
		}
	}
	stats, err = syncer.syncRevision(context.Background(), "sha256:v1:first")
	if err != nil || stats.Unchanged != 2 || len(host.gets()) != 4 || len(host.saves()) != 2 {
		t.Fatalf("same-revision sync stats=%+v gets=%d saves=%d err=%v", stats, len(host.gets()), len(host.saves()), err)
	}
	if logs := strings.Join(host.logMessages(), "\n"); strings.Contains(logs, "secret-a") || strings.Contains(logs, "secret-b") {
		t.Fatalf("credential leaked into logs: %s", logs)
	}
}

func TestCatalogRevisionSyncRetriesPendingSaveEvenWhenDiskMatches(t *testing.T) {
	path := `C:\auth\a.json`
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", Type: ProviderID, Path: path, Name: "a.json"},
	}, map[string][]byte{
		path: []byte(`{"type":"opencode-go","api_key":"secret-a","opencode_go_catalog_revision":"sha256:v1:first"}`),
	})
	syncer := newCatalogRevisionSyncer(NewHostBridge(host.call))
	stats, err := syncer.syncRevision(context.Background(), "sha256:v1:first")
	if err != nil || stats.Unchanged != 1 || stats.Saved != 0 || len(host.saves()) != 0 {
		t.Fatalf("unchanged sync stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}

	host.setSaveError(path, errors.New("ambiguous write result"))
	stats, err = syncer.syncRevision(context.Background(), "sha256:v1:second")
	if err != nil || stats.Failed != 1 || len(host.saves()) != 1 {
		t.Fatalf("failed sync stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
	if data := host.file(path); !strings.Contains(string(data), "sha256:v1:second") {
		t.Fatalf("failed save did not emulate an ambiguous disk write: %s", data)
	}

	host.setSaveError(path, nil)
	stats, err = syncer.syncRevision(context.Background(), "sha256:v1:second")
	if err != nil || stats.Saved != 1 || len(host.saves()) != 2 {
		t.Fatalf("pending retry stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
}

func TestCatalogRevisionSyncFindsNewAuthAtSameRevision(t *testing.T) {
	pathA := `C:\auth\a.json`
	pathB := `C:\auth\b.json`
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", Type: ProviderID, Path: pathA, Name: "a.json"},
	}, map[string][]byte{
		pathA: []byte(`{"type":"opencode-go","api_key":"secret-a","opencode_go_catalog_revision":"sha256:v1:same"}`),
	})
	syncer := newCatalogRevisionSyncer(NewHostBridge(host.call))
	stats, err := syncer.syncRevision(context.Background(), "sha256:v1:same")
	if err != nil || stats.Unchanged != 1 || len(host.saves()) != 0 {
		t.Fatalf("initial same-revision stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}

	host.addEntry(pluginapi.HostAuthFileEntry{AuthIndex: "b", Type: ProviderID, Path: pathB, Name: "b.json"}, []byte(`{"type":"opencode-go","api_key":"secret-b"}`))
	stats, err = syncer.syncRevision(context.Background(), "sha256:v1:same")
	if err != nil || stats.Unchanged != 1 || stats.Saved != 1 || len(host.saves()) != 1 {
		t.Fatalf("new-auth same-revision stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
}

func TestCatalogRevisionSyncContinuesAfterOneSaveFailure(t *testing.T) {
	pathA := `C:\auth\a.json`
	pathB := `C:\auth\b.json`
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", Type: ProviderID, Path: pathA, Name: "a.json"},
		{AuthIndex: "b", Type: ProviderID, Path: pathB, Name: "b.json"},
	}, map[string][]byte{
		pathA: []byte(`{"type":"opencode-go","api_key":"secret-a"}`),
		pathB: []byte(`{"type":"opencode-go","api_key":"secret-b"}`),
	})
	host.setSaveError(pathA, errors.New("host save failed"))
	stats, err := newCatalogRevisionSyncer(NewHostBridge(host.call)).syncRevision(context.Background(), "sha256:v1:batch")
	if err != nil || stats.Saved != 1 || stats.Failed != 1 || len(host.saves()) != 2 {
		t.Fatalf("partial sync stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
	if data := host.file(pathB); !strings.Contains(string(data), "sha256:v1:batch") {
		t.Fatalf("second auth was not saved: %s", data)
	}
}

func TestCatalogRevisionSyncRetriesAuthGetFailure(t *testing.T) {
	path := `C:\auth\a.json`
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "a", Type: ProviderID, Path: path, Name: "a.json"},
	}, map[string][]byte{
		path: []byte(`{"type":"opencode-go","api_key":"secret-a"}`),
	})
	host.setGetError("a", errors.New("temporary read failure"))
	syncer := newCatalogRevisionSyncer(NewHostBridge(host.call))
	stats, err := syncer.syncRevision(context.Background(), "sha256:v1:retry-get")
	if err != nil || stats.Failed != 1 || len(host.saves()) != 0 {
		t.Fatalf("failed get stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
	host.setGetError("a", nil)
	stats, err = syncer.syncRevision(context.Background(), "sha256:v1:retry-get")
	if err != nil || stats.Saved != 1 || len(host.saves()) != 1 {
		t.Fatalf("get retry stats=%+v saves=%d err=%v", stats, len(host.saves()), err)
	}
}

func TestManagerSyncsLifecycleAndTickerOnlyWhenRevisionChanges(t *testing.T) {
	path := `C:\auth\lifecycle.json`
	digest := sha256.Sum256([]byte(testKey))
	id := "opencode-go-key-" + hex.EncodeToString(digest[:])
	host := newCatalogSyncHost([]pluginapi.HostAuthFileEntry{
		{AuthIndex: "lifecycle", ID: id, Type: ProviderID, Path: path, Name: id + ".json"},
	}, map[string][]byte{
		path: []byte(`{"type":"opencode-go","id":"` + id + `","api_key":"` + testKey + `"}`),
	})
	host.setModelsBody([]byte(testCatalogJSON))
	m := NewManager(NewHostBridge(host.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("register response=%s err=%v", resp, err)
	}
	if got := len(host.saves()); got != 2 {
		t.Fatalf("initial AuthSave calls = %d, want migration and catalog revision", got)
	}
	if resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML)); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("same-revision reconfigure response=%s err=%v", resp, err)
	}
	if got := len(host.saves()); got != 2 {
		t.Fatalf("same-revision reconfigure AuthSave calls = %d, want no additional writes", got)
	}
	host.setModelsBody([]byte(`{"data":[{"id":"minimax-m3"}]}`))
	if resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML)); err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("changed-revision reconfigure response=%s err=%v", resp, err)
	}
	if got := len(host.saves()); got != 3 {
		t.Fatalf("changed-revision reconfigure AuthSave calls = %d, want 3", got)
	}

	installFastLoop(t, m, testValidYAML)
	time.Sleep(25 * time.Millisecond)
	if got := len(host.saves()); got != 3 {
		t.Fatalf("unchanged ticker AuthSave calls = %d, want 3", got)
	}
	host.setModelsBody([]byte(`{"data":[{"id":"glm-5.3"},{"id":"minimax-m3"}]}`))
	waitFor(t, "catalog revision sync after ticker change", func() bool {
		return len(host.saves()) == 4
	})
	saves := host.saves()
	latest := saves[3]
	var object map[string]json.RawMessage
	if err := json.Unmarshal(latest.JSON, &object); err != nil {
		t.Fatalf("ticker AuthSave JSON: %v", err)
	}
	var previousObject map[string]json.RawMessage
	if err := json.Unmarshal(saves[2].JSON, &previousObject); err != nil {
		t.Fatalf("previous AuthSave JSON: %v", err)
	}
	if len(object[catalogRevisionAuthField]) == 0 || string(object[catalogRevisionAuthField]) == string(previousObject[catalogRevisionAuthField]) {
		t.Fatalf("ticker did not write a changed revision: previous=%s latest=%s", saves[2].JSON, latest.JSON)
	}
}

type catalogSyncHost struct {
	mu      sync.Mutex
	entries []pluginapi.HostAuthFileEntry
	data    map[string][]byte
	saveErr map[string]error
	getErr  map[string]error
	saved   []pluginapi.HostAuthSaveRequest
	got     []string
	logs    []string
	models  []byte
}

func newCatalogSyncHost(entries []pluginapi.HostAuthFileEntry, data map[string][]byte) *catalogSyncHost {
	return &catalogSyncHost{
		entries: entries, data: data, saveErr: make(map[string]error), getErr: make(map[string]error),
	}
}

func (h *catalogSyncHost) call(method string, payload []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case pluginabi.MethodHostHTTPDo:
		if len(h.models) == 0 {
			return hostErr("missing_catalog", "catalog unavailable"), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: append([]byte(nil), h.models...)}), nil
	case pluginabi.MethodHostAuthList:
		return hostOK(hostAuthListResponse{Files: h.entries}), nil
	case pluginabi.MethodHostAuthGet:
		var req pluginapi.HostAuthGetRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		h.got = append(h.got, req.AuthIndex)
		for _, entry := range h.entries {
			if entry.AuthIndex == req.AuthIndex {
				if err := h.getErr[entry.AuthIndex]; err != nil {
					return nil, err
				}
				return hostOK(pluginapi.HostAuthGetResponse{
					AuthIndex: entry.AuthIndex,
					Name:      entry.Name,
					Path:      entry.Path,
					JSON:      append([]byte(nil), h.data[entry.Path]...),
				}), nil
			}
		}
		return hostErr("not_found", "missing auth"), nil
	case pluginabi.MethodHostAuthSave:
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		h.saved = append(h.saved, req)
		for _, entry := range h.entries {
			if entry.Name == req.Name {
				h.data[entry.Path] = append([]byte(nil), req.JSON...)
				if err := h.saveErr[entry.Path]; err != nil {
					return nil, err
				}
				return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: entry.Path}), nil
			}
		}
		return hostErr("not_found", "missing auth"), nil
	case pluginabi.MethodHostLog:
		var req hostLogReq
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		h.logs = append(h.logs, req.Message)
		return hostOK(map[string]any{}), nil
	default:
		return hostOK(map[string]any{}), nil
	}
}

func (h *catalogSyncHost) saves() []pluginapi.HostAuthSaveRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.HostAuthSaveRequest(nil), h.saved...)
}

func (h *catalogSyncHost) gets() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.got...)
}

func (h *catalogSyncHost) logMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

func (h *catalogSyncHost) setSaveError(path string, err error) {
	h.mu.Lock()
	h.saveErr[path] = err
	h.mu.Unlock()
}

func (h *catalogSyncHost) setGetError(index string, err error) {
	h.mu.Lock()
	h.getErr[index] = err
	h.mu.Unlock()
}

func (h *catalogSyncHost) file(path string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.data[path]...)
}

func (h *catalogSyncHost) setModelsBody(body []byte) {
	h.mu.Lock()
	h.models = append([]byte(nil), body...)
	h.mu.Unlock()
}

func (h *catalogSyncHost) addEntry(entry pluginapi.HostAuthFileEntry, data []byte) {
	h.mu.Lock()
	h.entries = append(h.entries, entry)
	h.data[entry.Path] = append([]byte(nil), data...)
	h.mu.Unlock()
}
