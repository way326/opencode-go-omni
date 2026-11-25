package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/config"
	"github.com/way326/opencode-go-omni/resources"
)

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	if len(got.Routes) != 1 || got.Routes[0].Method != http.MethodPost || got.Routes[0].Path != "/plugins/"+pluginName+"/quota-usage" {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 1 || got.Resources[0].Path != "/quota" || got.Resources[0].Menu != "OpenCode Go Quota" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
}

func TestQuotaListDoesNotCallHost(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	installQuotaTestConfig(t, m, config.Config{APIKeys: []config.APIKey{{Value: "quota-key-a"}, {Value: "quota-key-b"}}})
	var got quotaList
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-a") || got.Cards[1].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-b") {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if strings.Contains(string(resp.Body), "quota-key-") || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("list leaked key or called upstream: %s calls=%v", resp.Body, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
}

func TestQuotaRefresh(t *testing.T) {
	const key = "quota-refresh-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet || wire.URL != "https://quota.test/v1/usage" || wire.Headers.Get("Authorization") != "Bearer "+key || wire.Headers.Get("Accept") != "application/json" {
			t.Fatalf("bad quota request: %s", payload)
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"rolling":{"status":"ok","percent":9,"resetsAt":"2026-09-05T12:20:04Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-09-10T00:00:00Z"},"monthly":{"status":"ok","percent":6,"resetsAt":"2026-10-01T00:00:00Z"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	installQuotaTestConfig(t, m, config.Config{BaseURL: "https://quota.test/v1/", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}})
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaCard
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.Rolling.Percent != 9 || got.Usage.Weekly.ResetsAt != "2026-09-10T00:00:00Z" || got.Usage.Monthly.Percent != 6 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestQuotaUnknownKeyAndResource(t *testing.T) {
	const key = "quota-known-secret"
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	installQuotaTestConfig(t, m, config.Config{APIKeys: []config.APIKey{{Value: key}}})
	unknown := "opencode-go-key-unknown"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + unknown + `"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), unknown) || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("unknown response = %+v err=%v calls=%v", resp, err, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota"})
	if err != nil || resource.StatusCode != 0 || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resource.Body), "OpenCode Go Quota") || strings.Contains(string(resource.Body), key) {
		t.Fatalf("resource response = %+v err=%v", resource, err)
	}
}

func TestQuotaPageReadsRememberedManagementKey(t *testing.T) {
	if !strings.Contains(resources.QuotaPage, "parsed.state && parsed.state.managementKey") {
		t.Fatal("quota page does not read persisted state.managementKey")
	}
}

func TestQuotaPageDecodesCurrentCPAAuthStorage(t *testing.T) {
	for _, marker := range []string{"enc::v1::", "cli-proxy-api-webui::secure-storage", "TextDecoder"} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page does not decode current CPA auth storage: missing %q", marker)
		}
	}
}

func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	id, _ := quotaIdentity(key)
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: responder}
			m := NewManager(NewHostBridge(f.call))
			installQuotaTestConfig(t, m, config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}})
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
			if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), "upstream body") {
				t.Fatalf("response = %+v err=%v", resp, err)
			}
		})
	}
}

func installQuotaTestConfig(t *testing.T, m *Manager, cfg config.Config) {
	t.Helper()
	directory, err := buildCredentialDirectory(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.cfg = cfg
	m.credentialDirectory = directory
}

func TestQuotaPageIsStaticAndSecretFree(t *testing.T) {
	if resources.QuotaPage == "" || strings.Contains(resources.QuotaPage, testKey) || strings.Contains(resources.QuotaPage, "setInterval") || strings.Contains(resources.QuotaPage, "reset button") {
		t.Fatal("quota page contains a secret, polling, or reset action")
	}
	if !strings.Contains(resources.QuotaPage, "textContent") || !strings.Contains(resources.QuotaPage, "Remember password") {
		t.Fatal("quota page missing safe rendering or login guidance")
	}
}

func TestQuotaPageUsesManualSessionCache(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`const storageKey = "opencode-go-omni:quota"`,
		"sessionStorage.getItem(storageKey)",
		"sessionStorage.setItem(storageKey, JSON.stringify(cache))",
		"JSON.parse(stored)",
		"typeof parsed === \"object\"",
		"Array.isArray(parsed)",
		"catch (_) {}",
		"Object.entries(cache)",
		"values.set(keyID, entry.usage)",
		"cache[card.key_id] = {usage: result.usage, fetched_at: timestamp}",
		"delete cache[keyID]",
		"new Set(cards.map(card => card.key_id))",
		"toLocaleString",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page missing session cache marker %q", marker)
		}
	}
	for _, marker := range []string{
		"visibilitychange",
		"document.hidden",
		"window.onfocus",
		"window.addEventListener(\"focus\"",
		"TTL",
		"expiry",
		"expiration",
		"bulk refresh",
	} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page contains prohibited automatic or expiry behavior %q", marker)
		}
	}
}

func TestQuotaPageUsesNativeQuotaStylesAndThemeBridge(t *testing.T) {
	for _, marker := range []string{
		"quota-page",
		"quota-grid",
		"quota-card",
		"quota-track",
		"document.createElement(\"progress\")",
		"::-webkit-progress-value",
		"::-moz-progress-bar",
		"quota-track.quota-warning",
		"quota-track.quota-critical",
		`percent >= 95`,
		`percent >= 80`,
		`labelText.replace(/^OpenCode Go credential /, "")`,
		"function credentialAccessibleName(label)",
		`label.replace(/^OpenCode Go credential /, "OpenCode Go 凭据 ")`,
		`translate("refreshCredential") + " " + credentialAccessibleName(labelText)`,
		`"zh-CN": {`,
		`refreshAll: "全部刷新"`,
		`rolling: "滚动窗口"`,
		`weekly: "每周"`,
		`monthly: "每月"`,
		`resets: "重置时间"`,
		`pageTitle: "OpenCode Go 额度"`,
		`document.documentElement.lang`,
		`parentRoot.getAttribute("lang")`,
		`attributeFilter: ["data-theme", "lang"]`,
		`window.dispatchEvent(new Event("cpa-language-change"))`,
		`window.addEventListener("cpa-language-change", applyLanguage)`,
		`language.startsWith("zh") ? "zh-CN" : "en"`,
		`date.toLocaleString(locale,`,
		"quota-eyebrow",
		"quota-periods",
		"quota-status",
		"quota-updated",
		"repeat(auto-fit, minmax(min(100%, 380px), 1fr))",
		"@media (max-width: 600px)",
		"grid-template-columns: minmax(0, 1fr)",
		"prefers-reduced-motion: reduce",
		`[data-theme="white"]`,
		`[data-theme="dark"]`,
		`window.parent.document`,
		`data-theme`,
		`--bg-secondary`,
		`frameElement.style.backgroundColor`,
		`frameElement.parentElement.style.backgroundColor`,
		".quota-refresh",
		"--bg-secondary: #f5f7fa",
		"--bg-primary: #fff",
		"--bg-tertiary: #edf1f5",
		"--text-primary: #1e2937",
		"--text-secondary: #627084",
		"--text-tertiary: #8793a3",
		"--border-color: #e0e6ed",
		"--primary-color: #4d63d9",
		"--primary-hover: #4054c2",
		"aria-valuetext",
		"formatTimestamp",
		`resetsAt.textContent = translate("resets") + " "`,
		`class="quota-btn quota-btn-primary"`,
		`MutationObserver`,
		`catch (_) {}`,
	} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page missing styling marker %q", marker)
		}
	}
	if strings.Contains(resources.QuotaPage, `textContent = "Refresh card"`) || strings.Contains(resources.QuotaPage, "quota-button") || strings.Contains(resources.QuotaPage, "quota-refresh-small") {
		t.Fatal("quota page does not have exactly one secondary refresh button path")
	}
	for _, unsafeMarker := range []string{"innerHTML", "insertAdjacentHTML", "fill.style.width", "track.style", "track.style.setProperty"} {
		if strings.Contains(resources.QuotaPage, unsafeMarker) {
			t.Fatalf("quota page uses unsafe dynamic rendering behavior %q", unsafeMarker)
		}
	}
	for _, marker := range []string{"querySelectorAll('head link[rel=\"stylesheet\"], head style')", "cloneNode(true)", "dataset.cpaStyle"} {
		if strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page still clones parent styles: %q", marker)
		}
	}
}

func mustHashPrefix(key string) string {
	_, label := quotaIdentity(key)
	return strings.TrimPrefix(label, "OpenCode Go credential ")
}
