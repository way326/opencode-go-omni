package plugin

import (
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
)

func revisionModel() pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                        "opencode-go/glm-5.3",
		Object:                    "model",
		OwnedBy:                   ProviderID,
		DisplayName:               "GLM 5.3",
		ContextLength:             200000,
		MaxCompletionTokens:       32768,
		SupportedInputModalities:  []string{"image", "text"},
		SupportedOutputModalities: []string{"text"},
		Thinking: &pluginapi.ThinkingSupport{
			Min: 1024, Max: 32768, ZeroAllowed: true, DynamicAllowed: true,
			Levels: []string{"low", "high", "medium"},
		},
	}
}

func mustCatalogRevision(t *testing.T, models []pluginapi.ModelInfo) string {
	t.Helper()
	revision, err := catalogRevision(models)
	if err != nil {
		t.Fatalf("catalogRevision: %v", err)
	}
	if len(revision) != len("sha256:v1:")+64 || revision[:len("sha256:v1:")] != "sha256:v1:" {
		t.Fatalf("revision = %q, want sha256:v1:<64 hex chars>", revision)
	}
	return revision
}

func TestHandleModelsUsesPublishedModelProjection(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))); !env.OK {
		t.Fatalf("register failed: %v", env.Error)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelStatic, nil), &static)
	if want := m.servedModelInfos(); !reflect.DeepEqual(static.Models, want) {
		t.Fatalf("model.static = %#v, want published projection %#v", static.Models, want)
	}
	var forAuth pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelForAuth, []byte("{}")), &forAuth)
	if !reflect.DeepEqual(forAuth.Models, static.Models) {
		t.Fatalf("model.for_auth = %#v, want model.static projection %#v", forAuth.Models, static.Models)
	}
	if got := mustCatalogRevision(t, static.Models); got == mustCatalogRevision(t, nil) {
		t.Fatalf("non-empty model set hashed as empty projection: %s", got)
	}
}

func TestCatalogRevisionCanonicalizesOrderAndSets(t *testing.T) {
	first := revisionModel()
	second := revisionModel()
	second.ID = "opencode-go/minimax-m3"
	second.DisplayName = "MiniMax M3"

	ordered := mustCatalogRevision(t, []pluginapi.ModelInfo{first, second})
	reversed := mustCatalogRevision(t, []pluginapi.ModelInfo{second, first})
	if ordered != reversed {
		t.Fatalf("provider order changed revision: %s != %s", ordered, reversed)
	}

	canonical := revisionModel()
	reordered := revisionModel()
	reordered.SupportedInputModalities = []string{"text", "image", "text"}
	if got, want := mustCatalogRevision(t, []pluginapi.ModelInfo{reordered}), mustCatalogRevision(t, []pluginapi.ModelInfo{canonical}); got != want {
		t.Fatalf("modality order/duplicates changed revision: %s != %s", got, want)
	}
	levelsReordered := revisionModel()
	levelsReordered.Thinking.Levels = []string{"high", "low", "medium"}
	if got, want := mustCatalogRevision(t, []pluginapi.ModelInfo{levelsReordered}), mustCatalogRevision(t, []pluginapi.ModelInfo{canonical}); got == want {
		t.Fatal("thinking level order was treated as an unordered set")
	}

	nilSlices := revisionModel()
	nilSlices.SupportedInputModalities = nil
	nilSlices.SupportedOutputModalities = nil
	nilSlices.Thinking = &pluginapi.ThinkingSupport{Min: 1024, Max: 32768, ZeroAllowed: true, DynamicAllowed: true}
	emptySlices := nilSlices
	emptySlices.SupportedInputModalities = []string{}
	emptySlices.SupportedOutputModalities = []string{}
	emptySlices.Thinking = &pluginapi.ThinkingSupport{Min: 1024, Max: 32768, ZeroAllowed: true, DynamicAllowed: true, Levels: []string{}}
	if got, want := mustCatalogRevision(t, []pluginapi.ModelInfo{nilSlices}), mustCatalogRevision(t, []pluginapi.ModelInfo{emptySlices}); got != want {
		t.Fatalf("nil/empty slices changed revision: %s != %s", got, want)
	}

	unknownThinking := revisionModel()
	unknownThinking.Thinking = nil
	knownZeroThinking := revisionModel()
	knownZeroThinking.Thinking = &pluginapi.ThinkingSupport{}
	if got, want := mustCatalogRevision(t, []pluginapi.ModelInfo{unknownThinking}), mustCatalogRevision(t, []pluginapi.ModelInfo{knownZeroThinking}); got == want {
		t.Fatal("nil thinking and explicitly present zero thinking hashed identically")
	}
}

