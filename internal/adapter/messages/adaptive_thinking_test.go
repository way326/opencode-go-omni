package messages

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the adaptive-thinking profile of claude-haiku-5-5. Live
// evidence (dist/all-model-codemode-20261017 live-c run 84cb60c9): the upstream
// rejected the budget form with HTTP 400 '"thinking.type.enabled" is not
// supported for this model. Use "thinking.type.adaptive" and
// "output_config.effort"', while the plugin had sent
// {"thinking":{"type":"enabled","budget_tokens":24576}}. The models.dev
// snapshot for the same ID (inventory/modelsdev_opencode_models.json) declares
// reasoning_options toggle + effort (low, medium, high, xhigh, max) and
// temperature:false.
//
// The conversion is deliberately scoped to the exact upstream ID: sibling
// Claude-family and minimax/qwen models on this route keep the budget protocol
// they accept today, and a native claude client stays verbatim.

// adaptiveProfileModel is the resolved upstream ID of the pinned profile.
const adaptiveProfileModel = "claude-haiku-5-5"

// adaptiveProfileTS is the declared capability shape for the profile: the
// toggle option is ZeroAllowed, and the effort values are the declared set.
func adaptiveProfileTS() *pluginapi.ThinkingSupport {
	return &pluginapi.ThinkingSupport{
		Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: true,
		Levels: []string{"low", "medium", "high", "xhigh", "max"},
	}
}

// adaptiveResponsesBody is the Responses shape the live run used: reasoning
// effort, one declared tool, its replayed call/result history, a client
// max_output_tokens and sampling controls.
func adaptiveResponsesBody(t *testing.T, effort string) []byte {
	t.Helper()
	body := map[string]any{
		"model": "opencode-go/claude-haiku-5-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "run"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}},
		"max_output_tokens": 2000,
		"temperature":       0.7,
		"top_p":             0.9,
		"stream":            true,
	}
	if effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Responses fixture: %v", err)
	}
	return out
}

// adaptiveChatBody is the Chat Completions leg of the same request shape.
func adaptiveChatBody(t *testing.T, effort string) []byte {
	t.Helper()
	body := map[string]any{
		"model": "opencode-go/claude-haiku-5-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "run"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}},
		"max_completion_tokens": 2000,
		"temperature":           0.7,
		"top_p":                 0.9,
		"stream":                true,
	}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Chat fixture: %v", err)
	}
	return out
}

// adaptiveLegs are the two client protocols that resolve a declared effort.
var adaptiveLegs = []struct {
	name   string
	format string
	body   func(t *testing.T, effort string) []byte
}{
	{"openai-response", "openai-response", adaptiveResponsesBody},
	{"openai", "openai", adaptiveChatBody},
}

// assertToolCarrierIntact proves the tool declaration and the replayed
// call/result history survive every adaptive conversion: schema, tool_use id
// and tool_result pairing must be untouched by the thinking rewrite.
func assertToolCarrierIntact(t *testing.T, m map[string]any) {
	t.Helper()
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one declaration", m["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != "lookup" {
		t.Fatalf("tool = %v, want lookup", tools[0])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("input_schema = %v", tool["input_schema"])
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %v", m["messages"])
	}
	sawToolUse, sawToolResult := false, false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if block["name"] == "lookup" && block["id"] == "call_1" {
					sawToolUse = true
				}
			case "tool_result":
				if block["tool_use_id"] == "call_1" {
					sawToolResult = true
				}
			}
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Fatalf("tool history lost: %v", msgs)
	}
}

// TestAdaptiveProfileMapsDeclaredEfforts is the red-first regression: the
// declared effort values must reach the upstream as thinking.type=adaptive +
// output_config.effort, never as thinking.type=enabled + budget_tokens (the
// form the live run rejected), and the budget's max_tokens bump must be gone.
func TestAdaptiveProfileMapsDeclaredEfforts(t *testing.T) {
	for _, leg := range adaptiveLegs {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(leg.name+"/"+effort, func(t *testing.T) {
				out, eErr := BuildRequest(adaptiveProfileModel, leg.format, leg.body(t, effort), adaptiveProfileTS())
				if eErr != nil {
					t.Fatalf("BuildRequest failed: %v", eErr)
				}
				m := decodeReq(t, out)
				thinking, ok := m["thinking"].(map[string]any)
				if !ok || thinking["type"] != "adaptive" {
					t.Fatalf("thinking = %v, want type adaptive", m["thinking"])
				}
				if _, ok := thinking["budget_tokens"]; ok {
					t.Fatalf("adaptive thinking must not carry budget_tokens: %v", thinking)
				}
				config, ok := m["output_config"].(map[string]any)
				if !ok || config["effort"] != effort {
					t.Fatalf("output_config = %v, want effort %q", m["output_config"], effort)
				}
				if _, ok := m["temperature"]; ok {
					t.Fatalf("temperature must be dropped while thinking is active: %v", m)
				}
				if _, ok := m["top_p"]; ok {
					t.Fatalf("top_p must be dropped while thinking is active: %v", m)
				}
				if m["max_tokens"] != float64(2000) {
					t.Fatalf("max_tokens = %v, want the client value 2000: adaptive carries no budget to fit", m["max_tokens"])
				}
				if m["stream"] != true {
					t.Fatalf("stream = %v, want true", m["stream"])
				}
				assertToolCarrierIntact(t, m)
			})
		}
	}
}

