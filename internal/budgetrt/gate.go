package budgetrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/window"
)

// TaskMeta is one task's sidecar budget keys as this process read them from
// the spec: never read back from the DB for a
// decision.
type TaskMeta struct {
	Class    string   // "" = normal
	Prefer   []string // nil = none
	MaxTurns int      // 0 = the run's --max-turns
}

// Options configures a Gate.
type Options struct {
	Run, StateDir, RunDir string
	Mode                  string // resolved mode (observe | enforce); off builds no gate
	Config                budget.Config
	ConfigSHA             string // Config.SHA256(); recomputed when empty
	DefaultsUsed          []string
	MaxParallel           int
	Deadline              *time.Time
	CapUSD                *float64
	CapWindow             map[string]float64
	Owner                 string
	Prober                *window.Prober // nil = probe off
	QuietMax              time.Duration  // --stall-quiet-max
	Now                   func() time.Time
	Stderr                io.Writer

	// Tasks is the run's sidecar budget keys from the spec in memory. nil:
	// New reads the run's budget_tasks rows once, at start.
	Tasks map[string]TaskMeta
	// MaxTurns is the run's --max-turns (used when a sidecar names none).
	MaxTurns int
	// Model is the run's first-attempt model (--model): the model Tick
	// assumes for pending tasks when it checks for stale pools.
	Model string
	// BudgetLog is the budget.jsonl path; default <RunDir>/budget.jsonl.
	BudgetLog string
}

// Gate implements reconcile.Budget over internal/state and the
// pure package internal/budget.
type Gate struct {
	store *state.Store
	o     Options
	// Now is the gate's clock (default the store's Now). Hooks that receive
	// a now from the reconciler use that one.
	Now func() time.Time

	cfgJSON string
	logPath string
	prober  *window.Prober

	probeMu sync.Mutex
	probing bool
	probeWG sync.WaitGroup

	// mu guards everything below. Lock order: the DB connection (a tx) is
	// always taken before mu, never the other way round (the store has one
	// connection, and the reconciler calls the tx hooks with it held).
	mu sync.Mutex
	m  memory
	// batches holds the undo log of memory changes made inside a tx whose
	// outcome is not known yet (the reconciler commits it). They are
	// resolved at the next hook that holds the connection: a batch whose
	// witness event is in budget_events committed; otherwise it is undone.
	batches map[*sql.Tx]*batch
}

// memory is what this process wrote or ingested itself.
type memory struct {
	run      state.BudgetRunRow            // expected own budget_runs row (heartbeat excluded)
	tasks    map[string]TaskMeta           // cross-check 1
	res      map[int64]*ownRes             // attempt id -> own reservation (cross-check 3)
	parks    map[string]ownPark            // task -> own park row (cross-check 3)
	breakers map[int64]budget.Breaker      // own RETRY_STORM / SAME_FAILURE rows (cross-check 4)
	facts    map[int64]*budget.AttemptFact // breaker inputs from hook calls
	cost     map[int64]float64             // settled attempts' cost_usd_estimate (parsed by this process)
	floors   map[string]budget.Obs         // pool -> newest own observation
	halt     budget.Halt

	logged  map[string]string // "kind/id" -> row digest: budget_input lines in this run's file
	digests map[int64]string  // decision eid -> digest of its data (SelfCheck)
	last    map[string]*lastDecision
	fits    map[string]string // pool -> canonical JSON of the last budget_fit
	admits  map[string]admitStash
	verrs   int // last consecutive verdict-error count logged

	startEID int64 // budget_events ids below this predate this process
}

type ownRes struct {
	pools    string // canonical pools_json
	enforced bool
	open     bool
	est      *float64
	belt     *float64
	model    string
}

type ownPark struct {
	rule, reason string
	eid          int64
}

type lastDecision struct {
	key        string // rule|reason|inputs|breakers of a logged refusal; "" after an admit
	rule       string
	eid        int64 // the logged decision event
	suppressed int
}

type admitStash struct {
	eid      int64
	decision budget.Decision
	now      time.Time
}

type batch struct {
	witness *witness
	undo    []func()
}

type witness struct {
	eid  int64
	data string
}

