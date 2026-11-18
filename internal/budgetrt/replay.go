package budgetrt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
)

// Replay recomputes every budget_decision of evs (one run's budget.jsonl, in
// order): it rebuilds the decision's budget.Snapshot from the budget_input
// lines before it (the last line of an id wins, so a re-logged span counts)
// plus the decision's inline fields, runs budget.Decide and compares the
// canonical JSON with the recorded decision. It returns how many decisions
// it recomputed. This proves a decision follows from its recorded inputs,
// not that the inputs were true.
func Replay(evs []budgetlog.Event) (int, error) {
	r := newReplayer()
	n := 0
	for _, ev := range evs {
		switch ev.Type {
		case budgetlog.TypeInput:
			if err := r.input(ev.Data); err != nil {
				return n, fmt.Errorf("budget drift: seq %d (budget_input): %w", ev.Seq, err)
			}
		case budgetlog.TypeDecision:
			var d struct {
				DecisionEvent
				EID int64 `json:"eid"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return n, fmt.Errorf("budget drift: seq %d (budget_decision): %w", ev.Seq, err)
			}
			s, err := r.Snapshot(d.DecisionEvent)
			if err != nil {
				return n, fmt.Errorf("budget drift: eid %d (budget_decision): %w", d.EID, err)
			}
			if field, err := diffJSON(budget.Decide(s), d.Decision); err != nil || field != "" {
				if err != nil {
					return n, err
				}
				return n, fmt.Errorf("budget drift: eid %d (budget_decision): %s", d.EID, field)
			}
			n++
		}
	}
	return n, nil
}

type replayer struct {
	configs map[string]budget.Config
	obs     map[int64]budget.Obs
	spans   map[int64]budget.Span
	samples map[int64]budget.Sample
}

func newReplayer() *replayer {
	return &replayer{configs: map[string]budget.Config{}, obs: map[int64]budget.Obs{},
		spans: map[int64]budget.Span{}, samples: map[int64]budget.Sample{}}
}

func (r *replayer) input(data []byte) error {
	var in InputEvent
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	switch in.Kind {
	case InputConfig:
		var sha string
		if err := json.Unmarshal(in.ID, &sha); err != nil {
			return err
		}
		var c budget.Config
		if err := json.Unmarshal(in.Row, &c); err != nil {
			return err
		}
		r.configs[sha] = c
		return nil
	}
	var id int64
	if err := json.Unmarshal(in.ID, &id); err != nil {
		return err
	}
	switch in.Kind {
	case InputObs:
		var o budget.Obs
		if err := json.Unmarshal(in.Row, &o); err != nil {
			return err
		}
		r.obs[id] = o
	case InputSpan:
		var sp budget.Span
		if err := json.Unmarshal(in.Row, &sp); err != nil {
			return err
		}
		r.spans[id] = sp
	case InputSample:
		var sm budget.Sample
		if err := json.Unmarshal(in.Row, &sm); err != nil {
			return err
		}
		r.samples[id] = sm
	default:
		return fmt.Errorf("unknown input kind %q", in.Kind)
	}
	return nil
}

// Snapshot rebuilds one decision's snapshot; an id with no earlier input
// line is drift.
func (r *replayer) Snapshot(d DecisionEvent) (budget.Snapshot, error) {
	c, ok := r.configs[d.ConfigSHA256]
	if !ok {
		return budget.Snapshot{}, fmt.Errorf("config %s has no earlier budget_input", d.ConfigSHA256)
	}
	s := budget.Snapshot{
		Now: d.Now, Mode: d.Mode, Config: c, ConfigSHA256: d.ConfigSHA256,
		Obs: map[string][]budget.Obs{}, Status: []budget.Obs{}, Spans: []budget.Span{}, Samples: map[string][]budget.Sample{},
		Floors: d.Floors, Open: d.Open, Recent: d.Recent, Breakers: d.Breakers, Candidates: d.Candidates, Runs: d.Runs, For: d.For,
	}
	if s.Floors == nil {
		s.Floors = map[string]budget.Obs{}
	}
	for _, id := range d.ObsIDs {
		o, ok := r.obs[id]
		if !ok {
			return s, fmt.Errorf("obs %d has no earlier budget_input", id)
		}
		s.Obs[o.Pool] = append(s.Obs[o.Pool], o)
	}
	for p := range s.Obs {
		rows := s.Obs[p]
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
				return rows[i].ObservedAt.Before(rows[j].ObservedAt)
			}
			return rows[i].ID < rows[j].ID
		})
	}
	for _, id := range d.SpanIDs {
		sp, ok := r.spans[id]
		if !ok {
			return s, fmt.Errorf("span %d has no earlier budget_input", id)
		}
		s.Spans = append(s.Spans, sp)
	}
	for _, id := range d.SampleIDs {
		sm, ok := r.samples[id]
		if !ok {
			return s, fmt.Errorf("sample %d has no earlier budget_input", id)
		}
		s.Samples[sm.Model] = append(s.Samples[sm.Model], sm)
	}
	return s, nil
}

// diffJSON names the first top-level field (sorted) where a and b differ in
// canonical JSON, or "".
func diffJSON(a, b any) (string, error) {
	ca, err := Canonical(a)
	if err != nil {
		return "", err
	}
	cb, err := Canonical(b)
	if err != nil {
		return "", err
	}
	if bytes.Equal(ca, cb) {
		return "", nil
	}
	var ma, mb map[string]json.RawMessage
	if json.Unmarshal(ca, &ma) != nil || json.Unmarshal(cb, &mb) != nil {
		return "decision", nil
	}
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	ks := make([]string, 0, len(keys))
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		if !bytes.Equal(ma[k], mb[k]) {
			return "decision." + k, nil
		}
	}
	return "decision", nil
}
