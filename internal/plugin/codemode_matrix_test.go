// Code Mode offline regression matrix: every catalog ID Codex can select
// through this plugin must survive the native Code Mode wire shape — a
// Responses request declaring the freeform `exec` custom tool plus the
// custom_tool_call/custom_tool_call_output history pair — through the
// executor for that model's upstream protocol, and return to the client as a
// custom_tool_call whose raw input and call_id are intact.
//
// The matrix is fully offline: the host HTTP callbacks are faked and the
// upstream answers are canned per protocol, so a model that is not enabled on
// this machine still has its protocol routing pinned here.
//
// Fixture provenance:
//   - IDs: the 37 routable entries from the 38-model 2026-10-27 live catalog.
//     iceman-contributor-free is intentionally omitted while its upstream
//     endpoint returns 503. Eight IDs that left the live catalog are kept as a
//     legacy cohort so the compatibility tables stay covered without claiming
//     those models are still served.
//   - Routes: the product tables in internal/catalog (prefixRoutes plus the
//     exactRoutes entries, including beam-free -> chat-completions).
//   - Wire shape: dist/codemode-align-20261017/candidate-native-5/
//     patch-http-capture.json (native Codex 0.163.0-alpha.10.2): tools =
//     custom:exec with a lark grammar and no parameters, function:wait,
//     function:request_user_input; history = custom_tool_call{id, call_id,
//     name:"exec", input:<js>} + custom_tool_call_output{id, call_id,
//     output:[{type:"input_text",...}]}.
package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/way326/opencode-go-omni/internal/catalog"
)

// codeModeCohort separates the live catalog IDs from the IDs this matrix
// still exercises for compatibility after they left the live catalog.
type codeModeCohort int

const (
	codeModeLive codeModeCohort = iota
	codeModeLegacy
)

// codeModeMatrixModel is one catalog ID with the upstream protocol its
// routing metadata documents in the product tables.
type codeModeMatrixModel struct {
	id     string
	route  catalog.Route
	cohort codeModeCohort
}

// codeModeMatrixModels is the 45-ID offline matrix: 37 live routable IDs plus
// eight IDs that left the live catalog but still exercise the compatibility
// tables.
var codeModeMatrixModels = []codeModeMatrixModel{
	{"deepseek-v4.1-flash", catalog.RouteChatCompletions, codeModeLive},
	{"deepseek-v4-pro", catalog.RouteChatCompletions, codeModeLive},
	{"glm-5.1", catalog.RouteChatCompletions, codeModeLive},
	{"glm-5.2", catalog.RouteChatCompletions, codeModeLive},
	{"glm-5.3", catalog.RouteChatCompletions, codeModeLive},
	{"grok-4.6", catalog.RouteResponses, codeModeLive},
	{"grok-4.7", catalog.RouteResponses, codeModeLive},
	{"muse-spark-1.2-contributor", catalog.RouteResponses, codeModeLive},
	{"muse-spark-1.3-contributor", catalog.RouteResponses, codeModeLive},
	{"glm-5.3-flash", catalog.RouteChatCompletions, codeModeLive},
	{"omen-alpha", catalog.RouteChatCompletions, codeModeLive},
	{"gpt-5.6-luna", catalog.RouteResponses, codeModeLive},
	{"gpt-6-luna", catalog.RouteResponses, codeModeLive},
	{"claude-haiku-5-5", catalog.RouteMessages, codeModeLive},
	{"hy3", catalog.RouteChatCompletions, codeModeLive},
	{"hy4-preview", catalog.RouteChatCompletions, codeModeLive},
	{"kimi-k2.6", catalog.RouteChatCompletions, codeModeLive},
	{"kimi-k2.7-code", catalog.RouteChatCompletions, codeModeLive},
	{"kimi-k3", catalog.RouteChatCompletions, codeModeLive},
	{"kimi-k3.1-pro", catalog.RouteChatCompletions, codeModeLive},
	{"mimo-v2.5", catalog.RouteChatCompletions, codeModeLive},
	{"mimo-v2.6-flash", catalog.RouteChatCompletions, codeModeLive},
	{"mimo-v2.5-pro", catalog.RouteChatCompletions, codeModeLive},
	{"mimo-v2.6-pro", catalog.RouteChatCompletions, codeModeLive},
	{"minimax-m2.5", catalog.RouteMessages, codeModeLive},
	{"minimax-m2.7", catalog.RouteMessages, codeModeLive},
	{"gemini-4-argon", catalog.RouteGemini, codeModeLive},
	{"minimax-m3", catalog.RouteMessages, codeModeLive},
	{"minimax-m3.1-flash", catalog.RouteMessages, codeModeLive},
	{"longcat-2.0", catalog.RouteChatCompletions, codeModeLive},
	{"step-5-preview", catalog.RouteChatCompletions, codeModeLive},
	{"beam-free", catalog.RouteChatCompletions, codeModeLive},
	{"qwen3.6-plus", catalog.RouteChatCompletions, codeModeLive},
	{"qwen3.7-max", catalog.RouteChatCompletions, codeModeLive},
	{"qwen3.8-max", catalog.RouteMessages, codeModeLive},
	{"qwen3.8-flash", catalog.RouteMessages, codeModeLive},
	{"qwen3.7-plus", catalog.RouteMessages, codeModeLive},
	{"deepseek-v4-flash", catalog.RouteChatCompletions, codeModeLegacy},
	{"deepseek-v4-flash-vision-exp", catalog.RouteChatCompletions, codeModeLegacy},
	{"deepseek-flash", catalog.RouteChatCompletions, codeModeLegacy},
	{"step-5-preview-free", catalog.RouteChatCompletions, codeModeLegacy},
	{"cat-jam-free", catalog.RouteChatCompletions, codeModeLegacy},
	{"gemini-4-argon-contributor", catalog.RouteGemini, codeModeLegacy},
	{"longcat-2.5-preview-free", catalog.RouteChatCompletions, codeModeLegacy},
	{"space-bunny", catalog.RouteChatCompletions, codeModeLegacy},
}