// New builds the gate for one run and upserts its budget_runs row. On a
// resume it loads this run's own open reservations, parks, breakers and
// finished attempts (RunAttemptFacts, once) into memory.
func New(store *state.Store, o Options) (*Gate, error) {
	if store == nil {
		return nil, errors.New("budgetrt: store is required")
	}
	if o.Run == "" || o.RunDir == "" {
		return nil, errors.New("budgetrt: Options.Run and RunDir are required")
	}
	switch o.Mode {
	case budget.ModeObserve, budget.ModeEnforce:
	default:
		return nil, fmt.Errorf("budgetrt: mode must be observe or enforce, got %q (off builds no gate)", o.Mode)
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	o.Config.Mode = o.Mode
	if o.ConfigSHA == "" || o.ConfigSHA != o.Config.SHA256() {
		o.ConfigSHA = o.Config.SHA256()
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = 1
	}
	if o.BudgetLog == "" {
		o.BudgetLog = filepath.Join(o.RunDir, "budget.jsonl")
	}
	g := &Gate{store: store, o: o, Now: o.Now, logPath: o.BudgetLog, batches: map[*sql.Tx]*batch{}}
	if g.Now == nil {
		g.Now = store.Now
	}
	cj, err := json.Marshal(o.Config)
	if err != nil {
		return nil, fmt.Errorf("budgetrt: config: %w", err)
	}
	g.cfgJSON = string(cj)
	if o.Prober != nil {
		p := *o.Prober
		if p.MaxAge <= 0 {
			p.MaxAge = o.Config.StaleAfter
		}
		if p.MinInterval <= 0 {
			p.MinInterval = o.Config.ProbeMinInterval
		}
		if p.Now == nil {
			p.Now = g.Now
		}
		g.prober = &p
	}
	caps, err := capsJSON(o.CapUSD, o.CapWindow)
	if err != nil {
		return nil, err
	}
	g.m = memory{
		run: state.BudgetRunRow{Run: o.Run, Owner: o.Owner, Mode: o.Mode, MaxParallel: o.MaxParallel, Deadline: o.Deadline,
			CapsJSON: caps, ConfigSHA256: o.ConfigSHA, ConfigJSON: g.cfgJSON},
		res: map[int64]*ownRes{}, parks: map[string]ownPark{}, breakers: map[int64]budget.Breaker{},
		facts: map[int64]*budget.AttemptFact{}, cost: map[int64]float64{}, floors: map[string]budget.Obs{},
		logged: map[string]string{}, digests: map[int64]string{}, last: map[string]*lastDecision{},
		fits: map[string]string{}, admits: map[string]admitStash{},
	}
	if o.Tasks != nil {
		g.m.tasks = map[string]TaskMeta{}
		for id, t := range o.Tasks {
			g.m.tasks[id] = normMeta(t)
		}
	}
	ctx := context.Background()
	err = store.Immediate(ctx, func(tx *sql.Tx) error {
		row := g.m.run
		row.HeartbeatAt = g.Now()
		if err := state.UpsertBudgetRun(tx, row); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) + 1 FROM budget_events`).Scan(&g.m.startEID); err != nil {
			return err
		}
		if g.m.tasks == nil {
			if g.m.tasks, err = readBudgetTasks(tx, o.Run); err != nil {
				return err
			}
		}
		return g.loadOwn(tx)
	})
	if err != nil {
		return nil, fmt.Errorf("budgetrt: start: %w", err)
	}
	return g, nil
}

// loadOwn reads this run's rows written by an earlier process (resume).
func (g *Gate) loadOwn(tx *sql.Tx) error {
	rows, err := state.RunReservations(tx, g.o.Run)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.State != "open" {
			continue
		}
		pj, err := poolsJSON(r.Pools)
		if err != nil {
			return err
		}
		g.m.res[r.AttemptID] = &ownRes{pools: pj, enforced: r.Enforced, open: true, est: r.EstTokens, belt: r.BeltUSD, model: r.Model}
	}
	parks, err := state.Parks(tx, g.o.Run)
	if err != nil {
		return err
	}
	for _, p := range parks {
		g.m.parks[p.Task] = ownPark{rule: p.Rule, reason: p.Reason, eid: p.DecisionEID}
	}
	brs, err := ownBreakerRows(tx, g.o.Run)
	if err != nil {
		return err
	}
	for _, b := range brs {
		g.m.breakers[b.ID] = b
	}
	facts, err := state.RunAttemptFacts(tx, g.o.Run)
	if err != nil {
		return err
	}
	for i := range facts {
		f := facts[i]
		g.m.facts[f.AttemptID] = &f
	}
	var costRows *sql.Rows
	if costRows, err = tx.Query(`SELECT id, COALESCE(cost_usd_estimate, 0) FROM attempts WHERE run_id = ? AND ended_at IS NOT NULL`, g.o.Run); err != nil {
		return err
	}
	defer costRows.Close()
	for costRows.Next() {
		var id int64
		var c float64
		if err := costRows.Scan(&id, &c); err != nil {
			return err
		}
		g.m.cost[id] = c
	}
	return costRows.Err()
}

func normMeta(t TaskMeta) TaskMeta {
	if t.Class == "" {
		t.Class = budget.ClassNormal
	}
	if len(t.Prefer) == 0 {
		t.Prefer = nil
	}
	if t.MaxTurns < 0 {
		t.MaxTurns = 0
	}
	return t
}

func readBudgetTasks(tx *sql.Tx, run string) (map[string]TaskMeta, error) {
	rows, err := tx.Query(`SELECT task_id, class, prefer_json, max_turns FROM budget_tasks WHERE run_id = ? ORDER BY task_id`, run)
	if err != nil {
		return nil, fmt.Errorf("budgetrt: budget_tasks: %w", err)
	}
	defer rows.Close()
	out := map[string]TaskMeta{}
	for rows.Next() {
		var id, class, prefer string
		var mt sql.NullInt64
		if err := rows.Scan(&id, &class, &prefer, &mt); err != nil {
			return nil, err
		}
		var m TaskMeta
		m.Class, m.MaxTurns = class, int(mt.Int64)
		if prefer != "" && prefer != "null" {
			if err := json.Unmarshal([]byte(prefer), &m.Prefer); err != nil {
				return nil, fmt.Errorf("budgetrt: budget_tasks %s prefer_json: %w", id, err)
			}
		}
		out[id] = normMeta(m)
	}
	return out, rows.Err()
}

func capsJSON(usd *float64, win map[string]float64) (string, error) {
	c := struct {
		USDEstimate *float64           `json:"usd_estimate,omitempty"`
		Window      map[string]float64 `json:"window,omitempty"`
	}{USDEstimate: usd}
	if len(win) > 0 {
		c.Window = win
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("budgetrt: caps: %w", err)
	}
	return string(b), nil
}

func poolsJSON(p []budget.PoolReserve) (string, error) {
	if p == nil {
		p = []budget.PoolReserve{}
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// Observed hands the gate one observation the runner wrote (a worker stream
// line or a probe row). Per pool (aliased; unknown and _probe excluded) the
// newest row with a utilisation is the pool's floor: a row another writer
// put in gosplan.db can raise the pool's level, never lower it below this.
func (g *Gate) Observed(o state.WindowObservation) {
	if o.Window == "unknown" || o.Window == "_probe" || o.Window == "" || o.Utilization == nil {
		return
	}
	pool := o.Window
	if p, ok := g.o.Config.Alias[o.Window]; ok && p != "" {
		pool = p
	}
	at := o.ObservedAt
	if at.IsZero() {
		at = g.Now()
	}
	u := *o.Utilization
	ob := budget.Obs{ID: o.ID, Pool: pool, Utilization: &u, Status: o.Status, Source: o.Source, Documented: o.Documented, ObservedAt: at}
	if o.ResetsAt != nil {
		r := *o.ResetsAt
		ob.ResetsAt = &r
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.m.floors[pool]; ok && cur.ObservedAt.After(at) {
		return
	}
	g.m.floors[pool] = ob
}

// Halt is the budget halt this gate has stored (Kind "" = none).
func (g *Gate) Halt() budget.Halt {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.m.halt
}

// Flush appends the run's unflushed budget events to budget.jsonl. It must
// not be called inside a tx.
func (g *Gate) Flush(ctx context.Context) error {
	_, err := g.store.FlushBudgetEvents(ctx, g.o.Run, g.logPath)
	return err
}

// Wait waits for an in-flight /usage probe.
func (g *Gate) Wait() { g.probeWG.Wait() }

// ---- tx batches ----

// track registers an undo for a memory change made inside tx. mu is held.
func (g *Gate) track(tx *sql.Tx, undo func()) {
	b := g.batches[tx]
	if b == nil {
		b = &batch{}
		g.batches[tx] = b
	}
	b.undo = append(b.undo, undo)
}

// witnessed records the first event written in tx: the batch committed iff
// this row is in budget_events with the same data. mu is held.
func (g *Gate) witnessed(tx *sql.Tx, eid int64, data string) {
	b := g.batches[tx]
	if b == nil {
		b = &batch{}
		g.batches[tx] = b
	}
	if b.witness == nil {
		b.witness = &witness{eid: eid, data: data}
	}
}

// resolve settles every batch of a tx other than cur. Called with the DB
// connection held (inside cur) and mu held, so every other tx has ended.
func (g *Gate) resolve(cur *sql.Tx) error {
	for tx, b := range g.batches {
		if tx == cur {
			continue
		}
		committed := true
		if b.witness != nil {
			var data string
			err := cur.QueryRow(`SELECT data_json FROM budget_events WHERE id = ? AND run_id = ?`, b.witness.eid, g.o.Run).Scan(&data)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				committed = false
			case err != nil:
				return fmt.Errorf("budgetrt: resolve: %w", err)
			default:
				committed = data == b.witness.data
			}
		}
		if !committed {
			g.undoBatch(b)
		}
		delete(g.batches, tx)
	}
	return nil
}

func (g *Gate) undoBatch(b *batch) {
	for i := len(b.undo) - 1; i >= 0; i-- {
		b.undo[i]()
	}
}

// own runs fn inside one Immediate tx with mu held, and commits or undoes
// the memory changes it tracked by the tx outcome.
func (g *Gate) own(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var cur *sql.Tx
	err := g.store.Immediate(ctx, func(tx *sql.Tx) error {
		cur = tx
		g.mu.Lock()
		defer g.mu.Unlock()
		if err := g.resolve(tx); err != nil {
			return err
		}
		return fn(tx)
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.batches[cur]; b != nil {
		if err != nil {
			g.undoBatch(b)
		}
		delete(g.batches, cur)
	}
	return err
}

// ---- events ----

// event appends one budget event in tx and returns its eid and the stored
// data JSON. mu is held.
func (g *Gate) event(tx *sql.Tx, now time.Time, task, typ string, data any) (int64, string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, "", err
	}
	eid, err := state.AppendBudgetEvent(tx, now, g.o.Run, task, typ, json.RawMessage(raw))
	if err != nil {
		return 0, "", err
	}
	g.witnessed(tx, eid, string(raw))
	return eid, string(raw), nil
}

func tampered(format string, a ...any) error {
	return fmt.Errorf("%w: %s", reconcile.ErrBudgetTampered, fmt.Sprintf(format, a...))
}

func (g *Gate) mode() string { return g.o.Mode }

func (g *Gate) enforce() bool { return g.o.Mode == budget.ModeEnforce }

// factsSorted is the run's attempt facts by id. mu is held.
func (g *Gate) factsSorted() []budget.AttemptFact {
	out := make([]budget.AttemptFact, 0, len(g.m.facts))
	for _, f := range g.m.facts {
		x := *f
		x.FailedGates = append([]string(nil), f.FailedGates...)
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out
}

// setFact replaces (or adds) one attempt fact, tracked in tx. mu is held.
func (g *Gate) setFact(tx *sql.Tx, f budget.AttemptFact) {
	prev, had := g.m.facts[f.AttemptID]
	g.m.facts[f.AttemptID] = &f
	g.track(tx, func() {
		if had {
			g.m.facts[f.AttemptID] = prev
		} else {
			delete(g.m.facts, f.AttemptID)
		}
	})
}
