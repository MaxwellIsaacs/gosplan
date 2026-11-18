// Package driver starts and supervises one worker process per task attempt.
//
// Each worker runs in its own process group (Setpgid). Kill and the
// post-exit cleanup SIGKILL the whole group, so background children of the
// worker do not linger after the leader exits. Stdout is read line by line
// (lines may be arbitrarily long), appended to the stream file and sent on
// Lines(). A Supervisor refreshes the lease heartbeat on each line and kills
// the group after a period with no output.
//
// Known limit: a child that calls setsid() leaves the process group and
// escapes the kill. If it keeps stdout open, Wait still returns ExitGrace
// after the leader exits (the pipe is closed under it), but the child keeps
// running. Closing that needs D7's cgroup per worker
// (docs/trust-until-d7.md).
package driver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// linesBuffer is the capacity of the Lines() channel.
const linesBuffer = 1024

// Exec is the real core.Launcher.
type Exec struct{}

var _ core.Launcher = Exec{}

// Proc is a running worker; it implements core.Handle.
type Proc struct {
	cmd     *exec.Cmd
	pid     int
	pgid    int
	lines   chan []byte
	dropped atomic.Int64

	mu      sync.Mutex
	killed  bool
	stalled bool
	reaped  bool // the leader was waited for
	done    chan struct{}
	exit    core.Exit
}

var _ core.Handle = (*Proc)(nil)

// Launch starts req.Argv in its own process group with exactly req.Env.
// The Lines channel is buffered (1024); when it is full because nobody is
// reading, lines are still written to the stream file but not sent, and the
// drop is counted (see Proc.Dropped). The returned handle is a *Proc.
func (Exec) Launch(ctx context.Context, req core.LaunchRequest) (core.Handle, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("driver: empty argv")
	}
	stream, err := os.OpenFile(req.StreamPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("driver: open stream: %w", err)
	}
	stderr, err := os.OpenFile(req.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("driver: open stderr: %w", err)
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // an empty environment, never the parent's
	}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// An os.Pipe (not StdoutPipe) so the process can be reaped independently
	// of reading: a background child holding the write end must not keep us
	// from noticing that the leader exited.
	out, pw, err := os.Pipe()
	if err != nil {
		stream.Close()
		stderr.Close()
		return nil, fmt.Errorf("driver: stdout pipe: %w", err)
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		stream.Close()
		stderr.Close()
		out.Close()
		pw.Close()
		return nil, fmt.Errorf("driver: start %s: %w", req.Argv[0], err)
	}
	pw.Close()
	p := &Proc{
		cmd:   cmd,
		pid:   cmd.Process.Pid,
		pgid:  cmd.Process.Pid, // Setpgid with Pgid 0: the leader's pgid is its pid
		lines: make(chan []byte, linesBuffer),
		done:  make(chan struct{}),
	}
	go p.run(out, stream, stderr)
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

// ExitGrace bounds how long stdout is still read after the leader exited
// and its group was killed. A child that left the group (setsid) and kept
// the pipe open would otherwise hold EOF, and so Wait, forever.
var ExitGrace = 2 * time.Second

// run reads stdout in one goroutine while this one waits for the leader.
// When the leader exits, the rest of its group is SIGKILLed, the reader gets
// ExitGrace to reach EOF, then the pipe is closed under it, and the Exit is
// published. Wait therefore depends on the leader's process state, not on
// every holder of the pipe.
func (p *Proc) run(out *os.File, stream, stderr *os.File) {
	defer close(p.done)
	readc := make(chan error, 1)
	go func() { readc <- p.read(out, stream) }()

	waitErr := p.cmd.Wait()
	// The leader is gone: reap anything left in its group.
	_ = KillGroup(p.pgid)
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	var readErr error
	select {
	case readErr = <-readc:
	case <-time.After(ExitGrace):
		// Someone outside the group still holds stdout. Stop reading.
		_ = out.Close()
		if readErr = <-readc; errors.Is(readErr, os.ErrClosed) {
			readErr = nil
		}
	}
	out.Close()
	close(p.lines)
	cerr := errors.Join(stream.Close(), stderr.Close())

	p.mu.Lock()
	defer p.mu.Unlock()
	e := core.Exit{Killed: p.killed && !p.stalled, Stalled: p.stalled}
	var ee *exec.ExitError
	switch {
	case errors.As(waitErr, &ee):
		e.Code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.Code = -1
			e.Signal = ws.Signal().String()
		}
	case waitErr != nil:
		e.Code = -1
		e.Err = waitErr
	}
	e.Err = errors.Join(e.Err, readErr, cerr)
	p.exit = e
}

// read copies stdout lines to the stream file and Lines() until EOF or a
// read error. On a read error it kills the group, so Wait cannot block on a
// leader stuck writing to a pipe nobody reads.
func (p *Proc) read(out *os.File, stream *os.File) error {
	r := bufio.NewReaderSize(out, 64*1024)
	var readErr error
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = append([]byte(nil), line...) // detach from the reader's buffer
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			// Write with the newline; an unterminated final line gets one.
			if _, werr := stream.Write(append(line[:len(line):len(line)], '\n')); werr != nil && readErr == nil {
				readErr = werr
			}
			select {
			case p.lines <- line:
			default:
				p.dropped.Add(1)
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = errors.Join(readErr, err)
				if !errors.Is(err, os.ErrClosed) {
					_ = KillGroup(p.pgid)
				}
			}
			return readErr
		}
	}
}

// PID returns the leader's pid.
func (p *Proc) PID() int { return p.pid }

// PGID returns the process group id (equal to PID).
func (p *Proc) PGID() int { return p.pgid }

// Lines delivers stdout lines without the newline; closed at EOF.
func (p *Proc) Lines() <-chan []byte { return p.lines }

// Dropped is the number of lines not delivered on Lines() because the
// channel was full. They are still in the stream file.
func (p *Proc) Dropped() int { return int(p.dropped.Load()) }

// Wait blocks until the process exited and stdout hit EOF.
func (p *Proc) Wait() core.Exit {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

// Kill SIGKILLs the process group. It is idempotent. Once the leader has
// been reaped (and its group killed by run), it does nothing: the pgid may
// belong to someone else by then.
func (p *Proc) Kill() error {
	p.mu.Lock()
	p.killed = true
	reaped := p.reaped
	p.mu.Unlock()
	if reaped {
		return nil
	}
	return KillGroup(p.pgid)
}

// markStalled records that the supervisor is killing for inactivity.
func (p *Proc) markStalled() {
	p.mu.Lock()
	p.stalled = true
	p.mu.Unlock()
}

// KillGroup SIGKILLs -pgid; ESRCH is not an error.
func KillGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("driver: refusing to kill pgid %d", pgid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// GroupAlive reports whether any process in pgid still exists.
func GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// LeaseOwner returns "<hostname>:<pid>:<nonce>" for this orchestrator process.
func LeaseOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
