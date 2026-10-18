package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type shutdownCallResult struct {
	response []byte
	err      error
}

func startShutdown(m *Manager) <-chan shutdownCallResult {
	done := make(chan shutdownCallResult, 1)
	go func() {
		response, err := m.HandleCall(pluginabi.MethodPluginShutdown, nil)
		done <- shutdownCallResult{response: response, err: err}
	}()
	return done
}

func awaitShutdown(t *testing.T, done <-chan shutdownCallResult) shutdownCallResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil || !decodeEnv(t, result.response).OK {
			t.Fatalf("shutdown result = %s, err=%v", result.response, result.err)
		}
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after all held callbacks were released")
		return shutdownCallResult{}
	}
}

func requireShutdownBlocked(t *testing.T, done <-chan shutdownCallResult) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("shutdown returned while work was still held: %s, err=%v", result.response, result.err)
	case <-time.After(100 * time.Millisecond):
	}
}

func setShutdownDrainForTest(t *testing.T, timeout time.Duration) {
	t.Helper()
	old := shutdownDrainTimeout
	shutdownDrainTimeout = timeout
	t.Cleanup(func() { shutdownDrainTimeout = old })
}

func waitBridgeClosing(t *testing.T, bridge *HostBridge) {
	t.Helper()
	waitFor(t, "bridge admission to close", func() bool { return bridge.isClosing() })
}

func TestShutdownGateWaitsForCallbackAndAllowsFreshRegistration(t *testing.T) {
	setShutdownDrainForTest(t, 25*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHost := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHost)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			var req hostHTTPReq
			_ = json.Unmarshal(payload, &req)
			if strings.HasSuffix(req.URL, "/models") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(multiRouteCatalog)}), nil
			}
			close(entered)
			<-release
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, doErr := m.bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://slow.test"})
	cancel()
	if doErr == nil || !strings.Contains(doErr.Error(), "timed out") {
		t.Fatalf("host call error = %v, want timeout", doErr)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fake host callback did not enter")
	}

	shutdownDone := startShutdown(m)
	waitBridgeClosing(t, m.bridge)
	requireShutdownBlocked(t, shutdownDone)
	response, err := m.HandleCall(pluginabi.MethodAuthIdentifier, nil)
	if err != nil || decodeEnv(t, response).Error == nil || decodeEnv(t, response).Error.Code != "plugin_shutting_down" {
		t.Fatalf("new RPC during drain = %s, err=%v; want plugin_shutting_down", response, err)
	}
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		response, err := m.HandleCall(method, lifecycleRequestBody(testValidYAML))
		env := decodeEnv(t, response)
		if err != nil || env.OK || env.Error == nil || env.Error.Code != "plugin_shutting_down" {
			t.Fatalf("lifecycle admitted during drain: %s = %s, err=%v", method, response, err)
		}
	}
	releaseHost()
	awaitShutdown(t, shutdownDone)
	if !m.bridge.WaitForInFlight(time.Second) {
		t.Fatal("callback accounting did not drain after shutdown")
	}

	response, err = m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))
	if err != nil || !decodeEnv(t, response).OK {
		t.Fatalf("register after completed shutdown = %s, err=%v", response, err)
	}
	response, err = m.HandleCall(pluginabi.MethodAuthIdentifier, nil)
	if err != nil || !decodeEnv(t, response).OK {
		t.Fatalf("RPC after fresh registration = %s, err=%v", response, err)
	}
	if _, err := m.HandleCall(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("second shutdown after re-register: %v", err)
	}
}