// codeModeGrammarDefinition is a trimmed but structurally faithful copy of the
// native `exec` lark grammar (pragma line plus an opaque SOURCE rule).
const codeModeGrammarDefinition = "start: pragma_source | plain_source\npragma_source: PRAGMA_LINE NEWLINE SOURCE\nPRAGMA_LINE: /[ \\t]*\\/\\/ @exec:[^\\r\\n]*/\nSOURCE: /[\\s\\S]+/\n"

const codeModeOutputText = "Script completed\nWall time: 0.2 seconds\nOutput:\n"

func codeModeCatalogBody() string {
	entries := make([]map[string]any, 0, len(codeModeMatrixModels))
	for _, m := range codeModeMatrixModels {
		entries = append(entries, map[string]any{"id": m.id, "object": "model", "owned_by": "opencode"})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": entries})
	return string(body)
}

// codeModeEndpointFragment is the URL fragment the resolved model's protocol
// requires the executor to reach.
func codeModeEndpointFragment(route catalog.Route, id string) string {
	if route == catalog.RouteGemini {
		return "/v1/models/" + id + ":"
	}
	return route.EndpointPath()
}

// codeModeToolCase names one custom tool declaration and the wire name the
// upstream must see after namespace unrolling.
type codeModeToolCase struct {
	name      string
	namespace string
	wire      string
}

func codeModeExecTool() codeModeToolCase { return codeModeToolCase{name: "exec", wire: "exec"} }

func codeModeNamespacedExecTool() codeModeToolCase {
	return codeModeToolCase{name: "exec", namespace: "functions", wire: "functions__exec"}
}

// codeModeCall is one tool call the canned upstream returns.
type codeModeCall struct {
	callID string
	code   string
}

// codeModeHistory is the round-2 request's custom_tool_call/custom_tool_call_output pair.
type codeModeHistory struct {
	tool codeModeToolCase
	call codeModeCall
}

// codeModeWrappedArgs renders the required string-input wrapper independently
// of the product's own helper so the fixture cannot agree with a broken
// implementation by construction.
func codeModeWrappedArgs(code string) string {
	args, _ := json.Marshal(map[string]string{"input": code})
	return string(args)
}

// codeModeJS renders one Go string as a JSON string literal.
func codeModeJS(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func codeModeRequestBody(t *testing.T, id string, tool codeModeToolCase, history *codeModeHistory, stream bool) []byte {
	t.Helper()
	exec := map[string]any{
		"type":        "custom",
		"name":        tool.name,
		"description": "Run JavaScript code to orchestrate/compose tool calls",
		"format": map[string]any{
			"type": "grammar", "syntax": "lark", "definition": codeModeGrammarDefinition,
		},
	}
	var execDecl any = exec
	if tool.namespace != "" {
		execDecl = map[string]any{"type": "namespace", "name": tool.namespace, "tools": []any{exec}}
	}
	tools := []any{execDecl,
		map[string]any{"type": "function", "name": "wait", "description": "Waits on a yielded exec cell.", "strict": false,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"cell_id": map[string]any{"type": "string"}}, "required": []any{"cell_id"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_user_input", "description": "Request user input.", "strict": false,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"questions": map[string]any{"type": "array"}}, "required": []any{"questions"}, "additionalProperties": false}},
	}
	input := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "Run the Code Mode probe."}},
	}}
	if history != nil {
		call := map[string]any{
			"type": "custom_tool_call", "id": "ctc_" + history.call.callID,
			"call_id": history.call.callID, "name": history.tool.name, "input": history.call.code,
		}
		if history.tool.namespace != "" {
			call["namespace"] = history.tool.namespace
		}
		output := map[string]any{
			"type": "custom_tool_call_output", "id": "ctco_" + history.call.callID, "call_id": history.call.callID,
			"output": []any{
				map[string]any{"type": "input_text", "text": codeModeOutputText},
				map[string]any{"type": "input_text", "text": "{}"},
			},
		}
		input = append(input, call, output)
	}
	body, err := json.Marshal(map[string]any{
		"model": "opencode-go/" + id, "stream": stream, "input": input, "tools": tools,
		"tool_choice": "auto", "parallel_tool_calls": true, "store": false,
	})
	if err != nil {
		t.Fatalf("marshal Code Mode request: %v", err)
	}
	return body
}

// codeModePassthrough reports whether the resolved model's own upstream
// speaks native Responses, so the plugin must not rewrite the Code Mode body.
func codeModePassthrough(model codeModeMatrixModel) bool {
	return model.route == catalog.RouteResponses && strings.HasPrefix(model.id, "gpt")
}

