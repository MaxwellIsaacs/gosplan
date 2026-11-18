package budgetlog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
)

// Replay recomputes every budget decision, breaker change and fit in evs
// from the inputs recorded earlier in the same file. It proves
// that each recorded result follows from its recorded inputs. It does not
// prove the inputs were true: the log is worker-writable, so a consistent
// edit of an input line and the decision that used it replays clean
// (docs/trust-until-d7.md).
//
// n is the number of budget_decision events recomputed. drift is "" when
// nothing differs, else `budget drift: eid <n> (<type>): <first differing
// field>`. err is a malformed file (bad envelope, undecodable data), which is
// not drift.
func Replay(evs []Event) (n int, drift string, err error) {
	if err := Validate(evs); err != nil {
		return 0, "", err
	}
	r := &replayer{
		cfgs:    map[string]budget.Config{},
		obs:     map[int64]budget.Obs{},
		spans:   map[int64]budget.Span{},
		samples: map[int64]budget.Sample{},
	}
	for _, ev := range evs {
		var d string
		switch ev.Type {
		case TypeInput:
			err = r.input(ev)
		case TypeDecision:
			d, err = r.decision(ev)
			if err == nil && d == "" {
				n++
			}
		case TypeBreaker:
			d, err = r.breaker(ev)
		case TypeFit:
			d, err = r.fit(ev)
		}
		if err != nil {
			return n, "", fmt.Errorf("budgetlog: replay seq %d (%s): %w", ev.Seq, ev.Type, err)
		}
		if d != "" {
			return n, fmt.Sprintf("budget drift: eid %d (%s): %s", eidOf(ev), ev.Type, d), nil
		}
	}
	return n, "", nil
}

type replayer struct {
	cfgs    map[string]budget.Config // by sha
	lastCfg *budget.Config           // newest config line, for fit checks
	obs     map[int64]budget.Obs
	spans   map[int64]budget.Span
	samples map[int64]budget.Sample
}

func eidOf(ev Event) int64 {
	var d struct {
		EID int64 `json:"eid"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	return d.EID
}

func (r *replayer) input(ev Event) error {
	var d struct {
		Kind string          `json:"kind"`
		ID   json.RawMessage `json:"id"`
		Row  json.RawMessage `json:"row"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return err
	}
	if d.Kind == "config" {
		var sha string
		if err := json.Unmarshal(d.ID, &sha); err != nil {
			return fmt.Errorf("config id: %w", err)
		}
		var c budget.Config
		if err := json.Unmarshal(d.Row, &c); err != nil {
			return fmt.Errorf("config row: %w", err)
		}
		r.cfgs[sha] = c
		r.lastCfg = &c
		return nil
	}
	var id int64
	if err := json.Unmarshal(d.ID, &id); err != nil {
		return fmt.Errorf("%s id: %w", d.Kind, err)
	}
	switch d.Kind {
	case "obs":
		var o budget.Obs
		if err := json.Unmarshal(d.Row, &o); err != nil {
			return err
		}
		r.obs[id] = o
	case "span":
		var s budget.Span
		if err := json.Unmarshal(d.Row, &s); err != nil {
			return err
		}
		r.spans[id] = s
	case "sample":
		var s budget.Sample
		if err := json.Unmarshal(d.Row, &s); err != nil {
			return err
		}
		r.samples[id] = s
	default:
		return fmt.Errorf("unknown input kind %q", d.Kind)
	}
	return nil
}

type decisionData struct {
	Now       time.Time             `json:"now"`
	Mode      string                `json:"mode"`
	ConfigSHA string                `json:"config_sha256"`
	ObsIDs    []int64               `json:"obs_ids"`
	SpanIDs   []int64               `json:"span_ids"`
	SampleIDs []int64               `json:"sample_ids"`
	Floors    map[string]budget.Obs `json:"floors"`
	Open      []budget.Reservation  `json:"open"`
	Recent    []budget.Reservation  `json:"recent"`
	Breakers  []budget.Breaker      `json:"breakers"`
	Cands     []budget.Candidate    `json:"candidates"`
	Runs      []budget.RunState     `json:"runs"`
	For       budget.Key            `json:"for"`
	Decision  json.RawMessage       `json:"decision"`
}