func TestShutdownClosesLateDoStreamResult(t *testing.T) {
	setShutdownDrainForTest(t, 25*time.Millisecond)
	entered := make(chan struct{})
	releaseStart := make(chan struct{})
	closed := make(chan string, 1)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			close(entered)
			<-releaseStart
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "late-upstream"}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			var req hostStreamIDReq
			_ = json.Unmarshal(payload, &req)
			closed <- req.StreamID
		}
		return hostOK(map[string]any{}), nil
	}}
	bridge := NewHostBridge(f.call)
	m := NewManager(bridge)
	type startResult struct{ err error }
	startDone := make(chan startResult, 1)
	go func() {
		_, _, _, err := bridge.DoStream(context.Background(), pluginapi.HTTPRequest{Method: http.MethodPost, URL: "https://slow.test"})
		startDone <- startResult{err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("do_stream callback did not enter")
	}

	shutdownDone := startShutdown(m)
	waitBridgeClosing(t, bridge)
	requireShutdownBlocked(t, shutdownDone)
	close(releaseStart)
	select {
	case result := <-startDone:
		if result.err == nil || !strings.Contains(result.err.Error(), "shutting down") {
			t.Fatalf("late do_stream result error = %v, want shutdown rejection", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("late do_stream start did not return")
	}
	select {
	case id := <-closed:
		if id != "late-upstream" {
			t.Fatalf("closed stream ID = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("late upstream stream was not closed")
	}
	awaitShutdown(t, shutdownDone)
}

func TestShutdownCancelsEveryRegisteredStreamConcurrently(t *testing.T) {
	setShutdownDrainForTest(t, 25*time.Millisecond)
	readStarted := make(chan string, 2)
	closeStarted := make(chan string, 2)
	releaseCloses := make(chan struct{})
	var releaseReadOnce sync.Once
	releaseReads := make(chan struct{})
	var startMu sync.Mutex
	startCount := 0
	var closeMu sync.Mutex
	closeCount := 0
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var req hostHTTPReq
			_ = json.Unmarshal(payload, &req)
			if strings.HasSuffix(req.URL, "/models") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(multiRouteCatalog)}), nil
			}
		case pluginabi.MethodHostHTTPDoStream:
			startMu.Lock()
			startCount++
			id := startCount
			startMu.Unlock()
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: string(rune('0' + id))}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			var req hostStreamIDReq
			_ = json.Unmarshal(payload, &req)
			readStarted <- req.StreamID
			<-releaseReads
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			var req hostStreamIDReq
			_ = json.Unmarshal(payload, &req)
			closeStarted <- req.StreamID
			<-releaseCloses
			closeMu.Lock()
			closeCount++
			if closeCount == 2 {
				releaseReadOnce.Do(func() { close(releaseReads) })
			}
			closeMu.Unlock()
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, f.responder)
	if response, err := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)); err != nil || !decodeEnv(t, response).OK {
		t.Fatalf("register: %s, err=%v", response, err)
	}
	for i := 1; i <= 2; i++ {
		response, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream,
			execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-"+string(rune('0'+i))))
		if err != nil || !decodeEnv(t, response).OK {
			t.Fatalf("execute stream %d: %s, err=%v", i, response, err)
		}
	}
	seenReads := map[string]bool{}
	for range 2 {
		select {
		case id := <-readStarted:
			seenReads[id] = true
		case <-time.After(time.Second):
			t.Fatal("both stream pumps did not enter stream_read")
		}
	}
	if !seenReads["1"] || !seenReads["2"] {
		t.Fatalf("stream reads started for IDs %v", seenReads)
	}

	shutdownDone := startShutdown(m)
	waitBridgeClosing(t, m.bridge)
	for range 2 {
		select {
		case <-closeStarted:
		case <-time.After(time.Second):
			t.Fatal("shutdown serialized stream cleanup behind a blocked close")
		}
	}
	requireShutdownBlocked(t, shutdownDone)
	close(releaseCloses)
	awaitShutdown(t, shutdownDone)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 2 {
		t.Fatalf("upstream stream_close calls = %d, want 2", got)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamClose)); got != 2 {
		t.Fatalf("downstream stream_close calls = %d, want 2", got)
	}
}

func TestShutdownRejectsLifecycleAdmittedBeforeDrain(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		t.Run(method, func(t *testing.T) {
			m, f := newExecManager(t)
			// Capture the entry token of a lifecycle call which has not acquired
			// lifeMu yet. This models the queued caller without scheduler sleeps.
			epoch, admitted := m.lifecycleAdmission()
			if !admitted {
				t.Fatal("lifecycle was not admitted before shutdown")
			}
			awaitShutdown(t, startShutdown(m))
			before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
			response, err := m.handleLifecycleAdmitted(lifecycleRequestBody(testValidYAML), epoch)
			env := decodeEnv(t, response)
			if err != nil || env.OK || env.Error == nil || env.Error.Code != "plugin_shutting_down" {
				t.Fatalf("stale lifecycle = %s, err=%v", response, err)
			}
			if !m.bridge.isClosing() || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != before {
				t.Fatal("stale lifecycle reopened the bridge or contacted the host")
			}
			// A caller entering after the completed shutdown gets the new token.
			response, err = m.HandleCall(method, lifecycleRequestBody(testValidYAML))
			if err != nil || !decodeEnv(t, response).OK || m.bridge.isClosing() {
				t.Fatalf("fresh lifecycle did not reopen: %s, err=%v", response, err)
			}
		})
	}
}
