package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// step scripts one fake worker process.
type step struct {
	lines     []string      // stdout lines, in order
	lineGap   time.Duration // pause before each line
	code      int           // exit code once the lines are out
	hang      bool          // after the lines, block until Kill
	heartbeat time.Duration // while hanging, emit a line this often (0 = silent)
	files     map[string]string
}

func resultLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": text, "num_turns": 1,
	})
	return string(b)
}

var okStep = step{lines: []string{`{"type":"system","subtype":"init"}`, resultLine("done")}}

type launchRec struct {
	task    string
	attempt int
	at      time.Time
	req     core.LaunchRequest
}

// fakeLauncher starts scripted fakeHandles and tracks the live count.
type fakeLauncher struct {
	script   func(task string, attempt int) step
	onLaunch func(req core.LaunchRequest)
	ids      func(task string, attempt int) (pid, pgid int) // optional: what PID/PGID report

	mu       sync.Mutex
	launches []launchRec
	live     int
	peak     int
	killed   map[string]int
}

func (l *fakeLauncher) Launch(ctx context.Context, req core.LaunchRequest) (core.Handle, error) {
	if l.onLaunch != nil {
		l.onLaunch(req)
	}
	s := okStep
	if l.script != nil {
		s = l.script(req.Ref.Task, req.Ref.Attempt)
	}
	f, err := os.Create(req.StreamPath)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.launches = append(l.launches, launchRec{task: req.Ref.Task, attempt: req.Ref.Attempt, at: time.Now(), req: req})
	l.live++
	if l.live > l.peak {
		l.peak = l.live
	}
	l.mu.Unlock()
	h := &fakeHandle{l: l, task: req.Ref.Task, lines: make(chan []byte, 16), kill: make(chan struct{}), done: make(chan struct{})}
	if l.ids != nil {
		h.pid, h.pgid = l.ids(req.Ref.Task, req.Ref.Attempt)
	}
	go h.run(s, f, req.Dir)
	return h, nil
}

func (l *fakeLauncher) snapshot() ([]launchRec, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]launchRec(nil), l.launches...), l.peak
}

func (l *fakeLauncher) killCount(task string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.killed[task]
}

type fakeHandle struct {
	l        *fakeLauncher
	task     string
	lines    chan []byte
	kill     chan struct{}
	killOnce sync.Once
	done     chan struct{}
	exit     core.Exit
	pid      int
	pgid     int
}

func (h *fakeHandle) run(s step, f *os.File, dir string) {
	killed := false
	emit := func(line string) bool {
		fmt.Fprintln(f, line)
		select {
		case h.lines <- []byte(line):
			return true
		case <-h.kill:
			return false
		}
	}
	sleep := func(d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-h.kill:
			return false
		}
	}
	for _, line := range s.lines {
		if (s.lineGap > 0 && !sleep(s.lineGap)) || !emit(line) {
			killed = true
			break
		}
	}
	if !killed && s.hang {
		for !killed {
			if s.heartbeat <= 0 {
				<-h.kill
				killed = true
				break
			}
			if !sleep(s.heartbeat) || !emit(`{"type":"system","subtype":"heartbeat"}`) {
				killed = true
			}
		}
	}
	for name, body := range s.files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	f.Close()
	close(h.lines)
	if killed {
		h.exit = core.Exit{Code: -1, Signal: "killed", Killed: true}
	} else {
		h.exit = core.Exit{Code: s.code}
	}
	h.l.mu.Lock()
	h.l.live--
	h.l.mu.Unlock()
	close(h.done)
}

func (h *fakeHandle) PID() int             { return h.pid }
func (h *fakeHandle) PGID() int            { return h.pgid }
func (h *fakeHandle) Lines() <-chan []byte { return h.lines }
func (h *fakeHandle) Wait() core.Exit      { <-h.done; return h.exit }
func (h *fakeHandle) Kill() error {
	h.killOnce.Do(func() {
		h.l.mu.Lock()
		if h.l.killed == nil {
			h.l.killed = map[string]int{}
		}
		h.l.killed[h.task]++
		h.l.mu.Unlock()
		close(h.kill)
	})
	return nil
}

