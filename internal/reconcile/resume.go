package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

var integrationSHARe = regexp.MustCompile(`integration_sha=(\S+)`)

// logFacts is what the event log says, independent of gosplan.db.
type logFacts struct {
	statuses       map[string]string // replayed task statuses
	integrationSHA string            // last integration_sha in a run_finished reason
	launchBase     map[string]string // task -> base_sha of its last launch event
	started        bool              // a run_started event was seen
	runBase        string            // base_sha of the first run_started
	specSHA        string            // spec_sha256 of the first run_started
}

func readLog(path, run string) (logFacts, error) {
	f := logFacts{launchBase: map[string]string{}}
	evs, err := events.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		f.statuses = map[string]string{}
		return f, nil
	}
	if err != nil {
		return f, err
	}
	var mine []events.Event
	for _, ev := range evs {
		if ev.Run == run {
			mine = append(mine, ev)
		}
	}
	if f.statuses, err = events.TaskStatuses(mine); err != nil {
		return f, err
	}
	for _, ev := range mine {
		switch ev.Type {
		case events.RunStarted:
			var d state.RunStartedRecord
			if !f.started && json.Unmarshal(ev.Data, &d) == nil {
				f.started, f.runBase, f.specSHA = true, d.BaseSHA, d.SpecSHA256
			}
		case events.RunFinished:
			var d events.RunFinishedData
			if json.Unmarshal(ev.Data, &d) == nil {
				if m := integrationSHARe.FindStringSubmatch(d.Reason); m != nil {
					f.integrationSHA = m[1]
				}
			}
		case events.Launch:
			var d events.LaunchData
			if json.Unmarshal(ev.Data, &d) == nil && d.BaseSHA != "" {
				f.launchBase[ev.Task] = d.BaseSHA
			}
		}
	}
	return f, nil
}

// resumeChecks runs before a resumed run trusts anything in gosplan.db: the
// event log must replay to the DB's task statuses, the integration tip must
// agree between the DB, the log and the branch, and each task's recorded
// base must match the base its last launch event names. Returns a halt
// reason, or "".
func (r *Reconciler) resumeChecks(ctx context.Context) (string, error) {
	f, err := readLog(r.cfg.EventLog, r.cfg.Run)
	if err != nil {
		return fmt.Sprintf("%s: read %s: %v", ReasonInconsistent, r.cfg.EventLog, err), nil
	}
	m := r.snapshot()
	if reason, err := r.specChecks(ctx, m, f); reason != "" || err != nil {
		return reason, err
	}
	if d := events.Diff(f.statuses, m.statuses()); d != "" {
		return fmt.Sprintf("%s: events.jsonl does not replay to gosplan.db: %s", ReasonInconsistent, d), nil
	}
	if f.integrationSHA != m.run.IntegrationSHA {
		return fmt.Sprintf("%s: runs row %s: integration_sha db=%q, events.jsonl=%q", ReasonTampered, m.run.ID,
			m.run.IntegrationSHA, f.integrationSHA), nil
	}
	if m.run.IntegrationSHA != "" {
		got, err := r.cfg.Workspace.Head(ctx, r.cfg.Project, m.run.IntegrationBranch)
		if err != nil || got != m.run.IntegrationSHA {
			return fmt.Sprintf("%s: integration branch %s is at %q (%v), the run recorded %s", ReasonTampered,
				m.run.IntegrationBranch, got, err, m.run.IntegrationSHA), nil
		}
	}
	for _, t := range m.rows() {
		if lb := f.launchBase[t.ID]; lb != "" && lb != t.BaseSHA {
			return fmt.Sprintf("%s: tasks row %s: base_sha db=%q, last launch event=%q", ReasonTampered, t.ID, t.BaseSHA, lb), nil
		}
	}
	return "", nil
}