func (r *replayer) decision(ev Event) (string, error) {
	var d decisionData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "", err
	}
	cfg, ok := r.cfgs[d.ConfigSHA]
	if !ok {
		return fmt.Sprintf("config_sha256: no earlier budget_input config %s", d.ConfigSHA), nil
	}
	if got := cfg.SHA256(); got != d.ConfigSHA {
		return fmt.Sprintf("config_sha256: logged config hashes to %s, decision names %s", got, d.ConfigSHA), nil
	}
	s := budget.Snapshot{
		Now: d.Now, Mode: d.Mode, Config: cfg, ConfigSHA256: d.ConfigSHA,
		Obs: map[string][]budget.Obs{}, Status: []budget.Obs{}, Spans: []budget.Span{},
		Samples: map[string][]budget.Sample{}, Floors: d.Floors, Open: d.Open, Recent: d.Recent,
		Breakers: d.Breakers, Candidates: d.Cands, Runs: d.Runs, For: d.For,
	}
	for _, id := range d.ObsIDs {
		o, ok := r.obs[id]
		if !ok {
			return fmt.Sprintf("obs_ids: id %d has no earlier budget_input", id), nil
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
			return fmt.Sprintf("span_ids: id %d has no earlier budget_input", id), nil
		}
		s.Spans = append(s.Spans, sp)
	}
	sort.SliceStable(s.Spans, func(i, j int) bool { return s.Spans[i].AttemptID < s.Spans[j].AttemptID })
	for _, id := range d.SampleIDs {
		sm, ok := r.samples[id]
		if !ok {
			return fmt.Sprintf("sample_ids: id %d has no earlier budget_input", id), nil
		}
		s.Samples[sm.Model] = append(s.Samples[sm.Model], sm)
	}
	for m := range s.Samples {
		ss := s.Samples[m]
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].AttemptID > ss[j].AttemptID })
	}
	got, err := json.Marshal(budget.Decide(s))
	if err != nil {
		return "", err
	}
	return firstDiff("decision", d.Decision, got)
}

func (r *replayer) breaker(ev Event) (string, error) {
	var d struct {
		Action  string              `json:"action"`
		Breaker budget.Breaker      `json:"breaker"`
		Inputs  budget.BreakerInput `json:"inputs"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "", err
	}
	// Logged breaker rows may carry the id the DB gave them after the
	// evaluation; EvalBreakers' open rows have none.
	want := d.Breaker
	want.ID = 0
	wantJSON, _ := json.Marshal(want)
	var sameAction []budget.BreakerChange
	for _, ch := range budget.EvalBreakers(d.Inputs) {
		if ch.Action != d.Action {
			continue
		}
		sameAction = append(sameAction, ch)
		b := ch.Breaker
		b.ID = 0
		gotJSON, _ := json.Marshal(b)
		if diff, err := firstDiff("breaker", wantJSON, gotJSON); err != nil {
			return "", err
		} else if diff == "" {
			return "", nil
		}
	}
	if len(sameAction) == 0 {
		return fmt.Sprintf("action: recomputing the inputs gives no %q change", d.Action), nil
	}
	b := sameAction[0].Breaker
	b.ID = 0
	gotJSON, _ := json.Marshal(b)
	return firstDiff("breaker", wantJSON, gotJSON)
}

func (r *replayer) fit(ev Event) (string, error) {
	var d struct {
		Pool      string            `json:"pool"`
		Intervals []budget.Interval `json:"intervals"`
		Fit       json.RawMessage   `json:"fit"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "", err
	}
	if r.lastCfg == nil {
		return "fit: no earlier budget_input config to fit with", nil
	}
	got, err := json.Marshal(budget.FitRatio(d.Pool, d.Intervals, *r.lastCfg))
	if err != nil {
		return "", err
	}
	return firstDiff("fit", d.Fit, got)
}

// firstDiff compares two JSON documents as decoded values (so float
// formatting and map order do not matter; null, [] and {} count as equal
// empties) and names the first differing path in sorted-key order, or "".
func firstDiff(path string, want, got []byte) (string, error) {
	var a, b any
	if err := json.Unmarshal(want, &a); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(got, &b); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return diffAny(path, a, b), nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func diffAny(path string, a, b any) string {
	if isEmpty(a) && isEmpty(b) {
		return ""
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: recorded %s, recomputed %s", path, show(a), show(b))
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if d := diffAny(path+"."+k, x[k], y[k]); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("%s: recorded %s, recomputed %s", path, show(a), show(b))
		}
		for i := 0; i < len(x) || i < len(y); i++ {
			var xi, yi any
			if i < len(x) {
				xi = x[i]
			}
			if i < len(y) {
				yi = y[i]
			}
			if i >= len(x) || i >= len(y) {
				return fmt.Sprintf("%s[%d]: recorded %s, recomputed %s", path, i, show(xi), show(yi))
			}
			if d := diffAny(fmt.Sprintf("%s[%d]", path, i), xi, yi); d != "" {
				return d
			}
		}
		return ""
	}
	if reflect.DeepEqual(a, b) {
		return ""
	}
	return fmt.Sprintf("%s: recorded %s, recomputed %s", path, show(a), show(b))
}

func show(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 80 {
		return string(b[:77]) + "..."
	}
	return string(b)
}
