package plugin

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

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
		"remove.textContent = \"删除\"",
		"新增 Key",
		"cli-proxy-auth",
		"input.type = \"password\"",
		"CPA 的 config.yaml",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("combined Management Center page missing %q", marker)
		}
	}
	for _, placeholder := range []string{"__QUOTA_ENDPOINT__", "__SETTINGS_FORM__", "__SETTINGS_STYLES__", "__SETTINGS_SCRIPT__"} {
		if strings.Contains(body, placeholder) {
			t.Fatalf("quota/settings page left placeholder %q", placeholder)
		}
	}
	if strings.Contains(resources.SettingsScript, "localStorage.setItem") || strings.Contains(resources.SettingsScript, "sessionStorage.setItem") {
		t.Fatal("combined Management Center page must not persist plugin credentials in browser storage")
	}
	if !strings.Contains(resources.QuotaPage, "cards[index]?.label === remark") || !strings.Contains(resources.QuotaPage, "cards[index]?.label !== previousRemarks[index]") || !strings.Contains(resources.QuotaPage, "quotaRefreshSequence") {
		t.Fatal("quota page does not refresh credential names after plugin configuration reload")
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
