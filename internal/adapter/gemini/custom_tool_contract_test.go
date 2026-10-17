package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file pins the custom-tool contract of the native Gemini route (FR-005 /
// FR-006) for generic custom tools ("exec"-style) and for the apply_patch
// carrier. CPA's built-in openai-response<->gemini transformers own the
// conversion; the adapter forwards the client body and the translated Gemini
// body untouched, so these tests pin the end-to-end behaviour a Codex client
// sees on this route:
//
//   - a generic custom tool becomes a functionDeclaration whose schema
//     requires a single string `input` property, which is the same convention
//     the sibling routes implement through shared.ResponseTools.Normalize;
//   - replayed custom_tool_call / custom_tool_call_output history becomes a
//     functionCall/functionResponse pair carrying the raw input in args.input;
//   - a Gemini functionCall recovers the client's custom_tool_call with the
//     raw input restored byte-for-byte: namespace children, multiple calls per
//     result, empty and JSON-looking inputs, and inputs split across transport
//     chunks;
//   - the custom apply_patch carrier keeps its own schema and grammar
//     description instead of being dropped or rewritten.

// customExecInputs covers the raw text shapes a custom tool input takes:
// ordinary code with non-ASCII text and CRLF/backslash content, an empty
// input, and input that is itself a JSON document.
var customExecInputs = []string{
	"print(\"你好\")\r\n\tpath = \"C:\\\\tmp\\\\文件\"",
	"",
	`{"n":1,"s":"值","nested":{"a":[1,2]}}`,
}

// applyPatchFixture is one complete patch in the Codex apply_patch grammar.
const applyPatchFixture = "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"

func objectAt(t *testing.T, value any, what string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s: got %T, want an object", what, value)
	}
	return object
}

func arrayAt(t *testing.T, value any, what string) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("%s: got %T, want an array", what, value)
	}
	return array
}

func stringAt(t *testing.T, value any, what string) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s: got %T, want a string", what, value)
	}
	return text
}

func jsonBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

func textMessage(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func customCall(callID, name, input string) map[string]any {
	return map[string]any{"type": "custom_tool_call", "call_id": callID, "name": name, "input": input}
}

func customOutput(callID, output string) map[string]any {
	return map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": output}
}

// namespacedCustomTool is one `functions` namespace holding a single custom
// child, the Code CLI's exec shape.
func namespacedCustomTool(name string) []any {
	return []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
		map[string]any{"type": "custom", "name": name},
	}}}
}

// customExecFunctionsBody replays a custom exec call and its receipt, which is
// the shape a Codex turn produces after executing one.
func customExecHistoryBody() map[string]any {
	return map[string]any{
		"model": clientModel,
		"input": []any{
			textMessage("run code"),
			customCall("ctc_1", "exec", customExecInputs[0]),
			customOutput("ctc_1", "ok"),
			customCall("ctc_2", "exec", customExecInputs[1]),
			customOutput("ctc_2", "empty ok"),
			customCall("ctc_3", "exec", customExecInputs[2]),
			customOutput("ctc_3", "json ok"),
		},
		"tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Run code"}},
	}
}

// geminiFunctionInputs collects the functionCall arguments this adapter sent
// upstream, in order, as the raw args objects.
func geminiFunctionInputs(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	root := decode(t, body)
	var args []map[string]any
	for _, content := range arrayAt(t, root["contents"], "contents") {
		for _, part := range arrayAt(t, objectAt(t, content, "content")["parts"], "parts") {
			object := objectAt(t, part, "part")
			if call, ok := object["functionCall"].(map[string]any); ok {
				args = append(args, objectAt(t, call["args"], "functionCall.args"))
			}
		}
	}
	return args
}

// geminiFunctionResponseNames collects functionResponse.name values in order.
func geminiFunctionResponseNames(t *testing.T, body []byte) []string {
	t.Helper()
	root := decode(t, body)
	var names []string
	for _, content := range arrayAt(t, root["contents"], "contents") {
		for _, part := range arrayAt(t, objectAt(t, content, "content")["parts"], "parts") {
			object := objectAt(t, part, "part")
			if response, ok := object["functionResponse"].(map[string]any); ok {
				names = append(names, stringAt(t, response["name"], "functionResponse.name"))
			}
		}
	}
	return names
}

