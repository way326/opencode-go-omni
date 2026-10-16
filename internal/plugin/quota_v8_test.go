package plugin

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaV8IdentityAndEndpoint(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v8/resource/plugins/" + pluginName + "/quota"})
	if err != nil || resp.StatusCode >= 400 {
		t.Fatalf("resource error: %v, status %d", err, resp.StatusCode)
	}
	if !bytes.Contains(resp.Body, []byte("/v8/management/plugins/opencode-go-omni/quota-usage")) || bytes.Contains(resp.Body, []byte("__QUOTA_ENDPOINT__")) {
		t.Fatal("new quota page endpoint was not materialized")
	}
	resp, err = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v8/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil || resp.StatusCode >= 400 {
		t.Fatalf("quota list error: %v, status %d", err, resp.StatusCode)
	}
}
