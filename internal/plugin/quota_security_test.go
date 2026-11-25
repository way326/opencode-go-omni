package plugin

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaPageSecurityHeadersAndNonce(t *testing.T) {
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

	wantHeaders := map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	}
	for name, want := range wantHeaders {
		if got := resp.Headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}

	csp := resp.Headers.Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'none'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'self'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP missing %q: %q", directive, csp)
		}
	}

	match := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if len(match) != 2 {
		t.Fatalf("CSP missing script nonce: %q", csp)
	}
	nonce := match[1]
	body := string(resp.Body)
	if got := strings.Count(body, `<script nonce="`+nonce+`">`); got != 2 {
		t.Fatalf("script nonce occurrences = %d, want 2", got)
	}
	if got := strings.Count(body, `<style nonce="`+nonce+`">`); got != 1 {
		t.Fatalf("style nonce occurrences = %d, want 1", got)
	}
	if strings.Contains(body, "__QUOTA_ENDPOINT__") {
		t.Fatal("quota endpoint placeholder was not materialized")
	}
	if !strings.Contains(body, `<button id="refresh-all-btn" class="quota-btn quota-btn-primary" hidden>`) ||
		strings.Contains(body, `<button id="refresh-all-btn" class="quota-btn" style=`) ||
		!strings.Contains(body, "refreshAllBtn.hidden = cards.length === 0") {
		t.Fatal("refresh-all visibility must not depend on a CSP-blocked style attribute")
	}
	if !strings.Contains(body, "/v8/management/plugins/"+pluginName+"/quota-usage") {
		t.Fatal("quota management endpoint is missing")
	}
}

func TestQuotaPageNonceChangesPerResponse(t *testing.T) {
	first, err := quotaPageResponse("/v8")
	if err != nil {
		t.Fatal(err)
	}
	second, err := quotaPageResponse("/v8")
	if err != nil {
		t.Fatal(err)
	}
	if first.Headers.Get("Content-Security-Policy") == second.Headers.Get("Content-Security-Policy") {
		t.Fatal("quota page CSP nonce was reused")
	}
}