// geminiFunctionResponseResults collects functionResponse.response.result
// values in order, which is where a replayed custom_tool_call_output lands.
func geminiFunctionResponseResults(t *testing.T, body []byte) []string {
	t.Helper()
	root := decode(t, body)
	var results []string
	for _, content := range arrayAt(t, root["contents"], "contents") {
		for _, part := range arrayAt(t, objectAt(t, content, "content")["parts"], "parts") {
			object := objectAt(t, part, "part")
			response, ok := object["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			payload := objectAt(t, response["response"], "functionResponse.response")
			results = append(results, stringAt(t, payload["result"], "functionResponse.response.result"))
		}
	}
	return results
}

// firstFunctionDeclaration returns tools.0.functionDeclarations.0 as an object.
func firstFunctionDeclaration(t *testing.T, body []byte) map[string]any {
	t.Helper()
	root := decode(t, body)
	declarations := arrayAt(t, nested(t, root, "tools", "0", "functionDeclarations"), "functionDeclarations")
	if len(declarations) == 0 {
		t.Fatalf("no function declaration was produced: %s", body)
	}
	return objectAt(t, declarations[0], "functionDeclarations.0")
}

// TestBuildRequestCustomExecDeclarationAndHistoryForGemini proves a generic
// custom tool reaches Gemini as a functionDeclaration that requires the string
// `input` wrapper, its tool_choice maps, and the replayed history becomes
// functionCall/functionResponse pairs carrying the raw input unchanged.
func TestBuildRequestCustomExecDeclarationAndHistoryForGemini(t *testing.T) {
	body := customExecHistoryBody()
	body["tool_choice"] = map[string]any{"type": "custom", "name": "exec"}
	out, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, jsonBody(t, body), nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}

	declaration := firstFunctionDeclaration(t, out)
	if name := stringAt(t, declaration["name"], "declaration name"); name != "exec" {
		t.Fatalf("declaration name = %q, want exec", name)
	}
	if description := stringAt(t, declaration["description"], "declaration description"); !strings.Contains(description, "Run code") {
		t.Fatalf("tool description was lost: %q", description)
	}
	schema := objectAt(t, declaration["parametersJsonSchema"], "parametersJsonSchema")
	properties := objectAt(t, schema["properties"], "schema properties")
	input := objectAt(t, properties["input"], "input property")
	if inputType := stringAt(t, input["type"], "input type"); inputType != "string" {
		t.Fatalf("custom input property type = %q, want string", inputType)
	}
	required := arrayAt(t, schema["required"], "schema required")
	if len(required) != 1 || required[0] != "input" {
		t.Fatalf("custom tool schema required = %v, want [input]", required)
	}

	root := decode(t, out)
	mode := stringAt(t, nested(t, root, "toolConfig", "functionCallingConfig", "mode"), "functionCallingConfig.mode")
	if mode != "ANY" {
		t.Fatalf("tool_choice mode = %q, want ANY", mode)
	}
	allowed := arrayAt(t, nested(t, root, "toolConfig", "functionCallingConfig", "allowedFunctionNames"), "allowedFunctionNames")
	if len(allowed) != 1 || allowed[0] != "exec" {
		t.Fatalf("allowedFunctionNames = %v, want [exec]", allowed)
	}

	args := geminiFunctionInputs(t, out)
	if len(args) != len(customExecInputs) {
		t.Fatalf("functionCall count = %d, want %d", len(args), len(customExecInputs))
	}
	for i, want := range customExecInputs {
		if got := stringAt(t, args[i]["input"], "functionCall.args.input"); got != want {
			t.Fatalf("history input %d = %q, want %q", i, got, want)
		}
	}
	responses := geminiFunctionResponseNames(t, out)
	if len(responses) != len(customExecInputs) {
		t.Fatalf("functionResponse count = %d, want %d", len(responses), len(customExecInputs))
	}
	for i, name := range responses {
		if name != "exec" {
			t.Fatalf("functionResponse %d name = %q, want exec", i, name)
		}
	}
	wantResults := []string{"ok", "empty ok", "json ok"}
	results := geminiFunctionResponseResults(t, out)
	if len(results) != len(wantResults) {
		t.Fatalf("functionResponse count = %d, want %d", len(results), len(wantResults))
	}
	for i, want := range wantResults {
		if results[i] != want {
			t.Fatalf("functionResponse %d result = %q, want %q", i, results[i], want)
		}
	}
	// The client-only type and the shared transport wrapper must not leak: a
	// stacked shared.Normalize pass would add the wrapper text and drop the
	// custom identity before CPA's own converter ran.
	if strings.Contains(string(out), `"type":"custom"`) {
		t.Fatalf("custom tool type leaked upstream: %s", out)
	}
	if strings.Contains(string(out), "Upstream transport: call this as a JSON function") {
		t.Fatalf("shared custom wrapper was stacked onto the SDK converter: %s", out)
	}
}