// fakeVerifier returns scripted outcomes and records every request.
type fakeVerifier struct {
	outcome    func(task string, call int) core.VerifyOutcome
	outcomeReq func(req core.VerifyRequest) core.VerifyOutcome // wins over outcome

	mu    sync.Mutex
	calls []core.VerifyRequest
	times []time.Time
}

var passOutcome = core.VerifyOutcome{Exit: 0, Result: "pass"}

// verdictFor fills VerdictJSON (when the script left it empty) the way
// gosplan verify would: it names the base and head it was asked about.
func verdictFor(o core.VerifyOutcome, req core.VerifyRequest) core.VerifyOutcome {
	if o.VerdictJSON == nil && o.Exit != 2 {
		b, _ := json.Marshal(map[string]any{"result": o.Result, "base_sha": req.Base, "head_sha": req.Head})
		o.VerdictJSON = b
	}
	return o
}

func (v *fakeVerifier) Verify(ctx context.Context, req core.VerifyRequest) (core.VerifyOutcome, error) {
	v.mu.Lock()
	n := 0
	for _, c := range v.calls {
		if c.Task == req.Task {
			n++
		}
	}
	v.calls = append(v.calls, req)
	v.times = append(v.times, time.Now())
	v.mu.Unlock()
	if v.outcomeReq != nil {
		return verdictFor(v.outcomeReq(req), req), nil
	}
	if v.outcome == nil {
		return verdictFor(passOutcome, req), nil
	}
	return verdictFor(v.outcome(req.Task, n+1), req), nil
}

func (v *fakeVerifier) callsFor(task string) []core.VerifyRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []core.VerifyRequest
	for _, c := range v.calls {
		if c.Task == task {
			out = append(out, c)
		}
	}
	return out
}

// fakeWorkspace makes temp dirs and returns a fixed head per task. Bases
// and integration tips are strings naming their inputs, so they are
// deterministic like wt's.
type fakeWorkspace struct {
	prepareErr map[string]error
	heads      map[string]string // branch base name -> head override
	computeErr func(base string, heads []string) error
	conflicts  map[string][]string // head -> conflicting paths at integration

	mu       sync.Mutex
	prepares []core.PrepareRequest
	refs     map[string]string // integration branches
}

func (w *fakeWorkspace) ComputeBase(ctx context.Context, project, base string, heads []string) (string, error) {
	if w.computeErr != nil {
		if err := w.computeErr(base, heads); err != nil {
			return "", err
		}
	}
	if len(heads) == 0 {
		return base, nil
	}
	return "merge(" + base + "+" + strings.Join(heads, "+") + ")", nil
}

func (w *fakeWorkspace) Prepare(ctx context.Context, req core.PrepareRequest) (core.Prepared, error) {
	w.mu.Lock()
	w.prepares = append(w.prepares, req)
	w.mu.Unlock()
	if err := w.prepareErr[req.Task]; err != nil {
		return core.Prepared{}, err
	}
	dir := filepath.Join(req.Root, req.Task)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.Prepared{}, err
	}
	base, _ := w.ComputeBase(ctx, req.Project, req.BaseSHA, req.DepHeads)
	return core.Prepared{Dir: dir, Branch: "gosplan/" + req.Run + "/" + req.Task, BaseSHA: base}, nil
}

func (w *fakeWorkspace) Head(ctx context.Context, project, branch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if tip, ok := w.refs[branch]; ok {
		return tip, nil
	}
	if h, ok := w.heads[path.Base(branch)]; ok {
		return h, nil
	}
	return "head-" + path.Base(branch), nil
}

func (w *fakeWorkspace) Remove(ctx context.Context, project, dir string) error { return nil }

func (w *fakeWorkspace) Integrate(ctx context.Context, project, branch, baseSHA string, heads []string) (string, error) {
	var err error
	for i, h := range heads {
		if paths, ok := w.conflicts[h]; ok {
			heads, err = heads[:i], &fakeConflict{head: h, paths: paths}
			break
		}
	}
	tip, _ := w.ComputeBase(ctx, project, baseSHA, heads)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.refs == nil {
		w.refs = map[string]string{}
	}
	w.refs[branch] = tip
	if err != nil {
		return "", err
	}
	return tip, nil
}

type fakeConflict struct {
	head  string
	paths []string
}

func (c *fakeConflict) Error() string           { return "merge conflict on " + c.head }
func (c *fakeConflict) Unwrap() error           { return core.ErrMergeConflict }
func (c *fakeConflict) ConflictHead() string    { return c.head }
func (c *fakeConflict) ConflictPaths() []string { return c.paths }

