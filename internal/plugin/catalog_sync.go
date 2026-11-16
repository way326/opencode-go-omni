package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	catalogRevisionAuthField = "opencode_go_catalog_revision"
	catalogAuthGetTimeout    = 5 * time.Second
)

// catalogRevisionSyncer asks CPA's existing auth watcher to rebuild the model
// registry by persisting the served projection revision into plugin auth files.
// The mutex serializes the one full list/get/save batch owned by a Manager; it
// is not a queue and there is deliberately no per-auth worker state.
type catalogRevisionSyncer struct {
	bridge *HostBridge
	mu     sync.Mutex
	// pending contains physical paths whose AuthSave failed or had an unknown
	// result. It suppresses the disk-revision shortcut until a later save succeeds.
	pending map[string]struct{}
}

type catalogSyncStats struct {
	Candidates int
	Saved      int
	Unchanged  int
	Failed     int
}

func newCatalogRevisionSyncer(bridge *HostBridge) *catalogRevisionSyncer {
	return &catalogRevisionSyncer{bridge: bridge, pending: make(map[string]struct{})}
}

// syncRevision writes revision to every physical Auth owned by this plugin.
// A failed entry is retried on the next tick even when the file on disk already
// contains the same revision, because the previous save result is unknown.
func (s *catalogRevisionSyncer) syncRevision(ctx context.Context, revision string) (catalogSyncStats, error) {
	var stats catalogSyncStats
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return stats, fmt.Errorf("catalog revision is empty")
	}
	if s == nil || s.bridge == nil {
		return stats, fmt.Errorf("host bridge is unavailable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.bridge.AuthList(ctx)
	if err != nil {
		_ = s.bridge.Log("warn", "catalog sync partial failure", map[string]any{
			"stage": "auth_list",
		})
		return stats, err
	}
	targets := catalogSyncTargets(entries)
	stats.Candidates = len(targets)

	nextPending := make(map[string]struct{})
	for _, target := range targets {
		if _, ok := s.pending[target.pathKey]; ok {
			nextPending[target.pathKey] = struct{}{}
		}
	}

	for _, target := range targets {
		getCtx, cancel := context.WithTimeout(ctx, catalogAuthGetTimeout)
		auth, err := s.bridge.AuthGet(getCtx, target.entry.AuthIndex)
		cancel()
		if err != nil {
			nextPending[target.pathKey] = struct{}{}
			stats.Failed++
			continue
		}
		merged, unchanged, err := setCatalogRevision(auth.JSON, revision)
		if err != nil {
			nextPending[target.pathKey] = struct{}{}
			stats.Failed++
			continue
		}
		_, wasPending := nextPending[target.pathKey]
		if unchanged && !wasPending {
			stats.Unchanged++
			continue
		}
		if strings.TrimSpace(auth.Name) == "" {
			nextPending[target.pathKey] = struct{}{}
			stats.Failed++
			continue
		}
		if err := s.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: auth.Name, JSON: merged}); err != nil {
			nextPending[target.pathKey] = struct{}{}
			stats.Failed++
			continue
		}
		delete(nextPending, target.pathKey)
		stats.Saved++
	}

	s.pending = nextPending
	fields := map[string]any{
		"candidates": stats.Candidates,
		"saved":      stats.Saved,
		"unchanged":  stats.Unchanged,
		"failed":     stats.Failed,
	}
	switch {
	case stats.Failed > 0:
		_ = s.bridge.Log("warn", "catalog sync partial failure", fields)
	case stats.Saved > 0:
		_ = s.bridge.Log("info", "catalog sync requested", fields)
	case stats.Candidates > 0:
		_ = s.bridge.Log("info", "catalog sync unchanged", fields)
	}
	return stats, nil
}

type catalogSyncTarget struct {
	entry   pluginapi.HostAuthFileEntry
	pathKey string
}

// catalogSyncTargets keeps one representative per physical auth file. Multiple
// runtime AuthIndexes can point at the same JSON file; writing that file more
// than once in a batch has no benefit.
func catalogSyncTargets(entries []pluginapi.HostAuthFileEntry) []catalogSyncTarget {
	targets := make([]catalogSyncTarget, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.AuthIndex) == "" || strings.TrimSpace(entry.Path) == "" || entry.RuntimeOnly {
			continue
		}
		if !isOpenCodeGoAuth(entry) {
			continue
		}
		pathKey := normalizeAuthPath(entry.Path)
		if pathKey == "" {
			continue
		}
		if _, ok := seen[pathKey]; ok {
			continue
		}
		seen[pathKey] = struct{}{}
		targets = append(targets, catalogSyncTarget{entry: entry, pathKey: pathKey})
	}
	return targets
}

func isOpenCodeGoAuth(entry pluginapi.HostAuthFileEntry) bool {
	matched := false
	for _, provider := range []string{entry.Type, entry.Provider} {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		if !strings.EqualFold(provider, ProviderID) {
			return false
		}
		matched = true
	}
	return matched
}

func normalizeAuthPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// setCatalogRevision edits only the plugin-owned revision key. Other raw JSON
// members are carried through unchanged at the value level.
func setCatalogRevision(raw []byte, revision string) ([]byte, bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false, fmt.Errorf("auth json is not an object")
	}
	if object == nil {
		return nil, false, fmt.Errorf("auth json is not an object")
	}
	if current, ok := object[catalogRevisionAuthField]; ok {
		var value string
		if json.Unmarshal(current, &value) == nil && value == revision {
			return raw, true, nil
		}
	}
	encodedRevision, _ := json.Marshal(revision)
	object[catalogRevisionAuthField] = encodedRevision
	merged, err := json.Marshal(object)
	if err != nil {
		return nil, false, fmt.Errorf("auth json could not be encoded")
	}
	return merged, false, nil
}
