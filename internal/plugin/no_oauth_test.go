package plugin

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRegistrationDeclaresNoOAuthCapability pins the capability contract of
// the no-OAuth cutover: CPA v8.0.28 internal/pluginhost/snapshot.go maps a
// registered AuthProvider directly onto SupportsOAuth, and the SDK offers no
// way to hide OAuth while keeping the registration. Our AuthProvider.StartLogin
// only returns "unsupported", so registering it surfaces a misleading login
// card that fails with 500. The fix is registrationEnvelope reporting
// auth_provider=false while keeping the IdP-less API-key parse/refresh paths.
//
// The plugin.go registration now reports auth_provider=false; this test pins
// the live capability contract so a regression back to true fails the suite.
func TestRegistrationDeclaresNoOAuthCapability(t *testing.T) {
	var reg registrationResult
	decodeResult(t, registrationEnvelope(), &reg)
	if reg.Capabilities.AuthProvider {
		t.Fatalf("auth_provider must be false to avoid the misleading OAuth login card: %+v", reg.Capabilities)
	}
	if !reg.Capabilities.ModelProvider || !reg.Capabilities.Executor || !reg.Capabilities.ManagementAPI {
		t.Fatalf("model_provider/executor/management_api must stay registered: %+v", reg.Capabilities)
	}
	if reg.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor_model_scope = %q, want %q", reg.Capabilities.ExecutorModelScope, pluginapi.ExecutorModelScopeOAuth)
	}
	wantFormats := []string{"openai", "claude", "openai-response", "gemini"}
	if !reflect.DeepEqual(reg.Capabilities.ExecutorInputFormats, wantFormats) {
		t.Fatalf("executor_input_formats = %v, want %v", reg.Capabilities.ExecutorInputFormats, wantFormats)
	}
	if !reflect.DeepEqual(reg.Capabilities.ExecutorOutputFormats, wantFormats) {
		t.Fatalf("executor_output_formats = %v, want %v", reg.Capabilities.ExecutorOutputFormats, wantFormats)
	}
}

// TestLegacyAuthParserStaysAvailableAfterCapabilitySwitch documents the other
// half of the contract: dropping the AuthProvider REGISTRATION must not delete
// the direct ParseAuth/RefreshAuth handling that legacy or direct callers
// still exercise. The login entry points keep returning a clear "unsupported"
// envelope, so no caller is left thinking an OAuth flow exists.
func TestLegacyAuthParserStaysAvailableAfterCapabilitySwitch(t *testing.T) {
	m := NewManager(nil)
	fakeKey := "sk-no-oauth-direct-1"
	rawParse, err := m.HandleCall(pluginabi.MethodAuthParse, mustJSON(pluginapi.AuthParseRequest{
		Provider: ProviderID,
		FileName: "legacy.json",
		RawJSON:  []byte(`{"type":"opencode-go","id":"legacy","api_key":"` + fakeKey + `"}`),
	}))
	if err != nil {
		t.Fatalf("auth parse dispatch: %v", err)
	}
	var parsed pluginapi.AuthParseResponse
	decodeResult(t, rawParse, &parsed)
	if !parsed.Handled || parsed.Auth.ID != "legacy" || parsed.Auth.Attributes["api_key"] != fakeKey {
		t.Fatalf("direct auth parse = %#v", parsed)
	}

	rawRefresh, err := m.HandleCall(pluginabi.MethodAuthRefresh, mustJSON(pluginapi.AuthRefreshRequest{
		AuthID: "legacy", AuthProvider: ProviderID,
		Attributes: map[string]string{"api_key": fakeKey},
	}))
	if err != nil {
		t.Fatalf("auth refresh dispatch: %v", err)
	}
	var refreshed pluginapi.AuthRefreshResponse
	decodeResult(t, rawRefresh, &refreshed)
	if refreshed.Auth.ID != "legacy" || refreshed.Auth.Attributes["api_key"] != fakeKey {
		t.Fatalf("direct auth refresh = %#v", refreshed)
	}

	for _, method := range []string{pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll} {
		rawLogin, err := m.HandleCall(method, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s dispatch: %v", method, err)
		}
		env := decodeEnv(t, rawLogin)
		if env.OK || env.Error == nil || env.Error.Code != "unsupported" {
			t.Fatalf("%s must stay explicitly unsupported, got %#v", method, env)
		}
	}
}