// TestConvertCustomExecCallsToResponsesNonStream proves a Gemini result with
// multiple functionCalls restores the client's custom_tool_call items with the
// raw input byte-for-byte (including empty and JSON-looking input) while an
// ordinary function call keeps its arguments, and the namespace is restored.
func TestConvertCustomExecCallsToResponsesNonStream(t *testing.T) {
	original := jsonBody(t, map[string]any{
		"model": clientModel,
		"input": []any{textMessage("go")},
		"tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "Run code"},
			map[string]any{"type": "function", "name": "lookup",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
		}}},
	})
	upstreamRequest, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, original, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	result := jsonBody(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "functions__exec", "args": map[string]any{"input": customExecInputs[0]}}},
				map[string]any{"functionCall": map[string]any{"name": "functions__lookup", "args": map[string]any{"q": "x"}}},
				map[string]any{"functionCall": map[string]any{"name": "functions__exec", "args": map[string]any{"input": customExecInputs[1]}}},
			}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 7, "candidatesTokenCount": 3, "totalTokenCount": 10},
	})
	out, eErr := ConvertNonStreamResponseWithContext(FormatOpenAIResp, 200, result, upstreamModel, original, upstreamRequest)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
	}

	root := decode(t, out)
	var customItems []map[string]any
	var functionItems []map[string]any
	for _, item := range arrayAt(t, root["output"], "output") {
		object := objectAt(t, item, "output item")
		switch object["type"] {
		case "custom_tool_call":
			customItems = append(customItems, object)
		case "function_call":
			functionItems = append(functionItems, object)
		}
	}
	if len(customItems) != 2 {
		t.Fatalf("custom_tool_call count = %d, want 2: %s", len(customItems), out)
	}
	if len(functionItems) != 1 {
		t.Fatalf("function_call count = %d, want 1: %s", len(functionItems), out)
	}
	wantInputs := []string{customExecInputs[0], customExecInputs[1]}
	seenCallIDs := map[string]bool{}
	for i, item := range customItems {
		if name := stringAt(t, item["name"], "custom name"); name != "exec" {
			t.Fatalf("custom_tool_call %d name = %q, want exec", i, name)
		}
		if namespace := stringAt(t, item["namespace"], "custom namespace"); namespace != "functions" {
			t.Fatalf("custom_tool_call %d namespace = %q, want functions", i, namespace)
		}
		if input := stringAt(t, item["input"], "custom input"); input != wantInputs[i] {
			t.Fatalf("custom_tool_call %d input = %q, want %q", i, input, wantInputs[i])
		}
		callID := stringAt(t, item["call_id"], "custom call_id")
		if callID == "" || seenCallIDs[callID] {
			t.Fatalf("custom_tool_call %d call_id = %q, want a unique non-empty id", i, callID)
		}
		seenCallIDs[callID] = true
	}
	lookup := functionItems[0]
	if name := stringAt(t, lookup["name"], "function name"); name != "lookup" {
		t.Fatalf("function_call name = %q, want lookup", name)
	}
	if namespace := stringAt(t, lookup["namespace"], "function namespace"); namespace != "functions" {
		t.Fatalf("function_call namespace = %q, want functions", namespace)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(stringAt(t, lookup["arguments"], "function arguments")), &arguments); err != nil {
		t.Fatalf("function_call arguments are not JSON: %v", err)
	}
	if arguments["q"] != "x" {
		t.Fatalf("function_call arguments = %v, want q=x", arguments)
	}
	if strings.Contains(string(out), "functions__exec") || strings.Contains(string(out), "functions__lookup") {
		t.Fatalf("flattened wire name leaked to the client: %s", out)
	}
}

