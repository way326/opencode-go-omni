package plugin

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

const quotaFixture = `{"usage":{"rolling":{"status":"ok","percent":4.5,"resetsAt":"2026-10-15T06:58:30.000Z"},"weekly":{"status":"ok","percent":7,"resetsAt":"2026-10-19T00:00:00.000Z"},"monthly":{"status":"ok","percent":41,"resetsAt":"2026-10-27T03:46:17.000Z"}}}`

func quotaFixtureManager(f *fakeCaller) *Manager {
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{
		BaseURL: "https://quota.test/v1/", RequestTimeout: time.Second,
		APIKeys: []config.APIKey{{Value: "configured-key-must-not-be-used"}},
	}
	return m
}

func quotaRequestBody(key string) []byte {
	body, _ := json.Marshal(quotaFetchRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{
			Provider: ProviderID, AuthID: "selected-auth", AuthIndex: "selected-index",
			Attributes: map[string]string{"api_key": key},
		},
		HostCallbackID: "quota-request-context",
	})
	return body
}

func TestNativeQuotaRegistrationAndDescription(t *testing.T) {
	var registration registrationResult
	decodeResult(t, registrationEnvelope(), &registration)
	if !registration.Capabilities.QuotaProvider {
		t.Fatal("standard quota capability is missing")
	}
	m := NewManager(nil)
	var identifier struct{ Identifier string }
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaIdentifier, []byte(`{}`)), &identifier)
	if identifier.Identifier != ProviderID {
		t.Fatalf("identifier = %q", identifier.Identifier)
	}
	var description pluginapi.QuotaDescribeResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaDescribe, []byte(`{}`)), &description)
	if len(description.SupportedProviders) != 1 || description.SupportedProviders[0] != ProviderID || description.DisplayName != "OpenCode Go" || description.SupportsReset {
		t.Fatalf("description = %+v", description)
	}
}

func TestNativeQuotaSelectedCredentialsAndCallback(t *testing.T) {
	var seenKeys []string
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Errorf("unexpected method %s", method)
		}
		var request hostHTTPReq
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Error(err)
		}
		if request.Method != http.MethodGet || request.URL != "https://quota.test/v1/usage" || request.HostCallbackID != "quota-request-context" || request.Headers.Get("Accept") != "application/json" {
			t.Errorf("quota request lost endpoint, method, accept header or callback context")
		}
		seenKeys = append(seenKeys, request.Headers.Get("Authorization"))
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(quotaFixture)}), nil
	}}
	m := quotaFixtureManager(f)
	for _, key := range []string{"selected-key-A", "selected-key-B"} {
		var result pluginapi.QuotaFetchResponse
		raw := mustHandle(t, m, pluginabi.MethodQuotaFetch, quotaRequestBody(key))
		decodeResult(t, raw, &result)
		if strings.Contains(string(raw), key) || strings.Contains(string(raw), "configured-key-must-not-be-used") {
			t.Fatal("credential leaked to quota response")
		}
		if result.Subscription != nil || len(result.Summary) != 0 || len(result.Groups) != 1 || result.Groups[0].DisplayName != "OpenCode Go" || len(result.Groups[0].Buckets) != 3 {
			t.Fatalf("unexpected account quota shape: %+v", result)
		}
		for i, want := range []float64{.955, .93, .59} {
			bucket := result.Groups[0].Buckets[i]
			wantReset := []string{"2026-10-15T06:58:30.000Z", "2026-10-19T00:00:00.000Z", "2026-10-27T03:46:17.000Z"}[i]
			if math.Abs(bucket.RemainingFraction-want) > 1e-10 || bucket.Window != []string{"rolling", "weekly", "monthly"}[i] || bucket.ResetTime != wantReset {
				t.Fatalf("bucket %d = %+v", i, bucket)
			}
		}
	}
	if len(seenKeys) != 2 || seenKeys[0] != "Bearer selected-key-A" || seenKeys[1] != "Bearer selected-key-B" {
		t.Fatal("query did not use independently selected auth credentials")
	}
}

