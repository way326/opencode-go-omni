package plugin

import (
	"sync"
	"time"

	"github.com/way326/opencode-go-omni/internal/errclass"
)

// streamLifecycle gives normal completion, pump failures, and the watchdog one
// shared result. Notify the downstream before reclaiming the upstream: a host
// HTTP close callback may block, but it must not strand delivery completion.
type streamLifecycle struct {
	mu         sync.Mutex
	finished   bool
	timer      *time.Timer
	bridge     *HostBridge
	downID     string
	upstreamID string
	started    time.Time
}

func newStreamLifecycle(bridge *HostBridge, downID, upstreamID string, timeout time.Duration) *streamLifecycle {
	s := &streamLifecycle{bridge: bridge, downID: downID, upstreamID: upstreamID, started: time.Now()}
	// Hold the lock while publishing the timer so even an immediate deadline
	// cannot race its initialization. Track the watchdog itself, including the
	// gap before it enters a host callback, for safe plugin shutdown.
	s.mu.Lock()
	bridge.inFlight.Add(1)
	s.timer = time.AfterFunc(timeout, func() {
		defer bridge.inFlight.Done()
		defer func() {
			if recover() != nil {
				_ = bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		s.finish("stream exceeded request-timeout", "timeout")
	})
	bridge.registerStreamLifecycle(s)
	s.mu.Unlock()
	return s
}

func (s *streamLifecycle) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// claim publishes this caller as the single result owner: it marks the
// lifecycle finished under the lock, holds its own in-flight count, and stops
// the watchdog. ended() therefore reports true as soon as an owner exists.
// The owner count covers the whole cleanup window even when a watchdog that
// already fired (timerStopped=false) loses and exits first, so the drain can
// never observe zero while the winner is still about to enter host memory.
// timerStopped=true additionally transfers the stopped watchdog's count: that
// timer will never run its callback's deferred Done, so the owner must release
// it after cleanup.
func (s *streamLifecycle) claim() (owner, timerStopped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false, false
	}
	s.finished = true
	// The watchdog holds its count until its callback returns, so the counter
	// can never be zero here: a successful claim always adds to a live count.
	s.bridge.inFlight.Add(1)
	return true, s.timer.Stop()
}

// cleanup ends the downstream and then reclaims the upstream. Only the owner
// returned by claim may call it. The downstream close always comes first so a
// host whose upstream stream_close wedges cannot strand the client. A close
// callback error is a host transport diagnostic, not a delivery
// acknowledgment: it is logged redacted and never reported as success.
func (s *streamLifecycle) cleanup(errMsg string) {
	var downErr error
	if s.downID != "" {
		downErr = s.bridge.StreamCloseDownstream(s.downID, errMsg)
	}
	upErr := s.bridge.StreamClose(s.upstreamID)
	if downErr != nil || upErr != nil {
		debugTrace("executor stream cleanup down_id=%q upstream_id=%q downstream_close_err=%q upstream_close_err=%q", s.downID, s.upstreamID, redactCloseError(downErr), redactCloseError(upErr))
	}
}

// redactCloseError renders a host close-callback failure for the debug trace
// without credentials; nil renders as empty.
func redactCloseError(err error) string {
	if err == nil {
		return ""
	}
	return errclass.Redact(err.Error())
}

func (s *streamLifecycle) finish(errMsg, reason string) {
	owner, timerStopped := s.claim()
	if !owner {
		return
	}
	defer s.bridge.inFlight.Done()
	if timerStopped {
		// A stopped timer will never run the callback's deferred Done. Keep
		// its count through cleanup as well, including bootstrap HTTP errors
		// that do not have a separate pump goroutine holding a count.
		defer s.bridge.inFlight.Done()
	}
	defer s.bridge.unregisterStreamLifecycle(s)
	debugTrace("executor stream finish down_id=%q upstream_id=%q reason=%s failed=%t elapsed_ms=%d", s.downID, s.upstreamID, reason, errMsg != "", time.Since(s.started).Milliseconds())
	s.cleanup(errMsg)
}

// commitTerminal delivers the single deferred success terminal as the last
// downstream chunk and then ends the stream. Ownership is claimed before the
// host emit, so a racing watchdog or a later finish observes an existing owner
// instead of closing an error over a terminal that is already on the wire; the
// emit itself stays bounded by the bridge's emitTimeout. Delivery is never
// assumed from the chosen path: when the emit fails, the downstream close
// carries the redacted real error, and the trace is written only once the emit
// outcome is known so failed= reflects the actual result.
func (s *streamLifecycle) commitTerminal(payload []byte) {
	owner, timerStopped := s.claim()
	if !owner {
		return
	}
	defer s.bridge.inFlight.Done()
	if timerStopped {
		defer s.bridge.inFlight.Done()
	}
	defer s.bridge.unregisterStreamLifecycle(s)
	var emitErr error
	if s.downID != "" {
		emitErr = s.bridge.StreamEmit(s.downID, payload)
	}
	errMsg := ""
	if emitErr != nil {
		errMsg = errclass.Redact("stream terminal emit failed: " + emitErr.Error())
	}
	debugTrace("executor stream finish down_id=%q upstream_id=%q reason=%s failed=%t elapsed_ms=%d", s.downID, s.upstreamID, "terminal", emitErr != nil, time.Since(s.started).Milliseconds())
	s.cleanup(errMsg)
}