// ssePayloads extracts every `data:` JSON payload from emitted SSE frames.
func ssePayloads(t *testing.T, frames [][]byte) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, frame := range frames {
		for _, block := range strings.Split(string(frame), "\n\n") {
			for _, line := range strings.Split(block, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					continue
				}
				var decoded map[string]any
				if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
					t.Fatalf("SSE payload is not JSON: %v (%s)", err, payload)
				}
				payloads = append(payloads, decoded)
			}
		}
	}
	return payloads
}

// TestConvertCustomExecCallsToResponsesStream drives the same namespace custom
// call through the streaming converter fed one byte at a time, so the raw
// custom input (including multi-byte runes) crosses transport chunk boundaries
// and must still be restored exactly, with one terminal event.
func TestConvertCustomExecCallsToResponsesStream(t *testing.T) {
	original := jsonBody(t, map[string]any{
		"model": clientModel,
		"input": []any{textMessage("go")},
		"tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "custom", "name": "exec"},
			map[string]any{"type": "function", "name": "lookup",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
		}}},
	})
	upstreamRequest, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, original, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	callFrame := "data: " + string(jsonBody(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "functions__exec", "args": map[string]any{"input": customExecInputs[0]}}},
				map[string]any{"functionCall": map[string]any{"name": "functions__lookup", "args": map[string]any{"q": "x"}}},
			}},
		}},
	})) + "\n\n"
	finishFrame := "data: " + string(jsonBody(t, map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 7, "candidatesTokenCount": 3, "totalTokenCount": 10},
	})) + "\n\n"

	sc := NewStreamConverterWithContext(FormatOpenAIResp, upstreamModel, original, upstreamRequest)
	var frames [][]byte
	for i := 0; i < len(callFrame); i++ {
		events, _, eErr := sc.Feed([]byte(callFrame[i : i+1]))
		if eErr != nil {
			t.Fatalf("Feed(byte %d) failed: %v", i, eErr)
		}
		frames = append(frames, events...)
	}
	events, _, eErr := sc.Feed([]byte(finishFrame))
	if eErr != nil {
		t.Fatalf("Feed(finish frame) failed: %v", eErr)
	}
	frames = append(frames, events...)
	tail, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish failed: %v", eErr)
	}
	frames = append(frames, tail...)

	payloads := ssePayloads(t, frames)
	var inputDone []string
	var customItems []map[string]any
	var functionItems []map[string]any
	completed := 0
	inputDoneAt, customDoneAt := -1, -1
	for position, payload := range payloads {
		switch payload["type"] {
		case "response.custom_tool_call_input.done":
			inputDone = append(inputDone, stringAt(t, payload["input"], "input.done input"))
			if inputDoneAt < 0 {
				inputDoneAt = position
			}
		case "response.output_item.done":
			item := objectAt(t, payload["item"], "output item")
			switch item["type"] {
			case "custom_tool_call":
				customItems = append(customItems, item)
				if customDoneAt < 0 {
					customDoneAt = position
				}
			case "function_call":
				functionItems = append(functionItems, item)
			}
		case "response.completed":
			completed++
		}
	}
	if len(inputDone) != 1 || inputDone[0] != customExecInputs[0] {
		t.Fatalf("custom input.done = %q, want exactly %q", inputDone, customExecInputs[0])
	}
	if len(customItems) != 1 {
		t.Fatalf("streamed custom_tool_call count = %d, want 1", len(customItems))
	}
	customItem := customItems[0]
	if name := stringAt(t, customItem["name"], "custom name"); name != "exec" {
		t.Fatalf("streamed custom name = %q, want exec", name)
	}
	if namespace := stringAt(t, customItem["namespace"], "custom namespace"); namespace != "functions" {
		t.Fatalf("streamed custom namespace = %q, want functions", namespace)
	}
	if input := stringAt(t, customItem["input"], "custom input"); input != customExecInputs[0] {
		t.Fatalf("streamed custom input = %q, want %q", input, customExecInputs[0])
	}
	if len(functionItems) != 1 {
		t.Fatalf("streamed function_call count = %d, want 1", len(functionItems))
	}
	if name := stringAt(t, functionItems[0]["name"], "function name"); name != "lookup" {
		t.Fatalf("streamed function name = %q, want lookup", name)
	}
	if completed != 1 {
		t.Fatalf("response.completed count = %d, want 1", completed)
	}
	if inputDoneAt < 0 || customDoneAt < 0 || inputDoneAt > customDoneAt {
		t.Fatalf("custom input.done (%d) must precede output_item.done (%d)", inputDoneAt, customDoneAt)
	}
}

