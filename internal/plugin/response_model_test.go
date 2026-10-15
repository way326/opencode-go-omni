package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestClientResponseModelMetadataOnly(t *testing.T) {
	res := &resolvedExecution{clientModel: "opencode-go/muse-spark-1.3-contributor", model: "muse-spark-1.3-contributor"}
	for _, tc := range []struct{ name, format, path, body string }{
		{"chat", "openai", "model", `{"model":"muse-spark-1.3-contributor","choices":[]}`},
		{"responses", "openai-response", "model", `{"model":"muse-spark-1.3-contributor","usage":{"input_tokens":9007199254740993},"output":[{"type":"function_call","arguments":"{\"model\":\"muse-spark-1.3-contributor\"}"}],"thoughtSignature":"AQID"}`},
		{"response-event", "openai-response", "response.model", `{"type":"response.completed","response":{"model":"muse-spark-1.3-contributor"}}`},
		{"message", "claude", "model", `{"model":"muse-spark-1.3-contributor","content":[]}`},
		{"message-start", "claude", "message.model", `{"type":"message_start","message":{"model":"muse-spark-1.3-contributor"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clientResponseModel(res, tc.format, []byte(tc.body))
			if gjson.GetBytes(got, tc.path).String() != res.clientModel {
				t.Fatalf("model not restored: %s", got)
			}
			// All bytes outside the exact metadata value stay unchanged.
			want := strings.Replace(tc.body, `"model":"`+res.model+`"`, `"model":"`+res.clientModel+`"`, 1)
			if string(got) != want {
				t.Fatalf("unrelated data changed:\n%s\nwant:\n%s", got, want)
			}
		})
	}
	for _, body := range []string{
		`{"model":"different-model"}`, `{"model":"muse-spark-1.3-contributor-20261015"}`,
		`{"model":"opencode-go/muse-spark-1.3-contributor"}`, `{"modelVersion":"muse-spark-1.3-contributor"}`,
		`{"output":[{"model":"muse-spark-1.3-contributor"}]}`, `{"model":null}`, `not JSON`,
	} {
		if got := clientResponseModel(res, "openai-response", []byte(body)); string(got) != body {
			t.Fatalf("unexpected model must remain visible: %s", got)
		}
	}
	gemini := []byte(`{"model":"muse-spark-1.3-contributor","modelVersion":"v1"}`)
	if !bytes.Equal(clientResponseModel(res, "gemini", gemini), gemini) {
		t.Fatal("native Gemini metadata must pass through")
	}
}

func TestClientResponseModelSSEFraming(t *testing.T) {
	res := &resolvedExecution{clientModel: "opencode-go/muse", model: "muse"}
	stream := ": heartbeat\r\n\r\nid: 1\r\nretry: 1000\r\nevent: response.created\r\ndata: {\"response\":{\r\ndata: \"model\":\"muse\",\"usage\":9007199254740993}}\r\n\r\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"muse\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"model\":\"muse\"}}\n\n" +
		"data: [DONE]\n\n: trailing"
	got := string(clientResponseModel(res, "openai-response", []byte(stream)))
	if strings.Count(got, `"model":"opencode-go/muse"`) != 2 || !strings.Contains(got, "9007199254740993") {
		t.Fatalf("model/usage identity lost: %s", got)
	}
	for _, preserved := range []string{": heartbeat\r\n\r\n", "id: 1\r\nretry: 1000\r\nevent: response.created\r\n", `data: {"delta":"muse"}`, "data: [DONE]\n\n: trailing"} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("SSE metadata changed: %q", preserved)
		}
	}
	for _, body := range []string{"data: {\"model\":\"different\"}\n\n", "data: [DONE]\n\n", "event: response.created\ndata: {", ": heartbeat\n\n"} {
		if string(clientResponseModel(res, "openai-response", []byte(body))) != body {
			t.Fatalf("untouched SSE changed: %q", body)
		}
	}
}

func TestExecutorRestoresClientModelWithoutChangingUpstream(t *testing.T) {
	for _, tc := range []struct{ model, format, body string }{
		{"glm-5.3", "openai", ccRequestBody},
		{"minimax-m3", "claude", claudeRequestBody},
		{"gpt-5.6-luna", "openai-response", `{"input":"hi"}`},
		{"glm-5.3", "openai-response", `{"input":"hi"}`},
	} {
		t.Run(tc.model+"/"+tc.format, func(t *testing.T) {
			m, f := newExecManager(t)
			requested := "opencode-go/" + tc.model
			env := mustExecute(t, m, requested, tc.format, []byte(tc.body))
			if !env.OK {
				t.Fatalf("execute failed: %+v", env.Error)
			}
			var response pluginapi.ExecutorResponse
			if err := json.Unmarshal(env.Result, &response); err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(response.Payload, "model").String() != requested {
				t.Fatalf("client model lost: %s", response.Payload)
			}
			wire := wireBody(t, lastWire(t, f, pluginabi.MethodHostHTTPDo), "body")
			if gjson.GetBytes(wire, "model").String() != tc.model {
				t.Fatalf("public prefix leaked upstream: %s", wire)
			}
		})
	}
}

func TestExecutorRestoresClientModelOnFragmentedResponsesStream(t *testing.T) {
	created := "event: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-luna\"}}\r\n\r\n"
	terminal := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-luna\",\"usage\":{\"input_tokens\":1}}}\n\n"
	m, f := newStreamManager(t, streamScript{upstreamID: "up-model", frames: []string{created[:9], created[9:], terminal}})
	raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, execStreamReqBody("opencode-go/gpt-5.6-luna", "openai-response", []byte(`{"input":"hi"}`), "down-model"))
	if err != nil || !decodeEnv(t, raw).OK {
		t.Fatalf("execute stream: %s, %v", raw, err)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	var output bytes.Buffer
	for _, call := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		output.Write(wireBody(t, decodePayload(t, call), "payload"))
	}
	if strings.Count(output.String(), `"model":"opencode-go/gpt-5.6-luna"`) != 2 {
		t.Fatalf("stream model not restored on every response event: %s", output.String())
	}
	if !strings.Contains(output.String(), `"usage":{"input_tokens":1}`) {
		t.Fatal("usage was lost")
	}
	request := wireBody(t, lastWire(t, f, pluginabi.MethodHostHTTPDoStream), "body")
	if gjson.GetBytes(request, "model").String() != "gpt-5.6-luna" {
		t.Fatal("upstream request model changed")
	}
}
