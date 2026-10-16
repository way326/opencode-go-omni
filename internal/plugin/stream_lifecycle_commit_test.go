package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Commit-path pins for the deferred success terminal (WP-2). The lifecycle
// must publish exactly one result owner, deliver the terminal through the host
// emit callback, and only then end the downstream and reclaim the upstream. A
// failed or timed-out emit stays a real failure, and a request-timeout
// watchdog must never turn a terminal that is already on the wire into an
// error nor drop the shutdown drain below one in-flight owner.

const (
	commitDownID  = "down-commit"
	commitUpID    = "up-commit"
	commitWait    = 5 * time.Second
	commitPayload = `data: {"type":"response.completed","response":{"status":"completed"}}`
)

// commitHostScript records the host-visible commit sequence and lets a test
// park the emit or the upstream close callback.
type commitHostScript struct {
	mu          sync.Mutex
	steps       []string
	emits       [][]byte
	downErrs    []string
	upCloses    int
	onEmit      func() error
	onDownClose func()
	onUpClose   func()
}

func (h *commitHostScript) responder() func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostStreamEmit:
			var req struct {
				StreamID string `json:"stream_id"`
				Payload  []byte `json:"payload"`
			}
			_ = json.Unmarshal(payload, &req)
			h.mu.Lock()
			h.steps = append(h.steps, "emit")
			h.emits = append(h.emits, req.Payload)
			h.mu.Unlock()
			if h.onEmit != nil {
				if err := h.onEmit(); err != nil {
					return nil, err
				}
			}
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			var req struct {
				StreamID string `json:"stream_id"`
				Error    string `json:"error"`
			}
			_ = json.Unmarshal(payload, &req)
			h.mu.Lock()
			h.steps = append(h.steps, "down")
			h.downErrs = append(h.downErrs, req.Error)
			h.mu.Unlock()
			if h.onDownClose != nil {
				h.onDownClose()
			}
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			h.mu.Lock()
			h.steps = append(h.steps, "up")
			h.upCloses++
			h.mu.Unlock()
			if h.onUpClose != nil {
				h.onUpClose()
			}
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
}

func (h *commitHostScript) snapshot() (steps []string, emits [][]byte, downErrs []string, upCloses int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	steps = append([]string(nil), h.steps...)
	emits = append([][]byte(nil), h.emits...)
	downErrs = append([]string(nil), h.downErrs...)
	return steps, emits, downErrs, h.upCloses
}

func newCommitLifecycle(timeout time.Duration, h *commitHostScript) (*streamLifecycle, *fakeCaller, *HostBridge) {
	f := &fakeCaller{responder: h.responder()}
	bridge := NewHostBridge(f.call)
	return newStreamLifecycle(bridge, commitDownID, commitUpID, timeout), f, bridge
}

func commitCallCounts(f *fakeCaller) (emits, downs, ups int) {
	return len(f.callsOf(pluginabi.MethodHostStreamEmit)),
		len(f.callsOf(pluginabi.MethodHostStreamClose)),
		len(f.callsOf(pluginabi.MethodHostHTTPStreamClose))
}

// requireCleanCommit pins the whole successful path: the data-only terminal is
// the only emit, the downstream close is clean and precedes the upstream
// reclaim, and every count -- including the stopped watchdog and the owner
// claim -- drains.
func requireCleanCommit(t *testing.T, lc *streamLifecycle, bridge *HostBridge, f *fakeCaller, h *commitHostScript) {
	t.Helper()
	if !lc.ended() {
		t.Fatalf("ended() = false after a committed terminal")
	}
	steps, emits, downErrs, upCloses := h.snapshot()
	if got := strings.Join(steps, ","); got != "emit,down,up" {
		t.Fatalf("host callback order = %q, want the terminal emit before the clean closes", got)
	}
	if len(emits) != 1 || string(emits[0]) != commitPayload {
		t.Fatalf("emitted payloads = %q, want exactly the data-only terminal %q", emits, commitPayload)
	}
	if len(downErrs) != 1 || downErrs[0] != "" {
		t.Fatalf("downstream close errors = %q, want one clean close", downErrs)
	}
	if upCloses != 1 {
		t.Fatalf("upstream closes = %d, want 1", upCloses)
	}
	if emits, downs, ups := commitCallCounts(f); emits != 1 || downs != 1 || ups != 1 {
		t.Fatalf("host emit/down/up calls = %d/%d/%d, want 1/1/1", emits, downs, ups)
	}
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("in-flight callbacks did not drain after the commit; a lifecycle count leaked")
	}
}