// TestConvertLongNamespacedCustomToolRoundTrip covers the name funnel a long
// namespace child goes through: the qualified `namespace__child` name exceeds
// Gemini's 64-character limit, so the upstream declaration is the sanitized
// form, and the model echoes that form back. The client must still see its
// original local name and namespace with the custom identity intact.
func TestConvertLongNamespacedCustomToolRoundTrip(t *testing.T) {
	localName := strings.Repeat("a", 60) // functions__ + 60 chars = 71 > 64
	original := jsonBody(t, map[string]any{
		"model": clientModel,
		"input": []any{textMessage("go")},
		"tools": namespacedCustomTool(localName),
	})
	upstreamRequest, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, original, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	declared := stringAt(t, firstFunctionDeclaration(t, upstreamRequest)["name"], "declaration name")
	if declared == "" || len(declared) > 64 {
		t.Fatalf("declared name %q is not a valid Gemini function name", declared)
	}
	if declared == "functions__"+localName {
		t.Fatalf("long qualified name was not sanitized: %q", declared)
	}

	result := jsonBody(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": declared, "args": map[string]any{"input": "long ok"}}},
			}},
			"finishReason": "STOP",
		}},
	})
	out, eErr := ConvertNonStreamResponseWithContext(FormatOpenAIResp, 200, result, upstreamModel, original, upstreamRequest)
	if eErr != nil {
		t.Fatalf("ConvertNonStreamResponseWithContext failed: %v", eErr)
	}
	root := decode(t, out)
	items := arrayAt(t, root["output"], "output")
	if len(items) != 1 {
		t.Fatalf("output item count = %d, want 1: %s", len(items), out)
	}
	item := objectAt(t, items[0], "output item")
	if itemType := stringAt(t, item["type"], "item type"); itemType != "custom_tool_call" {
		t.Fatalf("item type = %q, want custom_tool_call", itemType)
	}
	if name := stringAt(t, item["name"], "item name"); name != localName {
		t.Fatalf("restored name = %q, want the original local name %q", name, localName)
	}
	if namespace := stringAt(t, item["namespace"], "item namespace"); namespace != "functions" {
		t.Fatalf("restored namespace = %q, want functions", namespace)
	}
	if input := stringAt(t, item["input"], "item input"); input != "long ok" {
		t.Fatalf("restored input = %q, want long ok", input)
	}
}