// specChecks compares what gosplan.db holds about the run's definition with the
// spec as loaded now (Config.Spec) and with the event log: the task set and
// every static task column (tier, prompt_path, deps_source, deps, scope,
// allow_new_protected) against the rows a fresh import of the spec would
// write; specs.content_sha256 against the spec's hash; and runs.base_sha
// and the spec hash against the log's run_started. The spec dir and the log
// are as writable by a worker as gosplan.db (same uid): this moves a forgery
// from one file to three that must agree, it does not prevent one.
func (r *Reconciler) specChecks(ctx context.Context, m *memState, f logFacts) (string, error) {
	sp := r.cfg.Spec
	tampered := func(format string, args ...any) (string, error) {
		return ReasonTampered + ": " + fmt.Sprintf(format, args...), nil
	}
	sha, err := r.cfg.Store.SpecSHA(ctx, r.cfg.Run)
	if err != nil {
		return "", err
	}
	if sha != sp.ContentSHA256 {
		return tampered("specs row of run %s: content_sha256 db=%q, spec dir %s hashes to %q", r.cfg.Run, sha, sp.Dir, sp.ContentSHA256)
	}
	if !f.started {
		return tampered("events.jsonl has no run_started event for run %s", r.cfg.Run)
	}
	if f.specSHA != sp.ContentSHA256 {
		return tampered("spec %s hashes to %q, events.jsonl run_started recorded spec_sha256=%q", sp.Dir, sp.ContentSHA256, f.specSHA)
	}
	if f.runBase != m.run.BaseSHA {
		return tampered("runs row %s: base_sha db=%q, events.jsonl run_started=%q", m.run.ID, m.run.BaseSHA, f.runBase)
	}
	want := map[string]state.TaskRow{}
	for _, t := range state.SpecTasks(r.cfg.Run, sp) {
		want[t.ID] = t
	}
	for _, id := range m.order {
		if _, ok := want[id]; !ok {
			return tampered("tasks row %s: not in the spec", id)
		}
	}
	for _, w := range state.SpecTasks(r.cfg.Run, sp) {
		t := m.tasks[w.ID]
		if t == nil {
			return tampered("tasks row %s: missing (the spec has it)", w.ID)
		}
		var fs []string
		field := func(name string, db, spec any) {
			if !reflect.DeepEqual(db, spec) {
				fs = append(fs, fmt.Sprintf("%s db=%s spec=%s", name, show(db), show(spec)))
			}
		}
		field("tier", t.Tier, w.Tier)
		field("prompt_path", t.PromptPath, w.PromptPath)
		field("deps_source", t.DepsSource, w.DepsSource)
		field("deps", depSet(t.Deps), depSet(w.Deps))
		field("scope_json", nilIfEmpty(t.Scope), nilIfEmpty(w.Scope))
		field("allow_new_protected", t.AllowNewProtected, w.AllowNewProtected)
		if len(fs) > 0 {
			return tampered("tasks row %s: %s", w.ID, strings.Join(fs, ", "))
		}
	}
	return "", nil
}

// depSet is deps sorted and deduplicated, nil for none.
func depSet(deps []string) []string {
	if len(deps) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(deps)))
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// loadResume fills memory's attempt refs from the DB and verifies every
// passed task again, by SHA, before the run continues: the recorded pass is
// not trusted. The base is recomputed from the run base and the (re-verified)
// dep heads and must equal the recorded one. `gosplan verify` is run without
// --cache, so no cached verdict is consulted. Returns a halt reason, or "".
func (r *Reconciler) loadResume(ctx context.Context) (string, error) {
	m := r.snapshot()
	for _, t := range m.rows() {
		if t.Status != core.StatusRunning && t.Status != core.StatusVerifying && t.Status != core.StatusPassed {
			continue
		}
		att, ok, err := r.cfg.Store.LatestAttempt(ctx, r.cfg.Run, t.ID)
		if err != nil {
			return "", err
		}
		// A running task's attempt is recovered only while it is open; an
		// ended one (finished as killed by an abort) just takes the retry path.
		if ok && (t.Status != core.StatusRunning || att.EndedAt.IsZero()) {
			m.attempt[t.ID] = core.AttemptRef{Run: r.cfg.Run, Task: t.ID, Attempt: att.N, AttemptID: att.ID}
		}
	}
	confirmed := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for _, t := range m.rows() {
			if t.Status != core.StatusPassed || confirmed[t.ID] {
				continue
			}
			ready := true
			for _, d := range t.Deps {
				ready = ready && confirmed[d]
			}
			if !ready {
				continue
			}
			reason, err := r.reverify(ctx, m, t)
			if err != nil || reason != "" {
				return reason, err
			}
			confirmed[t.ID], progress = true, true
		}
	}
	for _, t := range m.rows() {
		if t.Status == core.StatusPassed && !confirmed[t.ID] {
			return fmt.Sprintf("%s: task %s is passed but a dependency is not", ReasonTampered, t.ID), nil
		}
	}
	r.stateMu.Lock()
	r.mem.attempt, r.mem.verdicts = m.attempt, m.verdicts
	r.stateMu.Unlock()
	return "", nil
}

