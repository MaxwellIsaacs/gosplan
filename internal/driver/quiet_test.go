package driver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// quietHandle is an in-memory core.Handle: it prints its lines, stays
// silent for `silence`, then exits 0 (unless killed first).
type quietHandle struct {
	lines    chan []byte
	kill     chan struct{}
	killOnce sync.Once
	done     chan struct{}
	exit     core.Exit
}

func newQuietHandle(silence time.Duration, lines ...string) *quietHandle {
	h := &quietHandle{lines: make(chan []byte, len(lines)+1), kill: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		for _, l := range lines {
			h.lines <- []byte(l)
		}
		select {
		case <-time.After(silence):
			h.exit = core.Exit{Code: 0}
		case <-h.kill:
			h.exit = core.Exit{Code: -1, Killed: true}
		}
		close(h.lines)
	}()
	return h
}

func (h *quietHandle) PID() int             { return 0 }
func (h *quietHandle) PGID() int            { return 0 }
func (h *quietHandle) Lines() <-chan []byte { return h.lines }
func (h *quietHandle) Wait() core.Exit      { <-h.done; return h.exit }
func (h *quietHandle) Kill() error {
	h.killOnce.Do(func() { close(h.kill) })
	return nil
}

const apiRetry400 = `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":400,"error_status":529,"error":"overloaded"}`

func runQuiet(t *testing.T, sup Supervisor, h *quietHandle) core.Exit {
	t.Helper()
	done := make(chan core.Exit, 1)
	go func() { done <- sup.Run(context.Background(), core.AttemptRef{}, h) }()
	select {
	case e := <-done:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("Supervisor.Run did not return")
		return core.Exit{}
	}
}

// An api_retry line with retry_delay_ms 400 buys 400 ms on top of the
// 300 ms stall timeout: 600 ms of silence is not a stall.
func TestQuietAPIRetryExtendsStallTimer(t *testing.T) {
	h := newQuietHandle(600*time.Millisecond, `{"type":"system","subtype":"init"}`, apiRetry400)
	e := runQuiet(t, Supervisor{StallTimeout: 300 * time.Millisecond}, h)
	if e.Stalled || e.Killed || e.Code != 0 {
		t.Fatalf("api_retry wait killed as a stall: exit %+v", e)
	}
}

// The same silence without the api_retry line is a stall.
func TestQuietSilenceWithoutAPIRetryStalls(t *testing.T) {
	h := newQuietHandle(600*time.Millisecond, `{"type":"system","subtype":"init"}`)
	e := runQuiet(t, Supervisor{StallTimeout: 300 * time.Millisecond}, h)
	if !e.Stalled {
		t.Fatalf("600 ms silence with a 300 ms stall timeout was not a stall: exit %+v", e)
	}
}

// retry_delay_ms 10 000 is capped at QuietMax 200 ms: 300+200 < 600, stall.
func TestQuietAllowanceIsCapped(t *testing.T) {
	line := `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":10000}`
	h := newQuietHandle(600*time.Millisecond, line)
	e := runQuiet(t, Supervisor{StallTimeout: 300 * time.Millisecond, QuietMax: 200 * time.Millisecond}, h)
	if !e.Stalled {
		t.Fatalf("retry_delay_ms 10000 was not capped at QuietMax 200ms: exit %+v", e)
	}
}

// A custom Quiet is used instead of the api_retry parser, and still capped.
func TestQuietCustomFuncCapped(t *testing.T) {
	h := newQuietHandle(600*time.Millisecond, `{"type":"assistant"}`)
	sup := Supervisor{StallTimeout: 300 * time.Millisecond, QuietMax: time.Second,
		Quiet: func([]byte) time.Duration { return 500 * time.Millisecond }}
	if e := runQuiet(t, sup, h); e.Stalled {
		t.Fatalf("custom Quiet 500ms ignored: exit %+v", e)
	}
	h = newQuietHandle(600*time.Millisecond, `{"type":"assistant"}`)
	sup.QuietMax = 100 * time.Millisecond
	if e := runQuiet(t, sup, h); !e.Stalled {
		t.Fatalf("custom Quiet not capped at QuietMax: exit %+v", e)
	}
}

func TestQuietAPIRetryQuietParse(t *testing.T) {
	for _, c := range []struct {
		line string
		max  time.Duration
		want time.Duration
	}{
		{apiRetry400, time.Minute, 400 * time.Millisecond},
		{apiRetry400, 100 * time.Millisecond, 100 * time.Millisecond},
		{`{"type":"system","subtype":"init","retry_delay_ms":400}`, time.Minute, 0},
		{`{"type":"system","subtype":"api_retry","retry_delay_ms":-5}`, time.Minute, 0},
		{`{"type":"system","subtype":"api_retry","retry_delay_ms":9223372036854775807}`, time.Minute, time.Minute},
		{`not json`, time.Minute, 0},
	} {
		if got := APIRetryQuiet([]byte(c.line), c.max); got != c.want {
			t.Errorf("APIRetryQuiet(%s, %s) = %s, want %s", c.line, c.max, got, c.want)
		}
	}
}