// TestConvertArgslessCustomCallToResponsesStream covers a zero-argument custom
// call: Gemini may omit `args` entirely, and the client must still receive a
// custom_tool_call with an empty input instead of a dropped or malformed item.
func TestConvertArgslessCustomCallToResponsesStream(t *testing.T) {
	original := jsonBody(t, map[string]any{
		"model": clientModel,
		"input": []any{textMessage("go")},
		"tools": namespacedCustomTool("exec"),
	})
	upstreamRequest, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, original, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}
	frame := "data: " + string(jsonBody(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "functions__exec", "args": map[string]any{"input": "first()"}}},
				map[string]any{"functionCall": map[string]any{"name": "functions__exec"}},
			}},
			"finishReason": "STOP",
		}},
	})) + "\n\n"

	sc := NewStreamConverterWithContext(FormatOpenAIResp, upstreamModel, original, upstreamRequest)
	frames, _, eErr := sc.Feed([]byte(frame))
	if eErr != nil {
		t.Fatalf("Feed failed: %v", eErr)
	}
	tail, eErr := sc.Finish()
	if eErr != nil {
		t.Fatalf("Finish failed: %v", eErr)
	}
	frames = append(frames, tail...)

	var inputs []string
	for _, payload := range ssePayloads(t, frames) {
		if payload["type"] != "response.custom_tool_call_input.done" {
			continue
		}
		inputs = append(inputs, stringAt(t, payload["input"], "input.done input"))
	}
	if len(inputs) != 2 || inputs[0] != "first()" || inputs[1] != "" {
		t.Fatalf("streamed custom inputs = %q, want [first() and an empty input]", inputs)
	}
}

// TestBuildRequestKeepsApplyPatchCarrierForGemini proves the custom
// apply_patch declaration keeps its own schema and patch instructions on the
// Gemini route — the carrier the sibling shared normalization deliberately
// drops for chat/messages targets must survive here, and its replayed history
// keeps the raw patch text in args.input.
func TestBuildRequestKeepsApplyPatchCarrierForGemini(t *testing.T) {
	body := jsonBody(t, map[string]any{
		"model": clientModel,
		"input": []any{
			textMessage("patch it"),
			customCall("ctc_patch", "apply_patch", applyPatchFixture),
			customOutput("ctc_patch", "done"),
		},
		"tools": []any{map[string]any{
			"type": "custom", "name": "apply_patch",
			"description": "Edit files. This is a FREEFORM tool, so do not wrap the patch in JSON.",
			"format":      map[string]any{"type": "grammar", "syntax": "lark", "definition": "*** Begin Patch"},
		}},
	})
	out, eErr := BuildRequest(upstreamModel, FormatOpenAIResp, body, nil)
	if eErr != nil {
		t.Fatalf("BuildRequest failed: %v", eErr)
	}

	declaration := firstFunctionDeclaration(t, out)
	if name := stringAt(t, declaration["name"], "declaration name"); name != "apply_patch" {
		t.Fatalf("apply_patch declaration name = %q, want apply_patch", name)
	}
	schema := objectAt(t, declaration["parametersJsonSchema"], "apply_patch parametersJsonSchema")
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("apply_patch schema additionalProperties = %v, want false", schema["additionalProperties"])
	}
	required := arrayAt(t, schema["required"], "apply_patch required")
	if len(required) != 1 || required[0] != "input" {
		t.Fatalf("apply_patch required = %v, want [input]", required)
	}
	description := stringAt(t, declaration["description"], "apply_patch description")
	for _, want := range []string{"Edit files", "*** Begin Patch", "*** End Patch", "*** Update File: path"} {
		if !strings.Contains(description, want) {
			t.Fatalf("apply_patch description lost %q: %s", want, description)
		}
	}
	if strings.Contains(description, "FREEFORM tool") || strings.Contains(description, "Upstream transport: call this as a JSON function") {
		t.Fatalf("apply_patch description was rewritten by the shared wrapper: %s", description)
	}

	args := geminiFunctionInputs(t, out)
	if len(args) != 1 {
		t.Fatalf("apply_patch history functionCall count = %d, want 1", len(args))
	}
	if got := stringAt(t, args[0]["input"], "apply_patch args.input"); got != applyPatchFixture {
		t.Fatalf("apply_patch history input = %q, want %q", got, applyPatchFixture)
	}
	responses := geminiFunctionResponseNames(t, out)
	if len(responses) != 1 || responses[0] != "apply_patch" {
		t.Fatalf("apply_patch functionResponse = %v, want [apply_patch]", responses)
	}
}