func codeModeUpstreamResponse(model codeModeMatrixModel, tool codeModeToolCase, call codeModeCall) string {
	args := codeModeWrappedArgs(call.code)
	var body map[string]any
	switch model.route {
	case catalog.RouteChatCompletions:
		body = map[string]any{
			"id": "cc-matrix", "object": "chat.completion", "model": model.id,
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": call.callID, "type": "function",
						"function": map[string]any{"name": tool.wire, "arguments": args},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
		}
	case catalog.RouteMessages:
		body = map[string]any{
			"id": "msg-matrix", "type": "message", "role": "assistant", "model": model.id,
			"content": []any{map[string]any{
				"type": "tool_use", "id": call.callID, "name": tool.wire,
				"input": map[string]any{"input": call.code},
			}},
			"stop_reason": "tool_use", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 6},
		}
	case catalog.RouteGemini:
		body = map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"functionCall": map[string]any{"name": tool.wire, "args": map[string]any{"input": call.code}}}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 6, "totalTokenCount": 11},
			"modelVersion":  "gemini-matrix", "responseId": "resp-matrix",
		}
	case catalog.RouteResponses:
		if codeModePassthrough(model) {
			body = map[string]any{
				"id": "resp-matrix", "object": "response", "status": "completed", "model": model.id,
				"output": []any{map[string]any{
					"type": "custom_tool_call", "id": "ctc_" + call.callID, "call_id": call.callID,
					"name": tool.name, "input": call.code,
				}},
				"usage": map[string]any{"input_tokens": 5, "output_tokens": 6, "total_tokens": 11},
			}
			break
		}
		body = map[string]any{
			"id": "resp-matrix", "object": "response", "status": "completed", "model": model.id,
			"output": []any{map[string]any{
				"type": "function_call", "id": "fc_matrix", "call_id": call.callID,
				"name": tool.wire, "arguments": args,
			}},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 6, "total_tokens": 11},
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// codeModeChatChunk renders one Chat Completions chunk frame carrying the
// given assistant delta; a marshaled map keeps the JSON structural instead of
// hand-escaped.
func codeModeChatChunk(model string, delta map[string]any) string {
	chunk, _ := json.Marshal(map[string]any{
		"id": "cc-matrix", "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
	return "data: " + string(chunk) + "\n\n"
}

// codeModeStreamFrames renders the upstream SSE stream for one round.
func codeModeStreamFrames(model codeModeMatrixModel, tool codeModeToolCase, call codeModeCall) []string {
	args := codeModeWrappedArgs(call.code)
	switch model.route {
	case catalog.RouteChatCompletions:
		cut := len(`{"input":"`)
		fragments := []string{args[:cut], args[cut:]}
		frames := []string{codeModeChatChunk(model.id, map[string]any{"role": "assistant"})}
		for i, fragment := range fragments {
			entry := map[string]any{
				"index":    0,
				"function": map[string]any{"name": tool.wire, "arguments": fragment},
			}
			if i == 0 {
				entry["id"] = call.callID
			}
			frames = append(frames, codeModeChatChunk(model.id, map[string]any{"tool_calls": []any{entry}}))
		}
		frames = append(frames,
			fmt.Sprintf(`data: {"id":"cc-matrix","object":"chat.completion.chunk","model":%s,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n", codeModeJS(model.id)),
			"data: [DONE]\n\n")
		return frames
	case catalog.RouteMessages:
		cut := len(`{"input":"`)
		fragments := []string{args[:cut], args[cut:]}
		frames := []string{
			"event: message_start\ndata: " + fmt.Sprintf(`{"type":"message_start","message":{"id":"msg-matrix","role":"assistant","content":[],"model":%s}}`, codeModeJS(model.id)) + "\n\n",
			"event: content_block_start\ndata: " + fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, codeModeJS(call.callID), codeModeJS(tool.wire)) + "\n\n",
		}
		for _, fragment := range fragments {
			frames = append(frames, "event: content_block_delta\ndata: "+fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, codeModeJS(fragment))+"\n\n")
		}
		frames = append(frames,
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return frames
	case catalog.RouteGemini:
		return []string{"data: " + fmt.Sprintf(
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":%s,"args":{"input":%s}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":6,"totalTokenCount":11},"modelVersion":"gemini-matrix","responseId":"resp-matrix"}`+"\n\n",
			codeModeJS(tool.wire), codeModeJS(call.code))}
	case catalog.RouteResponses:
		if codeModePassthrough(model) {
			added := fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":%s,"type":"custom_tool_call","status":"in_progress","input":"","call_id":%s,"name":%s}}`,
				codeModeJS("ctc_"+call.callID), codeModeJS(call.callID), codeModeJS(tool.name))
			delta := fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","sequence_number":1,"item_id":%s,"output_index":0,"delta":%s}`,
				codeModeJS("ctc_"+call.callID), codeModeJS(call.code))
			done := fmt.Sprintf(`{"type":"response.custom_tool_call_input.done","sequence_number":2,"item_id":%s,"output_index":0,"input":%s}`,
				codeModeJS("ctc_"+call.callID), codeModeJS(call.code))
			itemDone := fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":%s,"type":"custom_tool_call","status":"completed","input":%s,"call_id":%s,"name":%s}}`,
				codeModeJS("ctc_"+call.callID), codeModeJS(call.code), codeModeJS(call.callID), codeModeJS(tool.name))
			completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-matrix","model":%s,"status":"completed","output":[{"id":%s,"type":"custom_tool_call","status":"completed","input":%s,"call_id":%s,"name":%s}]}}`,
				codeModeJS(model.id), codeModeJS("ctc_"+call.callID), codeModeJS(call.code), codeModeJS(call.callID), codeModeJS(tool.name))
			return []string{
				"event: response.output_item.added\ndata: " + added + "\n\n",
				"event: response.custom_tool_call_input.delta\ndata: " + delta + "\n\n",
				"event: response.custom_tool_call_input.done\ndata: " + done + "\n\n",
				"event: response.output_item.done\ndata: " + itemDone + "\n\n",
				"event: response.completed\ndata: " + completed + "\n\n",
			}
		}
		cut := len(`{"input":"`)
		fragments := []string{args[:cut], args[cut:]}
		added := fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_matrix","type":"function_call","call_id":%s,"name":%s,"arguments":""}}`, codeModeJS(call.callID), codeModeJS(tool.wire))
		frames := []string{"event: response.created\ndata: " + fmt.Sprintf(`{"type":"response.created","response":{"id":"resp-matrix","model":%s}}`, codeModeJS(model.id)) + "\n\n"}
		frames = append(frames, "event: response.output_item.added\ndata: "+added+"\n\n")
		for _, fragment := range fragments {
			frames = append(frames, "event: response.function_call_arguments.delta\ndata: "+fmt.Sprintf(
				`{"type":"response.function_call_arguments.delta","item_id":"fc_matrix","output_index":0,"delta":%s}`, codeModeJS(fragment))+"\n\n")
		}
		frames = append(frames,
			"event: response.function_call_arguments.done\ndata: "+fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":"fc_matrix","output_index":0,"arguments":%s}`, codeModeJS(args))+"\n\n",
			"event: response.output_item.done\ndata: "+fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_matrix","type":"function_call","call_id":%s,"name":%s,"arguments":%s}}`, codeModeJS(call.callID), codeModeJS(tool.wire), codeModeJS(args))+"\n\n",
			"event: response.completed\ndata: "+fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-matrix","model":%s,"status":"completed","output":[{"id":"fc_matrix","type":"function_call","call_id":%s,"name":%s,"arguments":%s}]}}`, codeModeJS(model.id), codeModeJS(call.callID), codeModeJS(tool.wire), codeModeJS(args))+"\n\n")
		return frames
	}
	return nil
}