func (w *fakeWorkspace) prepareFor(task string) (core.PrepareRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.prepares {
		if p.Task == task {
			return p, true
		}
	}
	return core.PrepareRequest{}, false
}

// harness is one imported run with fakes wired into a Config.
type harness struct {
	t      *testing.T
	dir    string
	dbPath string
	store  *state.Store
	cfg    Config
	l      *fakeLauncher
	v      *fakeVerifier
	w      *fakeWorkspace
}

const testRun = "20261028-000000-test"

func task(id string, tier int, deps ...string) spec.Task {
	return spec.Task{ID: id, Tier: tier, Deps: deps, DepsSource: "tiers"}
}

func newHarness(t *testing.T, tasks ...spec.Task) *harness {
	t.Helper()
	dir := t.TempDir()
	prompts := filepath.Join(dir, "spec", "prompts")
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &spec.Spec{Dir: filepath.Join(dir, "spec"), ContentSHA256: "test"}
	for _, tk := range tasks {
		tk.PromptPath = filepath.Join(prompts, tk.ID+".md")
		if err := os.WriteFile(tk.PromptPath, []byte("Do task "+tk.ID+".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sp.Tasks = append(sp.Tasks, tk)
	}
	dbPath := filepath.Join(dir, "state", "gosplan.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	project := filepath.Join(dir, "project")
	if err := store.ImportRun(context.Background(), testRun, project, "runbase", sp); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, dbPath: dbPath, store: store, l: &fakeLauncher{}, v: &fakeVerifier{}, w: &fakeWorkspace{}}
	h.cfg = Config{
		Run:              testRun,
		Project:          project,
		WorktreeRoot:     filepath.Join(dir, "worktrees"),
		RunDir:           filepath.Join(dir, "runs", testRun),
		EventLog:         filepath.Join(dir, "runs", testRun, "events.jsonl"),
		MaxParallel:      3,
		MaxAttempts:      3,
		StallTimeout:     300 * time.Millisecond,
		LeaseCheck:       50 * time.Millisecond,
		VerifyRetryDelay: 50 * time.Millisecond,
		RetryDelay:       func(n int) time.Duration { return time.Duration(n) * 50 * time.Millisecond },
		Model: func(n int) string {
			if n <= 1 {
				return "sonnet"
			}
			return "opus"
		},
		Spec:      sp,
		Store:     store,
		Launcher:  h.l,
		Verifier:  h.v,
		Workspace: h.w,
		BuildLaunch: func(t state.TaskRow, ref core.AttemptRef, prompt, sessionID, model string, wt core.Prepared) core.LaunchRequest {
			return core.LaunchRequest{Argv: []string{"fakeclaude", "-p", prompt, "--session-id", sessionID, "--model", model}}
		},
	}
	return h
}

// run runs a fresh Reconciler to completion (40s cap) and returns the status.
func (h *harness) run() string {
	h.t.Helper()
	return h.runOutcome().Status
}

// runOutcome is run, returning the whole Outcome.
func (h *harness) runOutcome() Outcome {
	h.t.Helper()
	r, err := New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	status, err := r.Run(ctx)
	if err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		h.t.Fatalf("Run did not finish within 40s (status %s)", status)
	}
	o := r.Outcome()
	if o.Status != status {
		h.t.Fatalf("Run returned %s, Outcome says %s", status, o.Status)
	}
	return o
}

func openSecond(h *harness) (*state.Store, error) { return state.Open(h.dbPath) }

// exec runs SQL on gosplan.db through a second connection, as a worker could.
func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	s, err := state.Open(h.dbPath)
	if err != nil {
		h.t.Error(err)
		return
	}
	defer s.Close()
	if _, err := s.DB().Exec(q, args...); err != nil {
		h.t.Error(err)
	}
}

func (h *harness) task(id string) state.TaskRow {
	h.t.Helper()
	tr, err := h.store.GetTask(context.Background(), testRun, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tr
}

func (h *harness) attempts(id string) []state.AttemptRow {
	h.t.Helper()
	as, err := h.store.ListAttempts(context.Background(), testRun, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return as
}

func outcomes(as []state.AttemptRow) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Outcome)
	}
	return out
}
