package driver

import (
	"context"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/streamjson"
)

// heartbeatInterval is the minimum spacing of heartbeat writes per lease.
const heartbeatInterval = time.Second

// DefaultStallQuietMax caps the api_retry quiet allowance when
// Supervisor.QuietMax is 0. Policy, not a
// measurement.
const DefaultStallQuietMax = 15 * time.Minute

// APIRetryQuiet is the stall allowance for one stdout line: retry_delay_ms
// of a system/api_retry line, capped at max; 0 for any other line (or a
// negative delay). retry_delay_ms comes from the worker's own stdout, so the
// cap is what stops one line from buying an endless wait.
func APIRetryQuiet(line []byte, max time.Duration) time.Duration {
	l, err := streamjson.Decode(line)
	if err != nil {
		return 0
	}
	r, ok, err := streamjson.ParseAPIRetry(l)
	if err != nil || !ok || r.RetryDelayMS <= 0 {
		return 0
	}
	d := time.Duration(r.RetryDelayMS) * time.Millisecond
	if d/time.Millisecond != time.Duration(r.RetryDelayMS) || d > max {
		d = max // overflow or over the cap
	}
	return max0(d)
}

func max0(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// Supervisor watches one handle: heartbeat on each line, stall kill.
type Supervisor struct {
	Store        *state.Store
	StallTimeout time.Duration     // 0 = core.StallTimeout
	OnLine       func(line []byte) // optional, called for every line, in order
	Now          func() time.Time  // optional
	// Quiet is the extra silence allowed after one line. After each line the stall timer is
	// StallTimeout + min(Quiet(line), QuietMax). nil = APIRetryQuiet.
	Quiet func(line []byte) time.Duration
	// QuietMax caps Quiet (policy); 0 = DefaultStallQuietMax.
	QuietMax time.Duration

	// heartbeat overrides Store.Heartbeat (tests count calls).
	heartbeat func(ctx context.Context, ref core.AttemptRef, at time.Time) error
}

// Run consumes h.Lines() until it closes, writes heartbeats for ref's lease
// (at most one per second), kills the group after StallTimeout with no line
// (plus the line's capped Quiet allowance: an api_retry wait is not a stall),
// and returns h.Wait() with Stalled set when it killed for inactivity.
//
// Heartbeats are written by their own goroutine: a write waiting on a
// locked gosplan.db (busy_timeout) never stalls reading the worker's stdout. A
// beat due while the previous one is still being written is dropped.
func (s *Supervisor) Run(ctx context.Context, ref core.AttemptRef, h core.Handle) core.Exit {
	timeout := s.StallTimeout
	if timeout <= 0 {
		timeout = core.StallTimeout
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	quietMax := s.QuietMax
	if quietMax <= 0 {
		quietMax = DefaultStallQuietMax
	}
	quiet := s.Quiet
	if quiet == nil {
		quiet = func(line []byte) time.Duration { return APIRetryQuiet(line, quietMax) }
	}
	beat := s.heartbeat
	if beat == nil && s.Store != nil {
		beat = s.Store.Heartbeat
	}

	beats := make(chan time.Time, 1)
	defer close(beats)
	if beat != nil {
		go func() {
			for t := range beats {
				_ = beat(ctx, ref, t)
			}
		}()
	}

	stalled := false
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var lastBeat time.Time
	lines := h.Lines()
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			if s.OnLine != nil {
				s.OnLine(line)
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout + min(max0(quiet(line)), quietMax))
			if t := now(); beat != nil && (lastBeat.IsZero() || t.Sub(lastBeat) >= heartbeatInterval) {
				lastBeat = t
				select {
				case beats <- t:
				default:
				}
			}
		case <-timer.C:
			stalled = true
			if m, ok := h.(interface{ markStalled() }); ok {
				m.markStalled()
			}
			_ = h.Kill()
			break loop
		}
	}
	e := h.Wait()
	if stalled {
		e.Stalled = true
		e.Killed = false
	}
	return e
}
