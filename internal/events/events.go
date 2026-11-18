// Package events is the append-only audit log for a gosplan run.
//
// The on-disk format is the M1 event schema. Each line is one JSON
// Event. The log is the source of truth; state.json is derived from it.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// SchemaVersion is the only version this package reads or writes.
const SchemaVersion = 1

// Type names an event kind. Payload shapes are in data.go.
type Type string

const (
	RunStarted    Type = "run_started"
	TaskState     Type = "task_state"
	Launch        Type = "launch"
	AttemptResult Type = "attempt_result"
	Gate          Type = "gate"
	Verdict       Type = "verdict"
	Blocker       Type = "blocker"
	RunFinished   Type = "run_finished"
)

var knownTypes = map[Type]bool{
	RunStarted: true, TaskState: true, Launch: true, AttemptResult: true,
	Gate: true, Verdict: true, Blocker: true, RunFinished: true,
}

// Event is one line of events.jsonl.
type Event struct {
	V    int             `json:"v"`
	Seq  int64           `json:"seq"`
	TS   time.Time       `json:"ts"`
	Run  string          `json:"run"`
	Task string          `json:"task,omitempty"`
	Type Type            `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ErrSeqGap is returned when a file's seq numbers are not 1..n in order.
var ErrSeqGap = errors.New("events: non-monotonic seq")

// Log appends events to one file. Several Log values, in one process or
// several (the orchestrator and `gosplan verify --log`), may append to the same
// file: each Append holds an exclusive flock on the file and takes its seq
// from the file's last line, not from memory. A single Log value is not
// safe for concurrent use by several goroutines.
type Log struct {
	f    *os.File
	path string
	run  string
	now  func() time.Time
}

// lock takes an exclusive flock on f and returns the unlock func. flock
// locks belong to the open file description, so two Log values on one
// path exclude each other even inside one process.
func lock(f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, fmt.Errorf("events: flock: %w", err)
		}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}

// warnOut receives warnings about repairs to the log (tests swap it).
var warnOut io.Writer = os.Stderr

// Open opens (or creates) the log at path for appending. It scans the
// existing file to recover the next seq, and refuses a file whose seq
// numbers are already broken (Append takes the next seq from the file). An unterminated last line (a write torn by a
// crash) is moved aside first, see quarantineTornTail. The scan runs under
// the same flock as Append.
func Open(path, run string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	unlock, err := lock(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	defer unlock()
	if err := quarantineTornTail(f, path); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	evs, err := readAll(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := validateSeq(evs); err != nil {
		f.Close()
		return nil, err
	}
	return &Log{f: f, path: path, run: run, now: time.Now}, nil
}

// Close closes the underlying file.
func (l *Log) Close() error { return l.f.Close() }

// Append writes one event and fsyncs it before returning, so a crash
// after Append returns cannot lose the event. Under an exclusive flock it
// repairs a torn tail left by a crashed writer, re-reads the last seq from
// the file, then writes and fsyncs, so writers sharing the file keep seq
// contiguous.
func (l *Log) Append(task string, t Type, data any) (Event, error) {
	if !knownTypes[t] {
		return Event{}, fmt.Errorf("events: unknown type %q", t)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	unlock, err := lock(l.f)
	if err != nil {
		return Event{}, err
	}
	defer unlock()
	if err := quarantineTornTail(l.f, l.path); err != nil {
		return Event{}, err
	}
	last, err := lastSeq(l.f)
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		V: SchemaVersion, Seq: last + 1, TS: l.now().UTC(),
		Run: l.run, Task: task, Type: t, Data: raw,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	if _, err := l.f.Write(line); err != nil {
		return Event{}, err
	}
	if err := l.f.Sync(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// quarantineTornTail repairs a log whose last line has no trailing newline.
// Append writes each event as one line ending in '\n', so such a tail can
// only be a write cut short by a crash; it never holds a whole event. The
// tail bytes are copied to <path>.torn-<unix-ts> (kept for forensics, never
// overwritten), the log is truncated to its last newline, and a warning goes
// to stderr. Lines that end in '\n' are not touched: a complete but invalid
// line still makes Open fail.
func quarantineTornTail(f *os.File, path string) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size == 0 {
		return nil
	}
	keep, err := lastNewlineEnd(f, size)
	if err != nil {
		return err
	}
	if keep == size {
		return nil
	}
	tail := make([]byte, size-keep)
	if _, err := f.ReadAt(tail, keep); err != nil {
		return fmt.Errorf("events: read torn tail: %w", err)
	}
	qpath, err := writeQuarantine(path, tail)
	if err != nil {
		return fmt.Errorf("events: quarantine torn tail: %w", err)
	}
	if err := f.Truncate(keep); err != nil {
		return fmt.Errorf("events: truncate torn tail: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	fmt.Fprintf(warnOut, "events: warning: %s ended in an unterminated line (%d bytes); moved it to %s and truncated the log to its last newline\n",
		path, len(tail), qpath)
	return nil
}

// lastSeq returns the seq of the last non-empty line of f, or 0 for an
// empty log. f must end in '\n' (quarantineTornTail ran first).
func lastSeq(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	for end := fi.Size(); end > 0; {
		// end-1 is the '\n' closing the line; find where the line starts.
		start, err := lastNewlineEnd(f, end-1)
		if err != nil {
			return 0, err
		}
		line := make([]byte, end-1-start)
		if _, err := f.ReadAt(line, start); err != nil {
			return 0, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			end = start
			continue
		}
		var ev struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			return 0, fmt.Errorf("events: last line: %w", err)
		}
		return ev.Seq, nil
	}
	return 0, nil
}

// lastNewlineEnd returns the offset just past the last '\n' in the first
// size bytes of f, or 0 if there is none. It reads backwards in chunks, so
// a healthy log costs one small read.
func lastNewlineEnd(f *os.File, size int64) (int64, error) {
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		b := buf[:end-start]
		if _, err := f.ReadAt(b, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// writeQuarantine writes tail to a new <path>.torn-<unix-ts> file (with a
// -N suffix if that name is taken) and fsyncs it. It returns the file name.
func writeQuarantine(path string, tail []byte) (string, error) {
	base := fmt.Sprintf("%s.torn-%d", path, time.Now().Unix())
	for n := 0; ; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		q, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := q.Write(tail); err != nil {
			q.Close()
			return "", err
		}
		if err := q.Sync(); err != nil {
			q.Close()
			return "", err
		}
		return name, q.Close()
	}
}

// ReadFile returns every event in path, validated.
func ReadFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	evs, err := readAll(f)
	if err != nil {
		return nil, err
	}
	if err := Validate(evs); err != nil {
		return nil, err
	}
	return evs, nil
}

func readAll(r io.Reader) ([]Event, error) {
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("events: line %d: %w", line, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// Validate checks the envelope and the rules in the schema's Validation
// section: known version and types, monotonic seq, that each task's
// task_state chain is continuous, and that a passing verdict lists no
// failed gates.
func Validate(evs []Event) error {
	if err := validateSeq(evs); err != nil {
		return err
	}
	last := map[string]string{}
	for _, ev := range evs {
		if ev.V != SchemaVersion {
			return fmt.Errorf("events: seq %d: unsupported version %d", ev.Seq, ev.V)
		}
		if !knownTypes[ev.Type] {
			return fmt.Errorf("events: seq %d: unknown type %q", ev.Seq, ev.Type)
		}
		if ev.Type == Verdict {
			var d VerdictData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("events: seq %d: verdict data: %w", ev.Seq, err)
			}
			if d.Result == "pass" && len(d.FailedGates) > 0 {
				return fmt.Errorf("events: seq %d: verdict pass with failed gates %v", ev.Seq, d.FailedGates)
			}
			continue
		}
		if ev.Type != TaskState {
			continue
		}
		var d TaskStateData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return fmt.Errorf("events: seq %d: task_state data: %w", ev.Seq, err)
		}
		prev, seen := last[ev.Task]
		if !seen && d.From != "" {
			return fmt.Errorf("events: seq %d: task %s first state must have empty from", ev.Seq, ev.Task)
		}
		if seen && d.From != prev {
			return fmt.Errorf("events: seq %d: task %s from=%q but prior state was %q", ev.Seq, ev.Task, d.From, prev)
		}
		last[ev.Task] = d.To
	}
	return nil
}

func validateSeq(evs []Event) error {
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			return fmt.Errorf("%w: position %d has seq %d", ErrSeqGap, i+1, ev.Seq)
		}
	}
	return nil
}
