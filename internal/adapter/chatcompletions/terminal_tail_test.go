package chatcompletions

import "testing"

func TestValidateTerminalTail(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		fail       bool
	}{
		{name: "empty"},
		{name: "whitespace", tail: "\r\n\n"},
		{name: "optional DONE", tail: "\ndata: [DONE]\r\n\r\n"},
		{name: "DONE without newline", tail: "data: [DONE]"},
		{name: "duplicate DONE", tail: "data: [DONE]\ndata: [DONE]\n", fail: true},
		{name: "late error", tail: "data: {\"error\":{}}\n\n", fail: true},
		{name: "partial late JSON", tail: "data: {\"error\":", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStreamConverter("openai-response")
			input := "data: {\"id\":\"tail\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"id\":\"tail\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n" + tc.tail
			_, done, err := sc.Feed([]byte(input))
			if err != nil || !done {
				t.Fatalf("fixture did not reach terminal: done=%v error=%v", done, err)
			}
			if got := sc.ValidateTerminalTail(); (got != nil) != tc.fail {
				t.Fatalf("tail validation=%v wantFailure=%v", got, tc.fail)
			}
		})
	}
}
