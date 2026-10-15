package gemini

import (
	"bytes"
	"testing"
)

func TestNativeGeminiFourPreservesOpaquePayload(t *testing.T) {
	body := []byte(`{"model":"opencode-go/gemini-4-argon-contributor","cachedContent":"cachedContents/fixture","tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"n":{"type":"integer","default":9007199254740993}}}}]}],"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}},{"fileData":{"mimeType":"application/pdf","fileUri":"https://example.invalid/fixture.pdf"}}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"opaque-gemini-four-signature"}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"n":9007199254740993}}}]}]}`)
	out, err := BuildRequest("gemini-4-argon-contributor", FormatGemini, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range [][]byte{[]byte("opaque-gemini-four-signature"), []byte("9007199254740993"), []byte(`"functionDeclarations"`), []byte(`"parameters"`), []byte(`"inlineData"`), []byte(`"fileData"`), []byte(`"cachedContent"`), []byte(`"functionResponse"`)} {
		if !bytes.Contains(out, marker) {
			t.Errorf("native field/value lost: %s", marker)
		}
	}
	if bytes.Contains(out, []byte("skip_thought_signature_validator")) || bytes.Contains(out, []byte(`"parametersJsonSchema"`)) {
		t.Fatal("native request was normalized unexpectedly")
	}
	out2, err := BuildRequest("gemini-4-argon-contributor", FormatGemini, out, nil)
	if err != nil || !bytes.Equal(out, out2) {
		t.Fatal("native matching model must be a byte-exact no-op")
	}
}
