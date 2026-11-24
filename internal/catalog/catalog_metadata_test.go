package catalog

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestUpstreamMetadataRoutesNewFamilies covers the catalog's current metadata
// shape: recommended_api chooses a supported route, a single supported API
// entry is used when the recommendation names a newer non-wire protocol, and
// an explicit unsupported declaration blocks a legacy family-prefix fallback.
func TestUpstreamMetadataRoutesNewFamilies(t *testing.T) {
	body := `{"data":[` +
		`{"id":"future-chat","name":"Future Chat","limit":{"context":1000000,"output":65536},` +
		`"modalities":{"input":["text","image/png"],"output":["text"]},` +
		`"recommended_api":"chat-completions","api":{"chat-completions":{"base_url":"https://example.invalid"}}},` +
		`{"id":"future-messages","recommended_api":"messages","api":{"messages":{}}},` +
		`{"id":"future-responses","recommended_api":"responses","api":{"responses":{}}},` +
		`{"id":"future-gemini","recommended_api":"interactions","api":{"generate-content":{},"interactions":{}}},` +
		`{"id":"hungerg-metadata","recommended_api":"chat-completions","api":{"chat-completions":{}}},` +
		`{"id":"iceman-contributor-free","recommended_api":"chat-completions","api":{"chat-completions":{}}},` +
		`{"id":"gpt-6-luna-decision","recommended_api":"systemone","api":{"systemone":{}}},` +
		`{"id":"embed-only","recommended_api":"embeddings","api":{"embeddings":{}}},` +
		`{"id":"ambiguous-protocol","recommended_api":"future-api","api":{"messages":{},"responses":{}}},` +
		`{"id":"qwen3.6-plus","recommended_api":"messages","api":{"messages":{}}},` +
		`{"id":"glm-legacy"}` +
		`]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(geminiCfg(), fc)
	mustRefresh(t, m)

	models := m.Models()
	wantRoutes := map[string]Route{
		"future-chat":      RouteChatCompletions,
		"future-messages":  RouteMessages,
		"future-responses": RouteResponses,
		"future-gemini":    RouteGemini,
		"hungerg-metadata": RouteChatCompletions,
		"qwen3.6-plus":     RouteChatCompletions, // exact compatibility wins
		"glm-legacy":       RouteChatCompletions, // no metadata: prefix fallback
	}
	if len(models) != len(wantRoutes) {
		t.Fatalf("routable models = %+v, want %d", models, len(wantRoutes))
	}
	for id, want := range wantRoutes {
		if got := findModel(t, models, id).Protocol; got != want {
			t.Errorf("%s: Protocol = %q, want %q", id, got, want)
		}
	}

	chat := findModel(t, models, "future-chat")
	if chat.PublicID != "opencode-go/future-chat" || chat.DisplayName != "Future Chat" {
		t.Errorf("future-chat identity = %+v", chat)
	}
	if chat.ContextLimit != 1000000 || chat.OutputLimit != 65536 {
		t.Errorf("future-chat limits = context %d output %d", chat.ContextLimit, chat.OutputLimit)
	}
	if strings.Join(chat.InputModes, ",") != "text,image/png" || strings.Join(chat.OutputModes, ",") != "text" {
		t.Errorf("future-chat modalities = input %v output %v", chat.InputModes, chat.OutputModes)
	}
	gemini := findModel(t, models, "future-gemini")
	if gemini.EndpointPath != "/v1/models/future-gemini:generateContent" {
		t.Errorf("future-gemini endpoint = %q", gemini.EndpointPath)
	}

	unsupported := make(map[string]string)
	for _, u := range m.Unsupported() {
		unsupported[u.UpstreamID] = u.Reason
	}
	if len(unsupported) != 4 {
		t.Fatalf("unsupported = %+v, want the four metadata-rejected entries", unsupported)
	}
	for _, id := range []string{"gpt-6-luna-decision", "embed-only"} {
		if unsupported[id] != "upstream metadata advertises no supported protocol" {
			t.Errorf("%s: unsupported reason = %q", id, unsupported[id])
		}
	}
	if unsupported["ambiguous-protocol"] != "upstream metadata is ambiguous between supported protocols" {
		t.Errorf("ambiguous reason = %q", unsupported["ambiguous-protocol"])
	}
	if unsupported["iceman-contributor-free"] != unavailableModels["iceman-contributor-free"] {
		t.Errorf("iceman reason = %q", unsupported["iceman-contributor-free"])
	}
}