func (r *Reconciler) reverify(ctx context.Context, m *memState, t state.TaskRow) (string, error) {
	base, err := r.computeBase(ctx, m, t)
	if err != nil {
		return fmt.Sprintf("%s: task %s: recompute base: %v", ReasonInconsistent, t.ID, err), nil
	}
	if base != t.BaseSHA {
		return fmt.Sprintf("%s: tasks row %s: base_sha db=%q, recomputed %q", ReasonTampered, t.ID, t.BaseSHA, base), nil
	}
	ref, ok := m.attempt[t.ID]
	att, aok, err := r.cfg.Store.LatestAttempt(ctx, r.cfg.Run, t.ID)
	if err != nil {
		return "", err
	}
	if !ok || !aok || att.VerdictResult != "pass" || att.HeadSHA != t.HeadSHA {
		return fmt.Sprintf("%s: task %s is passed but its latest attempt has no pass verdict for head %q", ReasonTampered, t.ID, t.HeadSHA), nil
	}
	req := core.VerifyRequest{
		Project: r.cfg.Project, Run: r.cfg.Run, Task: t.ID, Attempt: ref.Attempt,
		Base: base, Head: t.HeadSHA, Scope: t.Scope, AllowNewProtected: t.AllowNewProtected,
		EventLog: r.cfg.EventLog,
		LogsDir:  filepath.Join(r.cfg.RunDir, "verify-logs", t.ID, fmt.Sprintf("%d-resume", ref.Attempt)),
	}
	out, verr := r.cfg.Verifier.Verify(ctx, req)
	kind := classify(out, verr)
	if kind == verdictPass {
		if err := verdictNames(out.VerdictJSON, base, t.HeadSHA); err != nil {
			kind, out.Detail = verdictError, err.Error()
		}
	}
	switch kind {
	case verdictPass:
		m.verdicts[t.ID] = verdictMem{attempt: ref.Attempt, result: "pass", base: base, head: t.HeadSHA, tree: out.TreeSHA}
		return "", nil
	case verdictFail:
		return fmt.Sprintf("%s: task %s is recorded passed, but re-verifying %s..%s fails: %v", ReasonTampered, t.ID,
			short(base), short(t.HeadSHA), out.FailedGates), nil
	}
	detail := out.Detail
	if verr != nil {
		detail = verr.Error()
	}
	return fmt.Sprintf("%s: task %s is recorded passed, but %s..%s could not be re-verified: %s", ReasonInconsistent, t.ID,
		short(base), short(t.HeadSHA), firstLine(detail)), nil
}

// selfCheck is `gosplan replay --check` run in-process at the end of the run,
// plus the integration tip: the DB must still equal memory, the event log
// must replay to memory's statuses, and the branch and the log must name
// the tip this process produced. Returns a halt reason, or "".
func (r *Reconciler) selfCheck(ctx context.Context) string {
	if err := r.flush(ctx); err != nil {
		return fmt.Sprintf("%s: flush events: %v", ReasonInconsistent, err)
	}
	m := r.snapshot()
	run, err := r.cfg.Store.GetRun(ctx, r.cfg.Run)
	if err != nil {
		return fmt.Sprintf("%s: read run: %v", ReasonInconsistent, err)
	}
	tasks, err := r.cfg.Store.ListTasks(ctx, r.cfg.Run)
	if err != nil {
		return fmt.Sprintf("%s: read tasks: %v", ReasonInconsistent, err)
	}
	if d := m.diff(run, tasks); d != "" {
		return ReasonTampered + ": " + d
	}
	f, err := readLog(r.cfg.EventLog, r.cfg.Run)
	if err != nil {
		return fmt.Sprintf("%s: read %s: %v", ReasonInconsistent, r.cfg.EventLog, err)
	}
	if d := events.Diff(f.statuses, m.statuses()); d != "" {
		return fmt.Sprintf("%s: events.jsonl replay: %s", ReasonInconsistent, d)
	}
	if tip := m.run.IntegrationSHA; tip != "" {
		if f.integrationSHA != tip {
			return fmt.Sprintf("%s: events.jsonl names integration tip %q, the run produced %s", ReasonInconsistent, f.integrationSHA, tip)
		}
		got, err := r.cfg.Workspace.Head(ctx, r.cfg.Project, m.run.IntegrationBranch)
		if err != nil || got != tip {
			return fmt.Sprintf("%s: integration branch %s is at %q (%v), the run produced %s", ReasonInconsistent,
				m.run.IntegrationBranch, got, err, tip)
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
