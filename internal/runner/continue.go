package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/wt"
)

// subsetFile records, in a run's directory, that the run is some of a
// spec's tasks on top of another run's passed work (--only / --continue).
// A resume of that run reads it and filters the spec the same way, so the
// task set and spec hash still match what was imported.
const subsetFile = "subset.json"

type subset struct {
	OnRun string   `json:"on_run"`
	Only  []string `json:"only"`
}

// onRunState is what a new run builds on: the ended run's passed work.
type onRunState struct {
	base      string          // integration tip, or the run base with the passed heads merged in
	passed    map[string]bool // task -> passed in that run
	notPassed []string        // every other task, in (tier, id) order
}

// loadOnRun reads an ended run of the same project and computes the base a
// continuation starts from. Its passed heads are taken as recorded: they
// were verified by that run, and the new run verifies only its own tasks.
func loadOnRun(ctx context.Context, store *state.Store, project, id string) (onRunState, error) {
	rr, err := store.GetRun(ctx, id)
	if err != nil {
		return onRunState{}, fmt.Errorf("--on-run %s: %w", id, err)
	}
	if rr.Project != project {
		return onRunState{}, fmt.Errorf("--on-run %s belongs to project %s, not %s", id, rr.Project, project)
	}
	if !reconcile.Ended(rr.Status) && rr.Status != core.RunAborted {
		return onRunState{}, fmt.Errorf("--on-run %s is still %s; resume it with --run %s, or wait for it to end", id, rr.Status, id)
	}
	tasks, err := store.ListTasks(ctx, id)
	if err != nil {
		return onRunState{}, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Tier != tasks[j].Tier {
			return tasks[i].Tier < tasks[j].Tier
		}
		return tasks[i].ID < tasks[j].ID
	})
	st := onRunState{passed: map[string]bool{}}
	var heads []string
	for _, t := range tasks {
		if t.Status == core.StatusPassed && t.HeadSHA != "" {
			st.passed[t.ID] = true
			heads = append(heads, t.HeadSHA)
		} else {
			st.notPassed = append(st.notPassed, t.ID)
		}
	}
	if rr.IntegrationSHA != "" {
		st.base = rr.IntegrationSHA
		return st, nil
	}
	// Halted before integration (state_write_failed, environment, ...):
	// merge the passed heads onto the run base, the same way a task base is.
	if st.base, err = (wt.Git{}).ComputeBase(ctx, project, rr.BaseSHA, heads); err != nil {
		return onRunState{}, fmt.Errorf("--on-run %s: merge its passed heads: %w", id, err)
	}
	return st, nil
}

// filterSpec keeps only the tasks in only, in spec order. A dependency
// outside the subset must have passed in the run the subset builds on (its
// work is in the base); it is dropped from the task's deps. Any other
// dependency is an error: its work is missing.
func filterSpec(sp *spec.Spec, only []string, onRun string, passed map[string]bool) (*spec.Spec, error) {
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	out := *sp
	out.Tasks = nil
	for _, t := range sp.Tasks {
		if !want[t.ID] {
			continue
		}
		delete(want, t.ID)
		var deps []string
		for _, d := range t.Deps {
			switch {
			case containsStr(only, d):
				deps = append(deps, d)
			case passed[d]:
			default:
				return nil, fmt.Errorf("task %s depends on %s, which did not pass in run %s: add %s to --only", t.ID, d, onRun, d)
			}
		}
		t.Deps = deps
		out.Tasks = append(out.Tasks, t)
	}
	if len(want) > 0 {
		var missing []string
		for id := range want {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("--only: no such task in %s: %s", sp.Dir, strings.Join(missing, ", "))
	}
	if len(out.Tasks) == 0 {
		return nil, errors.New("--only: no tasks")
	}
	ids := append([]string(nil), only...)
	sort.Strings(ids)
	h := sha256.Sum256([]byte(sp.ContentSHA256 + "\x00on:" + onRun + "\x00only:" + strings.Join(ids, ",")))
	out.ContentSHA256 = hex.EncodeToString(h[:])
	return &out, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func writeSubset(runDir string, s subset) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, subsetFile), append(b, '\n'), 0o600)
}

// readSubset returns the run's subset, or ok=false for an ordinary run.
func readSubset(runDir string) (s subset, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(runDir, subsetFile))
	if errors.Is(err, os.ErrNotExist) {
		return subset{}, false, nil
	}
	if err != nil {
		return subset{}, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return subset{}, false, fmt.Errorf("%s: %w", subsetFile, err)
	}
	return s, true, nil
}