// TestAdaptiveProfileOffDynamicAndBoundaryEfforts pins the mapping outside the
// declared effort set: an explicit off state stays off (never silently
// adaptive), dynamic omits the effort override, the undeclared weaker level
// takes the declared floor, and unknown values keep the historical omission.
func TestAdaptiveProfileOffDynamicAndBoundaryEfforts(t *testing.T) {
	cases := []struct {
		name         string
		effort       string
		wantType     string // "" → no thinking field at all
		wantEffort   string // "" → no output_config
		keepSampling bool
	}{
		{"explicit off", "none", "disabled", "", true},
		{"dynamic", "auto", "adaptive", "", false},
		{"minimal takes the declared floor", "minimal", "adaptive", "low", false},
		{"unknown effort omitted", "maximum", "", "", true},
	}
	for _, leg := range adaptiveLegs {
		for _, tc := range cases {
			t.Run(leg.name+"/"+tc.name, func(t *testing.T) {
				out, eErr := BuildRequest(adaptiveProfileModel, leg.format, leg.body(t, tc.effort), adaptiveProfileTS())
				if eErr != nil {
					t.Fatalf("BuildRequest failed: %v", eErr)
				}
				m := decodeReq(t, out)
				if tc.wantType == "" {
					if _, ok := m["thinking"]; ok {
						t.Fatalf("thinking = %v, want omitted", m["thinking"])
					}
				} else {
					thinking, ok := m["thinking"].(map[string]any)
					if !ok || thinking["type"] != tc.wantType {
						t.Fatalf("thinking = %v, want type %q", m["thinking"], tc.wantType)
					}
					if _, ok := thinking["budget_tokens"]; ok {
						t.Fatalf("thinking must not carry budget_tokens: %v", thinking)
					}
				}
				if tc.wantEffort == "" {
					if _, ok := m["output_config"]; ok {
						t.Fatalf("output_config = %v, want omitted", m["output_config"])
					}
				} else {
					config, ok := m["output_config"].(map[string]any)
					if !ok || config["effort"] != tc.wantEffort {
						t.Fatalf("output_config = %v, want effort %q", m["output_config"], tc.wantEffort)
					}
				}
				_, hasTemperature := m["temperature"]
				if hasTemperature != tc.keepSampling {
					t.Fatalf("temperature present = %t, want %t: %v", hasTemperature, tc.keepSampling, m)
				}
				assertToolCarrierIntact(t, m)
			})
		}
	}
}

// TestBudgetProtocolUnchangedForOtherModels is the non-regression half: models
// that accept the budget form must keep it, including the max_tokens bump and
// the absence of output_config. None of them may be swept into adaptive by the
// profile fix.
func TestBudgetProtocolUnchangedForOtherModels(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{Min: 1024, Max: 32000, Levels: []string{"low", "medium", "high"}}
	for _, model := range []string{"minimax", "qwen3.8-max", "claude-opus-4-6"} {
		t.Run(model, func(t *testing.T) {
			out, eErr := BuildRequest(model, "openai-response", adaptiveResponsesBody(t, "high"), ts)
			if eErr != nil {
				t.Fatalf("BuildRequest failed: %v", eErr)
			}
			m := decodeReq(t, out)
			thinking, ok := m["thinking"].(map[string]any)
			if !ok || thinking["type"] != "enabled" {
				t.Fatalf("thinking = %v, want the budget form type enabled", m["thinking"])
			}
			if thinking["budget_tokens"] != float64(24576) {
				t.Fatalf("budget_tokens = %v, want the high table value 24576", thinking["budget_tokens"])
			}
			if _, ok := m["output_config"]; ok {
				t.Fatalf("budget protocol must not gain output_config: %v", m["output_config"])
			}
			if m["max_tokens"] != float64(25600) {
				t.Fatalf("max_tokens = %v, want the existing budget bump 25600", m["max_tokens"])
			}
		})
	}
}

// TestAdaptiveProfileNativeClaudePassthroughVerbatim pins the passthrough
// contract: a native claude client's own thinking/output_config arrive
// verbatim (only the model ID is rewritten), so the plugin never rewrites a
// Claude Protocol request on this route.
func TestAdaptiveProfileNativeClaudePassthroughVerbatim(t *testing.T) {
	body := `{"model":"opencode-go/claude-haiku-5-5","max_tokens":2048,"stream":true,` +
		`"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	out, eErr := BuildRequest(adaptiveProfileModel, "claude", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	m := decodeReq(t, out)
	if m["model"] != adaptiveProfileModel {
		t.Fatalf("model = %v, want %q", m["model"], adaptiveProfileModel)
	}
	thinking, ok := m["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("passthrough thinking = %v, want adaptive verbatim", m["thinking"])
	}
	config, ok := m["output_config"].(map[string]any)
	if !ok || config["effort"] != "xhigh" {
		t.Fatalf("passthrough output_config = %v, want effort xhigh verbatim", m["output_config"])
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Fatalf("passthrough must not invent budget_tokens: %v", thinking)
	}
	if tools, ok := m["tools"].([]any); !ok || len(tools) != 1 {
		t.Fatalf("passthrough tools = %v", m["tools"])
	}
}
