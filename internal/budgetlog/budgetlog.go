// Package budgetlog is the budget.jsonl writer and reader.
// Budget events do not go into
// events.jsonl: internal/events rejects unknown types and is locked, and
// events.Validate must keep passing. They go to <state>/runs/<run>/budget.jsonl
// with the same envelope as M1 events (v = 1, seq per file from 1 with no
// gaps, ts, run, task, type, data object) and the same rules: every Append
// holds an exclusive flock and takes its seq from the file, and a torn last
// line is quarantined. This package copies internal/events' approach; it
// does not import it.
package budgetlog

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

// SchemaVersion is the envelope version (the M1 event schema's).
const SchemaVersion = 1

// Budget event types. There is no budget_snapshot: each input row
// is logged once as a budget_input and decisions name rows by id.
const (
	TypeInput    = "budget_input"
	TypeDecision = "budget_decision"
	TypeSettle   = "budget_settle"
	TypeBreaker  = "budget_breaker"
	TypeFit      = "budget_fit"
	TypeProbe    = "budget_probe"
	TypeHalt     = "budget_halt"
)

var knownTypes = map[string]bool{
	TypeInput: true, TypeDecision: true, TypeSettle: true, TypeBreaker: true,
	TypeFit: true, TypeProbe: true, TypeHalt: true,
}

// KnownType reports whether t is one of the documented budget_* types.
func KnownType(t string) bool { return knownTypes[t] }

// Event is one line of budget.jsonl.
type Event struct {
	V    int             `json:"v"`
	Seq  int64           `json:"seq"`
	TS   time.Time       `json:"ts"`
	Run  string          `json:"run"`
	Task string          `json:"task,omitempty"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ErrSeqGap is returned when a file's seq numbers are not 1..n in order.
var ErrSeqGap = errors.New("budgetlog: non-monotonic seq")

// Log appends events to one budget.jsonl. Several Log values (in one process
// or several) may append to the same file: each Append holds an exclusive
// flock and takes its seq from the file's last line. A single Log is not
// safe for concurrent use by several goroutines.
type Log struct {
	f    *os.File
	path string
	run  string
	now  func() time.Time
}

func lock(f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, fmt.Errorf("budgetlog: flock: %w", err)
		}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}

// warnOut receives warnings about repairs to the log (tests swap it).
var warnOut io.Writer = os.Stderr

// Open opens (or creates, mode 0600) the log at path for appending. Under
// the flock it quarantines a torn tail and refuses a file whose envelope is
// already broken (bad seq, version, type or data).
func Open(path, run string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
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
	if err := Validate(evs); err != nil {
		f.Close()
		return nil, err
	}
	return &Log{f: f, path: path, run: run, now: time.Now}, nil
}

// Close closes the underlying file.
func (l *Log) Close() error { return l.f.Close() }

// Append writes one event of type typ (a budget_* type) for task and fsyncs
// it. data must marshal to a JSON object.
func (l *Log) Append(task, typ string, data any) (Event, error) {
	if !knownTypes[typ] {
		return Event{}, fmt.Errorf("budgetlog: unknown type %q", typ)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	if !isObject(raw) {
		return Event{}, fmt.Errorf("budgetlog: %s: data must be a JSON object, got %s", typ, raw)
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
	ev := Event{V: SchemaVersion, Seq: last + 1, TS: l.now().UTC(), Run: l.run, Task: task, Type: typ, Data: raw}
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

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(t, &m) == nil
}

// quarantineTornTail moves an unterminated last line (a write cut short by
// a crash) to <path>.torn-<unix-ts>, truncates the log to its last newline
// and warns on stderr. Complete lines are never touched.
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
		return fmt.Errorf("budgetlog: read torn tail: %w", err)
	}
	qpath, err := writeQuarantine(path, tail)
	if err != nil {
		return fmt.Errorf("budgetlog: quarantine torn tail: %w", err)
	}
	if err := f.Truncate(keep); err != nil {
		return fmt.Errorf("budgetlog: truncate torn tail: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	fmt.Fprintf(warnOut, "budgetlog: warning: %s ended in an unterminated line (%d bytes); moved it to %s and truncated the log to its last newline\n",
		path, len(tail), qpath)
	return nil
}

func lastSeq(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	for end := fi.Size(); end > 0; {
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
			return 0, fmt.Errorf("budgetlog: last line: %w", err)
		}
		return ev.Seq, nil
	}
	return 0, nil
}

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

func writeQuarantine(path string, tail []byte) (string, error) {
	base := fmt.Sprintf("%s.torn-%d", path, time.Now().Unix())
	for n := 0; ; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		q, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("budgetlog: line %d: %w", line, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// Validate checks the envelope: version 1, seq 1..n with no gaps, a known
// budget_* type, and data a JSON object.
func Validate(evs []Event) error {
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			return fmt.Errorf("%w: position %d has seq %d", ErrSeqGap, i+1, ev.Seq)
		}
		if ev.V != SchemaVersion {
			return fmt.Errorf("budgetlog: seq %d: unsupported version %d", ev.Seq, ev.V)
		}
		if !knownTypes[ev.Type] {
			return fmt.Errorf("budgetlog: seq %d: unknown type %q", ev.Seq, ev.Type)
		}
		if !isObject(ev.Data) {
			return fmt.Errorf("budgetlog: seq %d: data is not a JSON object", ev.Seq)
		}
	}
	return nil
}