// codeModeHost scripts the host HTTP callbacks for one case at a time: the
// catalog fetch is answered by wrapWithCatalog, and each inference call is
// checked against the endpoint the case's protocol requires before the canned
// upstream answer is served.
type codeModeHost struct {
	t      *testing.T
	model  codeModeMatrixModel
	body   string
	frames []string
	reads  int

	gotURL  string
	gotBody []byte
}

func (h *codeModeHost) expect(model codeModeMatrixModel, body string) {
	h.model, h.body, h.frames, h.reads = model, body, nil, 0
}

func (h *codeModeHost) expectStream(model codeModeMatrixModel, frames []string) {
	h.model, h.body, h.frames, h.reads = model, "", frames, 0
}

func (h *codeModeHost) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
		var wire hostHTTPReq
		_ = json.Unmarshal(payload, &wire)
		h.gotURL, h.gotBody = wire.URL, wire.Body
		fragment := codeModeEndpointFragment(h.model.route, h.model.id)
		if !strings.Contains(wire.URL, fragment) {
			h.t.Errorf("%s: upstream URL = %s, want the %s endpoint (%s)", h.model.id, wire.URL, h.model.route, fragment)
			return hostErr("matrix", "unexpected endpoint"), nil
		}
		if method == pluginabi.MethodHostHTTPDoStream {
			if !strings.Contains(wire.URL, ":streamGenerateContent") && h.model.route == catalog.RouteGemini {
				h.t.Errorf("%s: gemini stream URL = %s, want the streamGenerateContent action", h.model.id, wire.URL)
			}
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "up-" + h.model.id,
			}), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(h.body),
		}), nil
	case pluginabi.MethodHostHTTPStreamRead:
		if h.reads < len(h.frames) {
			frame := h.frames[h.reads]
			h.reads++
			return hostOK(hostStreamReadResp{Payload: []byte(frame)}), nil
		}
		return hostOK(hostStreamReadResp{Done: true}), nil
	default:
		return hostOK(map[string]any{}), nil
	}
}

// codeModeRunner drives one case against a manager whose host callbacks are
// scripted by host.
type codeModeRunner struct {
	m    *Manager
	f    *fakeCaller
	host *codeModeHost
}

// round runs one Code Mode round and returns the executor envelope plus the
// client-visible bytes (the converted payload, or the joined stream events).
func (r *codeModeRunner) round(t *testing.T, model codeModeMatrixModel, tool codeModeToolCase, prior *codeModeHistory, call codeModeCall, stream bool) (pluginabi.Envelope, string) {
	t.Helper()
	r.host.t = t
	body := codeModeRequestBody(t, model.id, tool, prior, stream)
	if stream {
		r.host.expectStream(model, codeModeStreamFrames(model, tool, call))
		before := len(r.f.callsOf(pluginabi.MethodHostStreamEmit))
		beforeClose := len(r.f.callsOf(pluginabi.MethodHostStreamClose))
		env := decodeEnv(t, mustHandle(t, r.m, pluginabi.MethodExecutorExecuteStream,
			execStreamReqBody("opencode-go/"+model.id, "openai-response", body, "down-"+model.id)))
		if !env.OK {
			return env, ""
		}
		r.m.bridge.WaitForInFlight(5 * time.Second)
		assertCodeModeUpstreamRequest(t, model, tool, r.host.gotBody, prior)
		blob := codeModeEmittedBlob(t, r.f, before)
		assertCodeModeClientStream(t, model, tool, blob, call, codeModeStreamDiagnostics(r.f, beforeClose))
		return env, blob
	}
	r.host.expect(model, codeModeUpstreamResponse(model, tool, call))
	env := decodeEnv(t, mustHandle(t, r.m, pluginabi.MethodExecutorExecute,
		execReqBody("opencode-go/"+model.id, "openai-response", body, false)))
	if !env.OK {
		return env, ""
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("%s: executor result decode: %v", model.id, err)
	}
	assertCodeModeUpstreamRequest(t, model, tool, r.host.gotBody, prior)
	assertCodeModeClientPayload(t, model, tool, out.Payload, call)
	return env, string(out.Payload)
}

// codeModeRequireRoute treats a classified 404 on a legacy ID as an accepted
// outcome (the model is gone from the live catalog) and fails everything else.
func codeModeRequireRoute(t *testing.T, model codeModeMatrixModel, env pluginabi.Envelope) bool {
	t.Helper()
	if env.OK {
		return true
	}
	if model.cohort == codeModeLegacy && env.Error != nil && env.Error.HTTPStatus == http.StatusNotFound {
		t.Skipf("%s left the live catalog and is no longer routable (classified 404)", model.id)
		return false
	}
	t.Fatalf("%s: executor failed: %+v", model.id, env.Error)
	return false
}

func codeModeEmittedBlob(t *testing.T, f *fakeCaller, from int) string {
	t.Helper()
	calls := f.callsOf(pluginabi.MethodHostStreamEmit)
	if from > len(calls) {
		from = len(calls)
	}
	var blob strings.Builder
	for _, call := range calls[from:] {
		blob.Write(wireBody(t, decodePayload(t, call), "payload"))
	}
	return blob.String()
}

// codeModeStreamDiagnostics renders the downstream close payloads recorded for
// one case, so a stream assertion failure shows the lifecycle error label the
// pump recorded instead of only the truncated event blob.
func codeModeStreamDiagnostics(f *fakeCaller, from int) string {
	calls := f.callsOf(pluginabi.MethodHostStreamClose)
	if from > len(calls) {
		from = len(calls)
	}
	var parts []string
	for _, call := range calls[from:] {
		parts = append(parts, string(call.payload))
	}
	if len(parts) == 0 {
		return "no downstream close recorded"
	}
	return strings.Join(parts, " | ")
}

func codeModeMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func codeModeSlice(value any) []any {
	s, _ := value.([]any)
	return s
}

