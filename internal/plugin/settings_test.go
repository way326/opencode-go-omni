package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
	"github.com/way326/opencode-go-omni/internal/config"
	"github.com/way326/opencode-go-omni/resources"
)

func TestCombinedManagementPageContainsSettingsViewAndSecurityHeaders(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v8/resource/plugins/" + pluginName + "/quota",
	})
	if err != nil {
		t.Fatalf("resource error: %v", err)
	}
	if resp.StatusCode != 0 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for name, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'self'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP missing %q: %q", directive, csp)
		}
	}
	match := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if len(match) != 2 {
		t.Fatalf("CSP missing script nonce: %q", csp)
	}
	body := string(resp.Body)
	if got := strings.Count(body, `<script nonce="`+match[1]+`">`); got != 2 {
		t.Fatalf("script nonce occurrences = %d, want 2", got)
	}
	if got := strings.Count(body, `<style nonce="`+match[1]+`">`); got != 1 {
		t.Fatalf("style nonce occurrences = %d, want 1", got)
	}
	for _, marker := range []string{
		"OpenCode Go Omni",
		"id=\"settings-btn\"",
		"id=\"settings-view\" hidden",
		"/v0/management/plugins/",
		"/v0/management/plugins/" + pluginName + "/catalog-diagnostics",
		"+ pluginID + \"/config\"",
		"request(configURL, \"GET\")",
		"request(configURL, \"PATCH\", patch)",
		"apiKeysChanged",
		"reveal.textContent = visible ? \"隐藏密钥\" : \"显示密钥\"",
		"remarkInput.value.trim() || defaultName",
		"apiKeys[index].remark = remarkInput.value",
		"const savedConfig = await request(configURL, \"GET\")",
		"CPA 接受了保存请求，但重新读取时备注字段不一致",
		"window.refreshQuotaCredentials(expectedRemarks)",
		"额度页名称会在重载完成后自动同步",
		"remove.textContent = \"从配置移除\"",
		"从本插件配置移除 Key",
		"不会删除 CPA 已生成的 Auth 记录，也不会撤销上游 Key",
		"若 CPA 仍选择旧记录，相关请求可能因未知凭据失败",
		"保存设置后才生效",
		"新增 Key",
		"cli-proxy-auth",
		"input.type = \"password\"",
		"CPA 的 config.yaml",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("combined Management Center page missing %q", marker)
		}
	}
	for _, placeholder := range []string{"__QUOTA_ENDPOINT__", "__DIAGNOSTICS_ENDPOINT__", "__SETTINGS_FORM__", "__SETTINGS_STYLES__", "__SETTINGS_SCRIPT__"} {
		if strings.Contains(body, placeholder) {
			t.Fatalf("quota/settings page left placeholder %q", placeholder)
		}
	}
	if strings.Contains(resources.SettingsScript, "localStorage.setItem") || strings.Contains(resources.SettingsScript, "sessionStorage.setItem") {
		t.Fatal("combined Management Center page must not persist plugin credentials in browser storage")
	}
	if strings.Contains(resources.QuotaPage, "sessionStorage.setItem(\"opencode-go-omni:diagnostics\"") ||
		strings.Contains(resources.QuotaPage, "localStorage.setItem(\"opencode-go-omni:diagnostics\"") {
		t.Fatal("catalog diagnostics must not be persisted in browser storage")
	}
	if !strings.Contains(resources.QuotaPage, "cards[index]?.label === remark") || !strings.Contains(resources.QuotaPage, "cards[index]?.label !== previousRemarks[index]") || !strings.Contains(resources.QuotaPage, "quotaRefreshSequence") {
		t.Fatal("quota page does not refresh credential names after plugin configuration reload")
	}
}

func TestCatalogDiagnosticsManagementEndpointIsReadOnlyAndRedacted(t *testing.T) {
	cfg, err := config.Load([]byte("api-keys:\n  - value: management-test-fixture\n"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &authRecorder{body: []byte(`{"data":[{"id":"glm-fixture"},{"id":"embedding-fixture","recommended_api":"embeddings"}]}`)}
	bridge := NewHostBridge(rec.call)
	m := NewManager(bridge)
	m.mgr = catalog.New(cfg, bridge)
	if _, err := m.mgr.RefreshConfigured(t.Context()); err != nil {
		t.Fatalf("seed diagnostics: %v", err)
	}
	for _, version := range []string{"/v0", "/v8"} {
		resp, err := m.HandleManagement(t.Context(), pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   version + "/management/plugins/" + pluginName + "/catalog-diagnostics",
		})
		if err != nil {
			t.Fatalf("%s diagnostics endpoint error: %v", version, err)
		}
		if resp.StatusCode != 0 || resp.Headers.Get("Content-Type") != "application/json" || resp.Headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s diagnostics response status/headers = %d %v", version, resp.StatusCode, resp.Headers)
		}
		var got struct {
			HasSnapshot   bool                `json:"has_snapshot"`
			RoutableCount int                 `json:"routable_count"`
			Excluded      []map[string]string `json:"excluded"`
		}
		if err := json.Unmarshal(resp.Body, &got); err != nil {
			t.Fatalf("decode diagnostics response: %v", err)
		}
		if !got.HasSnapshot || got.RoutableCount != 1 || len(got.Excluded) != 1 ||
			got.Excluded[0]["upstream_id"] != "embedding-fixture" || got.Excluded[0]["reason"] == "" {
			t.Fatalf("%s diagnostics response has wrong snapshot or exclusions: %s", version, resp.Body)
		}
		for _, forbidden := range []string{"management-test-fixture", "api_key", "Authorization", "Bearer "} {
			if strings.Contains(string(resp.Body), forbidden) {
				t.Errorf("%s diagnostics response includes secret-related data: %s", version, resp.Body)
			}
		}
	}
	if got := len(rec.seen()); got != 1 {
		t.Fatalf("diagnostics unexpectedly performed upstream calls (got %d calls including initial refresh)", got)
	}
	wrongMethod, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v8/management/plugins/" + pluginName + "/catalog-diagnostics",
	})
	if err != nil {
		t.Fatalf("wrong-method request error: %v", err)
	}
	if wrongMethod.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-method status = %d, want 404", wrongMethod.StatusCode)
	}
}

func TestCombinedManagementPageNonceChangesPerResponse(t *testing.T) {
	first, err := quotaPageResponse("/v8")
	if err != nil {
		t.Fatal(err)
	}
	second, err := quotaPageResponse("/v8")
	if err != nil {
		t.Fatal(err)
	}
	if first.Headers.Get("Content-Security-Policy") == second.Headers.Get("Content-Security-Policy") {
		t.Fatal("combined page CSP nonce was reused")
	}
}