func TestCommitTerminalEmitsTerminalThenClosesDownstreamFirst(t *testing.T) {
	h := &commitHostScript{}
	lc, f, bridge := newCommitLifecycle(time.Hour, h)
	lc.commitTerminal([]byte(commitPayload))
	requireCleanCommit(t, lc, bridge, f, h)
}

func TestStreamLifecycleCommitAndFinishHaveSingleOwner(t *testing.T) {
	t.Run("commit first then finish", func(t *testing.T) {
		h := &commitHostScript{}
		lc, f, _ := newCommitLifecycle(time.Hour, h)
		lc.commitTerminal([]byte(commitPayload))
		lc.finish("stream exceeded request-timeout", "timeout")

		steps, _, downErrs, upCloses := h.snapshot()
		if got := strings.Join(steps, ","); got != "emit,down,up" {
			t.Fatalf("host callback order = %q, want the committed terminal and no second close", got)
		}
		if len(downErrs) != 1 || downErrs[0] != "" {
			t.Fatalf("late finish changed the downstream result: %q", downErrs)
		}
		if upCloses != 1 {
			t.Fatalf("upstream closes = %d, want 1", upCloses)
		}
		if emits, downs, ups := commitCallCounts(f); emits != 1 || downs != 1 || ups != 1 {
			t.Fatalf("host emit/down/up calls = %d/%d/%d, want 1/1/1", emits, downs, ups)
		}
	})
	t.Run("finish first then commit", func(t *testing.T) {
		h := &commitHostScript{}
		lc, f, _ := newCommitLifecycle(time.Hour, h)
		lc.finish("stream exceeded request-timeout", "timeout")
		lc.commitTerminal([]byte(commitPayload))

		steps, emits, downErrs, upCloses := h.snapshot()
		if got := strings.Join(steps, ","); got != "down,up" {
			t.Fatalf("host callback order = %q, want only the timeout closes", got)
		}
		if len(emits) != 0 {
			t.Fatalf("terminal emitted after the stream was already closed: %q", emits)
		}
		if len(downErrs) != 1 || downErrs[0] != "stream exceeded request-timeout" {
			t.Fatalf("downstream close errors = %q, want the watchdog error", downErrs)
		}
		if upCloses != 1 {
			t.Fatalf("upstream closes = %d, want 1", upCloses)
		}
		if emits, downs, ups := commitCallCounts(f); emits != 0 || downs != 1 || ups != 1 {
			t.Fatalf("host emit/down/up calls = %d/%d/%d, want 0/1/1", emits, downs, ups)
		}
	})
}

func TestCommitTerminalEmitFailureClosesWithRealError(t *testing.T) {
	const secret = "test1234"
	h := &commitHostScript{onEmit: func() error {
		return errors.New("host write failed: Authorization: Bearer " + secret)
	}}
	lc, f, bridge := newCommitLifecycle(time.Hour, h)
	lc.commitTerminal([]byte(commitPayload))

	if !lc.ended() {
		t.Fatalf("ended() = false after a failed terminal commit")
	}
	steps, emits, downErrs, upCloses := h.snapshot()
	if got := strings.Join(steps, ","); got != "emit,down,up" {
		t.Fatalf("host callback order = %q, want a failed emit to still end both streams", got)
	}
	if len(emits) != 1 {
		t.Fatalf("emit attempts = %d, want 1", len(emits))
	}
	if len(downErrs) != 1 {
		t.Fatalf("downstream closes = %q, want exactly one", downErrs)
	}
	if msg := downErrs[0]; !strings.Contains(msg, "stream terminal emit failed") {
		t.Fatalf("downstream close error %q must name the failed terminal emit", msg)
	} else if strings.Contains(msg, secret) {
		t.Fatalf("downstream close error leaked a credential: %q", msg)
	}
	if upCloses != 1 {
		t.Fatalf("upstream closes = %d, want 1", upCloses)
	}
	if emits, downs, ups := commitCallCounts(f); emits != 1 || downs != 1 || ups != 1 {
		t.Fatalf("host emit/down/up calls = %d/%d/%d, want 1/1/1", emits, downs, ups)
	}
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("in-flight callbacks did not drain after a failed emit")
	}
}