func TestNativeQuotaStoredCredential(t *testing.T) {
	f := &fakeCaller{responder: func(_ string, payload []byte) ([]byte, error) {
		var request hostHTTPReq
		_ = json.Unmarshal(payload, &request)
		if request.Headers.Get("Authorization") != "Bearer imported-storage-key" {
			t.Error("storage credential not used")
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(quotaFixture)}), nil
	}}
	body, _ := json.Marshal(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		Provider: ProviderID, StorageJSON: []byte(`{"api_key":"imported-storage-key"}`),
	}})
	var result pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaFetch, body), &result)
}

func TestNativeQuotaMetadataCredential(t *testing.T) {
	f := &fakeCaller{responder: func(_ string, payload []byte) ([]byte, error) {
		var request hostHTTPReq
		_ = json.Unmarshal(payload, &request)
		if request.Headers.Get("Authorization") != "Bearer selected-metadata-key" {
			t.Error("metadata credential not selected")
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(quotaFixture)}), nil
	}}
	body, _ := json.Marshal(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		Provider: ProviderID, Metadata: map[string]any{"api_key": "selected-metadata-key"},
		StorageJSON: []byte(`{"api_key":"old-storage-key"}`),
	}})
	var result pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaFetch, body), &result)
}

func TestNativeQuotaConcurrentCredentials(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	releaseQuota := sync.OnceFunc(func() { close(release) })
	defer releaseQuota()
	f := &fakeCaller{responder: func(_ string, payload []byte) ([]byte, error) {
		var request hostHTTPReq
		_ = json.Unmarshal(payload, &request)
		arrived <- struct{}{}
		<-release
		body := quotaFixture
		if request.Headers.Get("Authorization") == "Bearer concurrent-B" {
			body = strings.Replace(body, `"percent":4.5`, `"percent":80`, 1)
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
	}}
	m := quotaFixtureManager(f)
	// Hold both host requests in flight before returning either response.
	m.cfg.RequestTimeout = 5 * time.Second
	results := []chan []byte{make(chan []byte, 1), make(chan []byte, 1)}
	for i, key := range []string{"concurrent-A", "concurrent-B"} {
		go func(i int, key string) {
			raw, _ := m.HandleCall(pluginabi.MethodQuotaFetch, quotaRequestBody(key))
			results[i] <- raw
		}(i, key)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("both independent quota requests did not reach the host")
		}
	}
	releaseQuota()
	for i, want := range []float64{.955, .2} {
		var result pluginapi.QuotaFetchResponse
		decodeResult(t, <-results[i], &result)
		if got := result.Groups[0].Buckets[0].RemainingFraction; math.Abs(got-want) > 1e-10 {
			t.Fatalf("concurrent credential %d got %v, want %v", i, got, want)
		}
	}
}

func TestNativeQuotaAttributesTakePrecedenceOverStorage(t *testing.T) {
	f := &fakeCaller{responder: func(_ string, payload []byte) ([]byte, error) {
		var request hostHTTPReq
		_ = json.Unmarshal(payload, &request)
		if request.Headers.Get("Authorization") != "Bearer selected-attribute-key" {
			t.Error("stored credential overrode selected attributes")
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(quotaFixture)}), nil
	}}
	var req quotaFetchRequest
	_ = json.Unmarshal(quotaRequestBody("selected-attribute-key"), &req)
	req.StorageJSON = []byte(`{"api_key":"old-storage-key"}`)
	body, _ := json.Marshal(req)
	var result pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaFetch, body), &result)
}

func TestNativeQuotaTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
		<-release
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(quotaFixture)}), nil
	}}
	m := quotaFixtureManager(f)
	m.cfg.RequestTimeout = 10 * time.Millisecond
	start := time.Now()
	raw := mustHandle(t, m, pluginabi.MethodQuotaFetch, quotaRequestBody("secret"))
	env, err := decodeEnvelope(raw)
	if err != nil || env.OK || env.Error == nil || env.Error.Code != "quota_failure" || strings.Contains(string(raw), "secret") {
		t.Fatalf("unexpected timeout result: %s, err=%v", raw, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("quota request did not honor configured timeout")
	}
}