// codeModeHasText reports whether want appears in any string reachable from
// value, so tool-result text is found regardless of the nesting the route uses.
func codeModeHasText(value any, want string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, want)
	case []any:
		for _, item := range v {
			if codeModeHasText(item, want) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if codeModeHasText(item, want) {
				return true
			}
		}
	}
	return false
}

func codeModeHasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// assertCodeModeUpstreamRequest pins what the upstream protocol must receive:
// the exec custom tool rewritten to its function form (except native GPT
// passthrough), the required string-input wrapper, and — on the second round —
// the custom_tool_call/custom_tool_call_output pair matched by call_id.
func assertCodeModeUpstreamRequest(t *testing.T, model codeModeMatrixModel, tool codeModeToolCase, body []byte, prior *codeModeHistory) {
	t.Helper()
	if len(body) == 0 {
		t.Fatalf("%s: no upstream request was recorded", model.id)
	}
	switch model.route {
	case catalog.RouteChatCompletions:
		assertCodeModeChatUpstream(t, model.id, tool, body, prior)
	case catalog.RouteMessages:
		assertCodeModeMessagesUpstream(t, model.id, tool, body, prior)
	case catalog.RouteGemini:
		assertCodeModeGeminiUpstream(t, model.id, tool, body, prior)
	case catalog.RouteResponses:
		assertCodeModeResponsesUpstream(t, model.id, tool, body, prior, codeModePassthrough(model))
	}
}

func assertCodeModeChatUpstream(t *testing.T, id string, tool codeModeToolCase, body []byte, prior *codeModeHistory) {
	t.Helper()
	var req struct {
		Model string `json:"model"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("%s: chat upstream body decode: %v (%s)", id, err, body)
	}
	if req.Model != id {
		t.Errorf("%s: upstream model = %q, want the resolved upstream id", id, req.Model)
	}
	if strings.Contains(string(body), `"type":"custom"`) {
		t.Errorf("%s: client-only custom tool declaration reached the chat upstream: %s", id, body)
	}
	found := false
	for _, decl := range req.Tools {
		if decl.Type == "custom" {
			t.Errorf("%s: tool %q still declares type custom upstream", id, decl.Function.Name)
		}
		if decl.Function.Name != tool.wire {
			continue
		}
		found = true
		if decl.Type != "function" {
			t.Errorf("%s: exec declaration type = %q, want function", id, decl.Type)
		}
		var params struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(decl.Function.Parameters, &params); err != nil {
			t.Errorf("%s: exec parameters decode: %v", id, err)
			continue
		}
		if params.Properties["input"].Type != "string" || !codeModeHasString(params.Required, "input") {
			t.Errorf("%s: exec parameters %s do not require the string input wrapper", id, decl.Function.Parameters)
		}
	}
	if !found {
		t.Fatalf("%s: chat upstream is missing the %s function declaration: %s", id, tool.wire, body)
	}
	if prior == nil {
		for _, msg := range req.Messages {
			if len(codeModeSlice(msg["tool_calls"])) > 0 || msg["tool_call_id"] != nil {
				t.Errorf("%s: first round carried tool history: %v", id, msg)
			}
		}
		return
	}
	foundCall, foundOutput := false, false
	for _, msg := range req.Messages {
		for _, rawCall := range codeModeSlice(msg["tool_calls"]) {
			entry := codeModeMap(rawCall)
			if entry["id"] != prior.call.callID {
				continue
			}
			fn := codeModeMap(entry["function"])
			if fn["name"] != tool.wire {
				t.Errorf("%s: history call name = %v, want %s", id, fn["name"], tool.wire)
			}
			arguments, _ := fn["arguments"].(string)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(arguments), &parsed); err != nil || parsed["input"] != prior.call.code {
				t.Errorf("%s: history call arguments = %q, want the wrapped input %s", id, arguments, codeModeWrappedArgs(prior.call.code))
			}
			foundCall = true
		}
		if msg["tool_call_id"] == prior.call.callID {
			foundOutput = true
			if !codeModeHasText(msg["content"], "Script completed") {
				t.Errorf("%s: tool result lost its output text: %v", id, msg)
			}
		}
	}
	if !foundCall || !foundOutput {
		t.Errorf("%s: chat history pair not matched by call_id %s (call=%t output=%t)", id, prior.call.callID, foundCall, foundOutput)
	}
}

func assertCodeModeMessagesUpstream(t *testing.T, id string, tool codeModeToolCase, body []byte, prior *codeModeHistory) {
	t.Helper()
	var req struct {
		Model string `json:"model"`
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"input_schema"`
		} `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("%s: messages upstream body decode: %v (%s)", id, err, body)
	}
	if req.Model != id {
		t.Errorf("%s: upstream model = %q, want the resolved upstream id", id, req.Model)
	}
	if strings.Contains(string(body), `"type":"custom"`) {
		t.Errorf("%s: client-only custom tool declaration reached the messages upstream: %s", id, body)
	}
	found := false
	for _, decl := range req.Tools {
		if decl.Name != tool.wire {
			continue
		}
		found = true
		if decl.InputSchema.Properties["input"].Type != "string" || !codeModeHasString(decl.InputSchema.Required, "input") {
			t.Errorf("%s: exec input_schema does not require the string input wrapper: %+v", id, decl.InputSchema)
		}
	}
	if !found {
		t.Fatalf("%s: messages upstream is missing the %s tool declaration: %s", id, tool.wire, body)
	}
	if prior == nil {
		return
	}
	foundCall, foundOutput := false, false
	for _, msg := range req.Messages {
		for _, rawBlock := range codeModeSlice(msg["content"]) {
			block := codeModeMap(rawBlock)
			switch block["type"] {
			case "tool_use":
				if block["id"] != prior.call.callID {
					continue
				}
				if block["name"] != tool.wire {
					t.Errorf("%s: history tool_use name = %v, want %s", id, block["name"], tool.wire)
				}
				if got := codeModeMap(block["input"])["input"]; got != prior.call.code {
					t.Errorf("%s: history tool_use input = %v, want the raw code", id, got)
				}
				foundCall = true
			case "tool_result":
				if block["tool_use_id"] != prior.call.callID {
					continue
				}
				foundOutput = true
				if !codeModeHasText(block["content"], "Script completed") {
					t.Errorf("%s: tool_result lost its output text: %v", id, block)
				}
			}
		}
	}
	if !foundCall || !foundOutput {
		t.Errorf("%s: messages history pair not matched by call_id %s (call=%t output=%t)", id, prior.call.callID, foundCall, foundOutput)
	}
}

func assertCodeModeGeminiUpstream(t *testing.T, id string, tool codeModeToolCase, body []byte, prior *codeModeHistory) {
	t.Helper()
	var req struct {
		Model string `json:"model"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name                 string          `json:"name"`
				ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("%s: gemini upstream body decode: %v (%s)", id, err, body)
	}
	if req.Model != id {
		t.Errorf("%s: upstream model = %q, want the resolved upstream id", id, req.Model)
	}
	if strings.Contains(string(body), `"type":"custom"`) {
		t.Errorf("%s: client-only custom tool declaration reached the gemini upstream: %s", id, body)
	}
	found := false
	for _, block := range req.Tools {
		for _, decl := range block.FunctionDeclarations {
			if decl.Name != tool.wire {
				continue
			}
			found = true
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(decl.ParametersJSONSchema, &schema); err != nil {
				t.Errorf("%s: gemini schema decode: %v", id, err)
				continue
			}
			if schema.Properties["input"].Type != "string" || !codeModeHasString(schema.Required, "input") {
				t.Errorf("%s: gemini declaration does not require the string input wrapper: %s", id, decl.ParametersJSONSchema)
			}
		}
	}
	if !found {
		t.Fatalf("%s: gemini upstream is missing the %s function declaration: %s", id, tool.wire, body)
	}
	if prior == nil {
		return
	}
	foundCall, foundOutput := false, false
	for _, turn := range req.Contents {
		for _, rawPart := range codeModeSlice(turn["parts"]) {
			part := codeModeMap(rawPart)
			if call := codeModeMap(part["functionCall"]); call != nil {
				if call["name"] != tool.wire {
					continue
				}
				if got := codeModeMap(call["args"])["input"]; got != prior.call.code {
					t.Errorf("%s: history functionCall args.input = %v, want the raw code", id, got)
				}
				foundCall = true
			}
			if response := codeModeMap(part["functionResponse"]); response != nil {
				if response["name"] != tool.wire {
					continue
				}
				foundOutput = true
				if !codeModeHasText(response, "Script completed") {
					t.Errorf("%s: functionResponse lost its output text: %v", id, response)
				}
			}
		}
	}
	if !foundCall || !foundOutput {
		t.Errorf("%s: gemini history pair not matched by name %s (call=%t output=%t)", id, tool.wire, foundCall, foundOutput)
	}
}