func TestCommitTerminalEmitDeadlineIsAFailure(t *testing.T) {
	oldEmit := emitTimeout
	emitTimeout = 80 * time.Millisecond
	defer func() { emitTimeout = oldEmit }()

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseEmit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseEmit)

	h := &commitHostScript{onEmit: func() error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}}
	lc, f, bridge := newCommitLifecycle(time.Hour, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lc.commitTerminal([]byte(commitPayload))
	}()

	select {
	case <-entered:
	case <-time.After(commitWait):
		t.Fatalf("terminal emit never entered the host callback")
	}
	select {
	case <-done:
	case <-time.After(commitWait):
		t.Fatalf("commitTerminal did not honor the emit deadline")
	}

	steps, _, downErrs, upCloses := h.snapshot()
	if got := strings.Join(steps, ","); got != "emit,down,up" {
		t.Fatalf("host callback order = %q, want the timed-out emit to end both streams", got)
	}
	if len(downErrs) != 1 || strings.TrimSpace(downErrs[0]) == "" {
		t.Fatalf("a timed-out terminal emit must end downstream with an explicit error, got %q", downErrs)
	}
	if upCloses != 1 {
		t.Fatalf("upstream closes = %d, want 1", upCloses)
	}
	if bridge.WaitForInFlight(100 * time.Millisecond) {
		t.Fatalf("bridge reported drained while the timed-out emit callback was still parked")
	}
	releaseEmit()
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("in-flight callbacks did not drain after the parked emit returned")
	}
	if emits, downs, ups := commitCallCounts(f); emits != 1 || downs != 1 || ups != 1 {
		t.Fatalf("host emit/down/up calls = %d/%d/%d, want 1/1/1", emits, downs, ups)
	}
}

func TestCommitTerminalBlockedEmitSurvivesRequestTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseEmit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseEmit)

	h := &commitHostScript{onEmit: func() error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}}
	lc, f, bridge := newCommitLifecycle(120*time.Millisecond, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lc.commitTerminal([]byte(commitPayload))
	}()

	select {
	case <-entered:
	case <-time.After(commitWait):
		t.Fatalf("terminal emit never entered the host callback")
	}
	// The request-timeout watchdog fires while the emit is parked; because the
	// commit already claimed the result it must not end the stream behind the
	// terminal that is on the wire.
	time.Sleep(300 * time.Millisecond)
	if steps, _, _, upCloses := h.snapshot(); strings.Join(steps, ",") != "emit" || upCloses != 0 {
		t.Fatalf("watchdog ended the stream while the terminal emit was parked: steps=%v upCloses=%d", steps, upCloses)
	}
	releaseEmit()
	select {
	case <-done:
	case <-time.After(commitWait):
		t.Fatalf("commitTerminal did not return after the emit was released")
	}
	requireCleanCommit(t, lc, bridge, f, h)
}

func TestCommitTerminalBlockedUpstreamCloseStillEndsDownstream(t *testing.T) {
	upEntered := make(chan struct{})
	upRelease := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseUp := func() { releaseOnce.Do(func() { close(upRelease) }) }
	t.Cleanup(releaseUp)

	h := &commitHostScript{onUpClose: func() {
		enteredOnce.Do(func() { close(upEntered) })
		<-upRelease
	}}
	lc, f, bridge := newCommitLifecycle(time.Hour, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lc.commitTerminal([]byte(commitPayload))
	}()

	select {
	case <-upEntered:
	case <-time.After(commitWait):
		t.Fatalf("upstream close never entered the host callback")
	}
	steps, _, downErrs, _ := h.snapshot()
	if got := strings.Join(steps, ","); got != "emit,down,up" {
		t.Fatalf("host callback order = %q; the downstream close must be delivered before the wedged upstream close", got)
	}
	if len(downErrs) != 1 || downErrs[0] != "" {
		t.Fatalf("downstream close errors = %q, want one clean close before the upstream reclaim", downErrs)
	}
	if bridge.WaitForInFlight(150 * time.Millisecond) {
		t.Fatalf("bridge reported drained while the upstream close was still wedged")
	}
	releaseUp()
	select {
	case <-done:
	case <-time.After(commitWait):
		t.Fatalf("commitTerminal did not return after the upstream close was released")
	}
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("in-flight callbacks did not drain after the wedged upstream close was released")
	}
	if emits, downs, ups := commitCallCounts(f); emits != 1 || downs != 1 || ups != 1 {
		t.Fatalf("host emit/down/up calls = %d/%d/%d, want 1/1/1", emits, downs, ups)
	}
}

