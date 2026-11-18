package events

import (
	"encoding/json"
	"fmt"
	"sort"
)

// TaskStatuses folds task_state events in seq order and returns each
// task's final status. A task that never had a task_state event is absent.
func TaskStatuses(evs []Event) (map[string]string, error) {
	out := map[string]string{}
	for _, ev := range evs {
		if ev.Type != TaskState {
			continue
		}
		var d TaskStateData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return nil, fmt.Errorf("events: seq %d: %w", ev.Seq, err)
		}
		out[ev.Task] = d.To
	}
	return out, nil
}

// Diff compares replayed statuses against a recorded state map (the
// contents of state.json). It returns the first mismatch in sorted task-id
// order, or "" if they agree exactly.
func Diff(replayed, recorded map[string]string) string {
	for _, task := range sortedKeys(replayed) {
		want := replayed[task]
		if got, ok := recorded[task]; !ok || got != want {
			return fmt.Sprintf("task %s: log says %q, state says %q", task, want, got)
		}
	}
	for _, task := range sortedKeys(recorded) {
		if _, ok := replayed[task]; !ok {
			return fmt.Sprintf("task %s: state says %q, log has no task_state", task, recorded[task])
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