func assertCodeModeResponsesUpstream(t *testing.T, id string, tool codeModeToolCase, body []byte, prior *codeModeHistory, passthrough bool) {
	t.Helper()
	var req struct {
		Model string           `json:"model"`
		Input []map[string]any `json:"input"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("%s: responses upstream body decode: %v (%s)", id, err, body)
	}
	if req.Model != id {
		t.Errorf("%s: upstream model = %q, want the resolved upstream id", id, req.Model)
	}
	if passthrough {
		found := false
		for _, decl := range req.Tools {
			if decl["name"] != tool.name || decl["namespace"] != nil {
				continue
			}
			found = true
			if decl["type"] != "custom" {
				t.Errorf("%s: native GPT passthrough rewrote the exec declaration: %v", id, decl)
			}
			if decl["format"] == nil {
				t.Errorf("%s: native GPT passthrough dropped the grammar format: %v", id, decl)
			}
		}
		if !found {
			t.Fatalf("%s: native GPT passthrough is missing the custom exec declaration: %s", id, body)
		}
		if prior != nil {
			foundCall, foundOutput := false, false
			for _, item := range req.Input {
				switch item["type"] {
				case "custom_tool_call":
					if item["call_id"] != prior.call.callID {
						continue
					}
					if item["input"] != prior.call.code {
						t.Errorf("%s: passthrough history input was rewritten: %v", id, item["input"])
					}
					foundCall = true
				case "custom_tool_call_output":
					if item["call_id"] == prior.call.callID {
						foundOutput = true
					}
				}
			}
			if !foundCall || !foundOutput {
				t.Errorf("%s: native passthrough lost the custom history pair (call=%t output=%t)", id, foundCall, foundOutput)
			}
		}
		return
	}
	found := false
	for _, decl := range req.Tools {
		if decl["name"] != tool.wire {
			continue
		}
		found = true
		if decl["type"] != "function" {
			t.Errorf("%s: exec declaration type = %v, want function on the normalized route", id, decl["type"])
		}
		params := codeModeMap(decl["parameters"])
		properties := codeModeMap(params["properties"])
		if properties["input"] == nil {
			t.Errorf("%s: exec declaration lost its input property: %v", id, decl)
		}
		if required, _ := params["required"].([]any); len(required) != 1 || required[0] != "input" {
			t.Errorf("%s: exec declaration required = %v, want [input]", id, params["required"])
		}
	}
	if !found {
		t.Fatalf("%s: normalized responses upstream is missing the %s function declaration: %s", id, tool.wire, body)
	}
	for _, item := range req.Input {
		if item["namespace"] != nil {
			t.Errorf("%s: flattened history item still carries a namespace: %v", id, item)
		}
	}
	if prior == nil {
		return
	}
	foundCall, foundOutput := false, false
	for _, item := range req.Input {
		switch item["type"] {
		case "function_call":
			if item["call_id"] != prior.call.callID {
				continue
			}
			if item["name"] != tool.wire {
				t.Errorf("%s: history function_call name = %v, want %s", id, item["name"], tool.wire)
			}
			arguments, _ := item["arguments"].(string)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(arguments), &parsed); err != nil || parsed["input"] != prior.call.code {
				t.Errorf("%s: history function_call arguments = %q, want the wrapped input", id, arguments)
			}
			foundCall = true
		case "function_call_output":
			if item["call_id"] == prior.call.callID {
				foundOutput = true
				if !codeModeHasText(item["output"], "Script completed") {
					t.Errorf("%s: function_call_output lost its text: %v", id, item)
				}
			}
		}
	}
	if !foundCall || !foundOutput {
		t.Errorf("%s: responses history pair not matched by call_id %s (call=%t output=%t)", id, prior.call.callID, foundCall, foundOutput)
	}
}

// assertCodeModeClientPayload pins the non-stream client contract: exactly one
// custom_tool_call carrying the original tool identity and raw input, no
// function_call shim, and no flattened wire name for namespaced tools.
func assertCodeModeClientPayload(t *testing.T, model codeModeMatrixModel, tool codeModeToolCase, payload []byte, call codeModeCall) {
	t.Helper()
	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("%s: client payload decode: %v (%s)", model.id, err, payload)
	}
	var custom []map[string]any
	for _, item := range resp.Output {
		switch item["type"] {
		case "custom_tool_call":
			custom = append(custom, item)
		case "function_call":
			t.Errorf("%s: exec surfaced as a function_call shim: %s", model.id, payload)
		}
	}
	if len(custom) != 1 {
		t.Fatalf("%s: custom_tool_call items = %d, want 1: %s", model.id, len(custom), payload)
	}
	assertCodeModeClientItem(t, model, tool, custom[0], call, string(payload))
}

// assertCodeModeClientStream pins the streamed client contract: the custom
// tool call is announced with its original identity, the complete raw input is
// delivered, and neither the wrapper JSON nor a flattened wire name leaks.
func assertCodeModeClientStream(t *testing.T, model codeModeMatrixModel, tool codeModeToolCase, blob string, call codeModeCall, diagnostics string) {
	t.Helper()
	if !strings.Contains(blob, `"type":"custom_tool_call"`) {
		t.Fatalf("%s: stream never announced a custom_tool_call (close: %s): %s", model.id, diagnostics, blob)
	}
	if !strings.Contains(blob, `"input":`+codeModeJS(call.code)) {
		t.Fatalf("%s: stream never carried the complete custom input: %s", model.id, blob)
	}
	if !strings.Contains(blob, `"name":`+codeModeJS(tool.name)) {
		t.Fatalf("%s: stream never carried the original tool name: %s", model.id, blob)
	}
	if strings.Contains(blob, `{\"input\":\"`) {
		t.Fatalf("%s: raw input wrapper fragment leaked into the client stream: %s", model.id, blob)
	}
	if model.route != catalog.RouteGemini {
		if !strings.Contains(blob, `"call_id":`+codeModeJS(call.callID)) {
			t.Fatalf("%s: stream lost call_id %s: %s", model.id, call.callID, blob)
		}
	} else if !strings.Contains(blob, `"call_id":"call_`) {
		t.Fatalf("%s: gemini stream never carried a synthesized call_id: %s", model.id, blob)
	}
	if tool.namespace != "" {
		if !strings.Contains(blob, `"namespace":`+codeModeJS(tool.namespace)) {
			t.Fatalf("%s: stream never restored the tool namespace: %s", model.id, blob)
		}
		if strings.Contains(blob, tool.wire) {
			t.Fatalf("%s: flattened wire name leaked into the client stream: %s", model.id, blob)
		}
	}
}

func assertCodeModeClientItem(t *testing.T, model codeModeMatrixModel, tool codeModeToolCase, item map[string]any, call codeModeCall, payload string) {
	t.Helper()
	if item["name"] != tool.name {
		t.Errorf("%s: custom_tool_call name = %v, want %s", model.id, item["name"], tool.name)
	}
	if got, _ := item["namespace"].(string); got != tool.namespace {
		t.Errorf("%s: custom_tool_call namespace = %v, want %q", model.id, item["namespace"], tool.namespace)
	}
	if item["input"] != call.code {
		t.Errorf("%s: custom_tool_call input = %v, want the raw code", model.id, item["input"])
	}
	if model.route == catalog.RouteGemini {
		if callID, _ := item["call_id"].(string); !strings.HasPrefix(callID, "call_") {
			t.Errorf("%s: gemini custom_tool_call call_id = %v, want a synthesized call_* id", model.id, item["call_id"])
		}
	} else if item["call_id"] != call.callID {
		t.Errorf("%s: custom_tool_call call_id = %v, want %s", model.id, item["call_id"], call.callID)
	}
	if tool.namespace != "" && strings.Contains(payload, tool.wire) {
		t.Errorf("%s: flattened wire name leaked to the client: %s", model.id, payload)
	}
}

func codeModeManager(t *testing.T, host *codeModeHost) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(codeModeCatalogBody(), host.call)
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

// TestCodeModeMatrixCatalogDiscovery pins that every live matrix ID is
// discovered and routed to its documented protocol, and that a legacy ID which
// left the live catalog may be unroutable without failing the matrix.
func TestCodeModeMatrixCatalogDiscovery(t *testing.T) {
	host := &codeModeHost{t: t}
	m, _ := codeModeManager(t, host)
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	if mgr == nil {
		t.Fatal("register did not publish a catalog manager")
	}
	byID := map[string]catalog.ModelRecord{}
	for _, rec := range mgr.Models() {
		byID[rec.UpstreamID] = rec
	}
	for _, want := range codeModeMatrixModels {
		rec, ok := byID[want.id]
		if !ok {
			if want.cohort == codeModeLegacy {
				t.Logf("legacy ID %s is no longer routable (accepted)", want.id)
				continue
			}
			t.Errorf("live ID %s is not published by discovery", want.id)
			continue
		}
		if rec.Protocol != want.route {
			t.Errorf("%s: protocol = %q, want %q", want.id, rec.Protocol, want.route)
		}
		if wantEndpoint := codeModeEndpoint(want.route, want.id); rec.EndpointPath != wantEndpoint {
			t.Errorf("%s: endpoint = %q, want %q", want.id, rec.EndpointPath, wantEndpoint)
		}
		if rec.PublicID != "opencode-go/"+want.id {
			t.Errorf("%s: public id = %q", want.id, rec.PublicID)
		}
	}
	for _, unsupported := range mgr.Unsupported() {
		for _, model := range codeModeMatrixModels {
			if model.id == unsupported.UpstreamID && model.cohort == codeModeLive {
				t.Errorf("live model %s excluded from routing: %s", model.id, unsupported.Reason)
			}
		}
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	published := map[string]bool{}
	for _, mi := range static.Models {
		published[mi.ID] = true
	}
	for _, want := range codeModeMatrixModels {
		if want.cohort == codeModeLive && !published["opencode-go/"+want.id] {
			t.Errorf("live ID %s missing from model.static", want.id)
		}
	}
}

// codeModeEndpoint is the endpoint path a live matrix model must publish.
func codeModeEndpoint(route catalog.Route, id string) string {
	if route == catalog.RouteGemini {
		return "/v1/models/" + id + ":generateContent"
	}
	return route.EndpointPath()
}

func TestCodeModeMatrixNonStreamRoundTrip(t *testing.T) {
	host := &codeModeHost{t: t}
	m, f := codeModeManager(t, host)
	runner := &codeModeRunner{m: m, f: f, host: host}
	tool := codeModeExecTool()
	round1 := codeModeCall{callID: "call_0000_matrix_r1", code: `text("ROUND-ONE")`}
	round2 := codeModeCall{callID: "call_0000_matrix_r2", code: "const r = await tools.exec_command({ cmd: \"Get-Content -Raw seed.txt\" });\ntext('你好');"}
	for _, model := range codeModeMatrixModels {
		model := model
		t.Run(model.id, func(t *testing.T) {
			env, _ := runner.round(t, model, tool, nil, round1, false)
			if !codeModeRequireRoute(t, model, env) {
				return
			}
			env, _ = runner.round(t, model, tool, &codeModeHistory{tool: tool, call: round1}, round2, false)
			codeModeRequireRoute(t, model, env)
		})
	}
}

func TestCodeModeMatrixStreamRoundTrip(t *testing.T) {
	runners := map[catalog.Route]*codeModeRunner{}
	tool := codeModeExecTool()
	round1 := codeModeCall{callID: "call_0000_matrix_s1", code: `text("ROUND-ONE")`}
	round2 := codeModeCall{callID: "call_0000_matrix_s2", code: "const r = await tools.exec_command({ cmd: \"Get-Content -Raw seed.txt\" });\ntext('你好');"}
	for _, model := range codeModeMatrixModels {
		model := model
		runner, ok := runners[model.route]
		if !ok {
			host := &codeModeHost{t: t}
			m, f := codeModeManager(t, host)
			runner = &codeModeRunner{m: m, f: f, host: host}
			runners[model.route] = runner
		}
		t.Run(model.id, func(t *testing.T) {
			env, _ := runner.round(t, model, tool, nil, round1, true)
			if !codeModeRequireRoute(t, model, env) {
				return
			}
			env, _ = runner.round(t, model, tool, &codeModeHistory{tool: tool, call: round1}, round2, true)
			codeModeRequireRoute(t, model, env)
		})
	}
}

// TestCodeModeResponsesNonGPTCustomNamespaceHistory is the second-round
// contract for the Responses models the plugin normalizes itself
// (grok/muse): a custom tool declared inside a namespace and the
// custom_tool_call/custom_tool_call_output history pair must survive both
// execution modes — flattening to the namespaced wire name upstream and
// restoring name+namespace plus the raw input downstream.
func TestCodeModeResponsesNonGPTCustomNamespaceHistory(t *testing.T) {
	models := []codeModeMatrixModel{
		{"grok-4.6", catalog.RouteResponses, codeModeLive},
		{"grok-4.7", catalog.RouteResponses, codeModeLive},
		{"muse-spark-1.2-contributor", catalog.RouteResponses, codeModeLive},
		{"muse-spark-1.3-contributor", catalog.RouteResponses, codeModeLive},
	}
	host := &codeModeHost{t: t}
	m, f := codeModeManager(t, host)
	runner := &codeModeRunner{m: m, f: f, host: host}
	tool := codeModeNamespacedExecTool()
	round1 := codeModeCall{callID: "call_0000_ns_r1", code: `text("NAMESPACE-ONE")`}
	round2 := codeModeCall{callID: "call_0000_ns_r2", code: "const r = await tools.exec_command({ cmd: \"Get-Content -Raw seed.txt\" });\ntext('你好');"}
	for _, model := range models {
		model := model
		for _, stream := range []bool{false, true} {
			mode := map[bool]string{true: "stream", false: "json"}[stream]
			t.Run(model.id+"/"+mode, func(t *testing.T) {
				env, _ := runner.round(t, model, tool, nil, round1, stream)
				if !codeModeRequireRoute(t, model, env) {
					return
				}
				env, _ = runner.round(t, model, tool, &codeModeHistory{tool: tool, call: round1}, round2, stream)
				codeModeRequireRoute(t, model, env)
			})
		}
	}
}

// TestCodeModeNativeGPTStaysPassthrough pins the exception the matrix must not
// dissolve: for the native GPT models the Code Mode body (custom exec tool and
// custom history) reaches upstream unmodified and the native custom_tool_call
// comes back with its raw input intact.
func TestCodeModeNativeGPTStaysPassthrough(t *testing.T) {
	models := []codeModeMatrixModel{
		{"gpt-5.6-luna", catalog.RouteResponses, codeModeLive},
		{"gpt-6-luna", catalog.RouteResponses, codeModeLive},
	}
	host := &codeModeHost{t: t}
	m, f := codeModeManager(t, host)
	runner := &codeModeRunner{m: m, f: f, host: host}
	tool := codeModeExecTool()
	round1 := codeModeCall{callID: "call_0000_gpt_r1", code: `text("NATIVE-ONE")`}
	for _, model := range models {
		model := model
		for _, stream := range []bool{false, true} {
			mode := map[bool]string{true: "stream", false: "json"}[stream]
			t.Run(model.id+"/"+mode, func(t *testing.T) {
				env, _ := runner.round(t, model, tool, nil, round1, stream)
				codeModeRequireRoute(t, model, env)
			})
		}
	}
}