func TestCommitTerminalHoldsShutdownDrainUntilEmitReturns(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseEmit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseEmit)

	h := &commitHostScript{onEmit: func() error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}}
	lc, f, bridge := newCommitLifecycle(time.Hour, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lc.commitTerminal([]byte(commitPayload))
	}()

	select {
	case <-entered:
	case <-time.After(commitWait):
		t.Fatalf("terminal emit never entered the host callback")
	}
	// handleShutdown waits on exactly this drain before unloading the library,
	// so it must keep reporting busy while the owner is inside the host ABI.
	if bridge.WaitForInFlight(150 * time.Millisecond) {
		t.Fatalf("shutdown drain reported drained while the terminal emit was parked")
	}
	releaseEmit()
	select {
	case <-done:
	case <-time.After(commitWait):
		t.Fatalf("commitTerminal did not return after the emit was released")
	}
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("in-flight callbacks did not drain after the emit returned")
	}
	if _, downs, ups := commitCallCounts(f); downs != 1 || ups != 1 {
		t.Fatalf("host down/up calls = %d/%d, want 1/1", downs, ups)
	}
}

// TestCommitTerminalWatchdogRaceHasSingleOwner drives the single-result-owner
// contract with synchronous gates instead of sleeps. The timeout outcome is
// pinned two ways: the real watchdog claims first (its gated downstream close
// proves the claim before the commit was attempted), and a start gate releases
// a finish/commit pair that races on the mutex alone with either winner
// allowed. Every round must still produce exactly one close set and one drain.
func TestCommitTerminalWatchdogRaceHasSingleOwner(t *testing.T) {
	t.Run("watchdog timeout claims before commit", func(t *testing.T) {
		downEntered := make(chan struct{})
		downRelease := make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		releaseDown := func() { releaseOnce.Do(func() { close(downRelease) }) }
		t.Cleanup(releaseDown)

		h := &commitHostScript{onDownClose: func() {
			enteredOnce.Do(func() { close(downEntered) })
			<-downRelease
		}}
		// timeout 0 makes the real watchdog fire immediately; the gated
		// downstream close proves it claimed before the commit attempt.
		lc, f, bridge := newCommitLifecycle(0, h)
		select {
		case <-downEntered:
		case <-time.After(commitWait):
			t.Fatalf("watchdog timeout never reached the downstream close")
		}
		lc.commitTerminal([]byte(commitPayload))

		if !lc.ended() {
			t.Fatalf("ended() = false after the watchdog claimed")
		}
		_, emits, downErrs, _ := h.snapshot()
		if len(emits) != 0 {
			t.Fatalf("commit emitted a terminal after the watchdog already claimed: %q", emits)
		}
		if len(downErrs) != 1 || !strings.Contains(downErrs[0], "request-timeout") {
			t.Fatalf("downstream close errors = %q, want the watchdog timeout", downErrs)
		}
		releaseDown()
		if !bridge.WaitForInFlight(commitWait) {
			t.Fatalf("in-flight callbacks did not drain after the timeout close")
		}
		steps, _, downErrs, upCloses := h.snapshot()
		if got := strings.Join(steps, ","); got != "down,up" {
			t.Fatalf("host callback order = %q, want only the timeout closes", got)
		}
		if len(downErrs) != 1 || upCloses != 1 {
			t.Fatalf("downstream/upstream closes = %d/%d, want 1/1", len(downErrs), upCloses)
		}
		if emits, downs, ups := commitCallCounts(f); emits != 0 || downs != 1 || ups != 1 {
			t.Fatalf("host emit/down/up calls = %d/%d/%d, want 0/1/1", emits, downs, ups)
		}
	})

	t.Run("finish and commit race the start gate", func(t *testing.T) {
		const rounds = 24
		for i := 0; i < rounds; i++ {
			h := &commitHostScript{}
			lc, f, bridge := newCommitLifecycle(time.Hour, h)
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				lc.finish("stream exceeded request-timeout", "timeout")
			}()
			go func() {
				defer wg.Done()
				<-start
				lc.commitTerminal([]byte(commitPayload))
			}()
			close(start)
			wg.Wait()

			if !lc.ended() {
				t.Fatalf("round %d: ended() = false after both paths ran", i)
			}
			if !bridge.WaitForInFlight(commitWait) {
				t.Fatalf("round %d: in-flight callbacks did not drain; double Done or a leaked lifecycle count", i)
			}
			_, _, downErrs, upCloses := h.snapshot()
			emits, downs, ups := commitCallCounts(f)
			if len(downErrs) != 1 || downs != 1 || upCloses != 1 || ups != 1 {
				t.Fatalf("round %d: downstream closes = host %d / recorded %d, upstream closes = host %d / recorded %d; want exactly one of each",
					i, downs, len(downErrs), ups, upCloses)
			}
			switch {
			case emits == 1:
				if downErrs[0] != "" {
					t.Fatalf("round %d: losing finish injected error %q after the commit owner won", i, downErrs[0])
				}
			case emits == 0:
				if strings.TrimSpace(downErrs[0]) == "" {
					t.Fatalf("round %d: no owner delivered a failure close", i)
				}
			default:
				t.Fatalf("round %d: terminal emitted %d times", i, emits)
			}
		}
	})
}