func TestCatalogRevisionCoversPublishedFields(t *testing.T) {
	base := revisionModel()
	baseRevision := mustCatalogRevision(t, []pluginapi.ModelInfo{base})
	changes := map[string]func(*pluginapi.ModelInfo){
		"id":                    func(m *pluginapi.ModelInfo) { m.ID = "opencode-go/glm-5.3-next" },
		"display name":          func(m *pluginapi.ModelInfo) { m.DisplayName = "GLM 5.3 Next" },
		"context length":        func(m *pluginapi.ModelInfo) { m.ContextLength++ },
		"max completion tokens": func(m *pluginapi.ModelInfo) { m.MaxCompletionTokens++ },
		"input modalities":      func(m *pluginapi.ModelInfo) { m.SupportedInputModalities = append(m.SupportedInputModalities, "audio") },
		"output modalities": func(m *pluginapi.ModelInfo) {
			m.SupportedOutputModalities = append(m.SupportedOutputModalities, "image")
		},
		"thinking minimum": func(m *pluginapi.ModelInfo) { m.Thinking.Min++ },
		"thinking maximum": func(m *pluginapi.ModelInfo) { m.Thinking.Max++ },
		"thinking zero":    func(m *pluginapi.ModelInfo) { m.Thinking.ZeroAllowed = !m.Thinking.ZeroAllowed },
		"thinking dynamic": func(m *pluginapi.ModelInfo) { m.Thinking.DynamicAllowed = !m.Thinking.DynamicAllowed },
		"thinking levels":  func(m *pluginapi.ModelInfo) { m.Thinking.Levels = []string{"low", "medium"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := revisionModel()
			change(&changed)
			if got := mustCatalogRevision(t, []pluginapi.ModelInfo{changed}); got == baseRevision {
				t.Fatalf("%s change did not change revision", name)
			}
		})
	}
}

func TestCatalogRevisionIgnoresExecutorOnlyFields(t *testing.T) {
	base := catalog.ModelRecord{
		PublicID:     "opencode-go/glm-5.3",
		UpstreamID:   "glm-5.3",
		DisplayName:  "GLM 5.3",
		Protocol:     catalog.RouteChatCompletions,
		EndpointPath: "/v1/chat/completions",
		ContextLimit: 200000,
		OutputLimit:  32768,
		InputModes:   []string{"text"},
		OutputModes:  []string{"text"},
	}
	changed := base
	changed.UpstreamID = "internal/renamed-upstream"
	changed.Protocol = catalog.RouteMessages
	changed.EndpointPath = "/v1/messages"

	baseRevision := mustCatalogRevision(t, publishedModelInfos([]catalog.ModelRecord{base}))
	changedRevision := mustCatalogRevision(t, publishedModelInfos([]catalog.ModelRecord{changed}))
	if baseRevision != changedRevision {
		t.Fatalf("executor-only fields changed registry revision: %s != %s", baseRevision, changedRevision)
	}
}

func TestRevisionTracksFailClosedClear(t *testing.T) {
	var succeed atomic.Bool
	succeed.Store(true)
	m, _ := newTestManager(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			if !succeed.Load() {
				return hostErr("upstream_down", "simulated failure"), nil
			}
		}
		return catalogResponder(true, testCatalogJSON)(method, payload)
	})
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })

	yamlText := testValidYAML + "catalog:\n  stale-while-unavailable: false\n"
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(yamlText))); !env.OK {
		t.Fatalf("register failed: %v", env.Error)
	}
	before := mustCatalogRevision(t, m.servedModelInfos())

	succeed.Store(false)
	m.mu.RLock()
	mgr, cfg := m.mgr, m.cfg
	m.mu.RUnlock()
	if err := refreshOnce(t.Context(), mgr, m.bridge, time.Second, cfg); err == nil {
		t.Fatal("fail-closed refresh unexpectedly succeeded")
	}
	models := m.servedModelInfos()
	if len(models) != 0 {
		t.Fatalf("fail-closed projection = %+v, want empty", models)
	}
	if after := mustCatalogRevision(t, models); before == after {
		t.Fatalf("fail-closed clear kept revision %s", before)
	}
}

func TestRevisionTracksReconfigureSeededProjection(t *testing.T) {
	var succeed atomic.Bool
	succeed.Store(true)
	m, _ := newTestManager(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo && !succeed.Load() {
			return hostErr("upstream_down", "simulated failure"), nil
		}
		return catalogResponder(true, testCatalogJSON)(method, payload)
	})
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })

	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))); !env.OK {
		t.Fatalf("register failed: %v", env.Error)
	}
	beforeModels := m.servedModelInfos()
	beforeRevision := mustCatalogRevision(t, beforeModels)
	if got := beforeModels[0].ID; got != "opencode-go/glm-5.3" {
		t.Fatalf("initial public ID = %q", got)
	}

	succeed.Store(false)
	noPrefix := testValidYAML + "model-prefix:\n  enabled: false\n"
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(noPrefix))); !env.OK {
		t.Fatalf("reconfigure failed: %v", env.Error)
	}
	afterModels := m.servedModelInfos()
	if len(afterModels) != 1 || afterModels[0].ID != "glm-5.3" {
		t.Fatalf("seeded projection = %+v, want unprefixed glm-5.3", afterModels)
	}
	if after := mustCatalogRevision(t, afterModels); after == beforeRevision {
		t.Fatalf("seeded projection kept stale revision %s", beforeRevision)
	}
}