func TestNativeQuotaRejectsInvalidCredentialWithoutNetwork(t *testing.T) {
	for name, request := range map[string][]byte{
		"malformed":        []byte(`{"api_key":"secret"`),
		"null":             []byte(`null`),
		"provider":         []byte(`{"provider":"other","attributes":{"api_key":"secret"}}`),
		"missing":          []byte(`{"provider":"opencode-go"}`),
		"storage":          []byte(`{"provider":"opencode-go","storage_json":"bm90IGpzb24="}`),
		"invalid_metadata": []byte(`{"provider":"opencode-go","metadata":{"api_key":true},"storage_json":"eyJhcGlfa2V5Ijoic2VjcmV0In0="}`),
		"blank_attributes": []byte(`{"provider":"opencode-go","attributes":{"api_key":" "},"metadata":{"api_key":"secret"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{}
			raw := mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaFetch, request)
			env, err := decodeEnvelope(raw)
			if err != nil || env.OK || env.Error == nil || env.Error.Code != "invalid_request" || strings.Contains(string(raw), "secret") || len(f.recorded()) != 0 {
				t.Fatalf("invalid credential result: %s, err=%v", raw, err)
			}
		})
	}
}

func TestNativeQuotaResetNeverCallsHost(t *testing.T) {
	f := &fakeCaller{}
	raw := mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaReset, quotaRequestBody("secret"))
	env, err := decodeEnvelope(raw)
	if err != nil || env.OK || env.Error == nil || env.Error.Code != "unsupported" || len(f.recorded()) != 0 {
		t.Fatalf("reset result = %s, err=%v", raw, err)
	}
}

func TestQuotaResponseValidation(t *testing.T) {
	for name, body := range map[string]string{
		"empty":           `{}`,
		"null_usage":      `{"usage":null}`,
		"missing_window":  strings.Replace(quotaFixture, `"weekly":`, `"different":`, 1),
		"null_window":     strings.Replace(quotaFixture, `"monthly":{`, `"monthly":null,"unused":{`, 1),
		"missing_percent": strings.Replace(quotaFixture, `"percent":4.5,`, ``, 1),
		"null_percent":    strings.Replace(quotaFixture, `"percent":4.5`, `"percent":null`, 1),
		"negative":        strings.Replace(quotaFixture, `"percent":4.5`, `"percent":-1`, 1),
		"wrong_type":      strings.Replace(quotaFixture, `"percent":4.5`, `"percent":"4.5"`, 1),
		"missing_status":  strings.Replace(quotaFixture, `"status":"ok",`, ``, 1),
		"invalid_reset":   strings.Replace(quotaFixture, `2026-10-15T06:58:30.000Z`, `not-a-date`, 1),
		"invalid_json":    `upstream echoed secret invalid json`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
				return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
			}}
			m := quotaFixtureManager(f)
			raw := mustHandle(t, m, pluginabi.MethodQuotaFetch, quotaRequestBody("secret"))
			env, err := decodeEnvelope(raw)
			if err != nil || env.OK || env.Error == nil || strings.Contains(string(raw), "secret") {
				t.Fatalf("unexpected validation result = %s, err=%v", raw, err)
			}
			// The inherited standalone endpoint shares the same validation.
			if _, err := fetchQuota(context.Background(), m.bridge, m.cfg.BaseURL, m.cfg.RequestTimeout, "secret"); err == nil {
				t.Fatal("standalone fetch accepted malformed quota")
			}
		})
	}
}

func TestNativeQuotaFailureRedactsUpstream(t *testing.T) {
	for _, code := range []int{401, 403, 429, 503} {
		f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: code, Body: []byte("secret upstream response")}), nil
		}}
		raw := mustHandle(t, quotaFixtureManager(f), pluginabi.MethodQuotaFetch, quotaRequestBody("secret"))
		env, err := decodeEnvelope(raw)
		if err != nil || env.OK || env.Error == nil || env.Error.Code != "quota_failure" || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "upstream response") {
			t.Fatalf("unsafe failure: %s, err=%v", raw, err)
		}
	}
}

func TestNormalizeQuotaBoundaries(t *testing.T) {
	result := normalizeQuota(quotaUsage{
		Rolling: quotaWindow{Percent: 0}, Weekly: quotaWindow{Percent: 100}, Monthly: quotaWindow{Percent: 125},
	})
	for i, want := range []float64{1, 0, 0} {
		if got := result.Groups[0].Buckets[i].RemainingFraction; got != want {
			t.Fatalf("bucket %d = %v, want %v", i, got, want)
		}
	}
}