// TestClaimOwnerCountSurvivesFiredWatchdogExit pins the unload-safety window
// deterministically instead of racing for a fired timer: the test publishes a
// timer callback that has already started (so Stop() reports false) but parks
// it on loserRelease before it can claim. The claim therefore wins with
// timerStopped=false, the loser is then allowed to exit, and the drain must
// still observe the owner's own claim count until the owner releases it.
func TestClaimOwnerCountSurvivesFiredWatchdogExit(t *testing.T) {
	f := &fakeCaller{responder: (&commitHostScript{}).responder()}
	bridge := NewHostBridge(f.call)

	lc := &streamLifecycle{bridge: bridge, downID: commitDownID, upstreamID: commitUpID, started: time.Now()}
	fired := make(chan struct{})
	loserRelease := make(chan struct{})
	loserExited := make(chan struct{})
	var releaseOnce sync.Once
	releaseLoser := func() { releaseOnce.Do(func() { close(loserRelease) }) }
	t.Cleanup(releaseLoser)
	bridge.inFlight.Add(1) // the watchdog entry count, exactly like newStreamLifecycle
	lc.mu.Lock()
	lc.timer = time.AfterFunc(0, func() {
		defer bridge.inFlight.Done()
		close(fired)
		<-loserRelease
		lc.finish("stream exceeded request-timeout", "timeout")
		close(loserExited)
	})
	lc.mu.Unlock()

	select {
	case <-fired:
	case <-time.After(commitWait):
		t.Fatalf("test watchdog callback never started")
	}
	owner, timerStopped := lc.claim()
	if !owner {
		t.Fatalf("claim lost to a parked timer callback")
	}
	if timerStopped {
		t.Fatalf("Stop() reported a live timer after the callback had already started")
	}
	releaseLoser()
	select {
	case <-loserExited:
	case <-time.After(commitWait):
		t.Fatalf("losing watchdog callback never exited")
	}
	// The loser's Done has run (or is about to); the winner has not entered any
	// host callback yet. The drain must still observe the owner's claim count.
	if bridge.WaitForInFlight(150 * time.Millisecond) {
		t.Fatalf("drain hit zero between the losing watchdog exit and the owner cleanup")
	}
	bridge.inFlight.Done() // the claim owner releases its own count
	if !bridge.WaitForInFlight(commitWait) {
		t.Fatalf("drain did not finish after the owner count was released")
	}
}
