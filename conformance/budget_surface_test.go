// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// budget_surface_test.go: RED BY DESIGN. Schema migration, `gosplan budget`,
// `gosplan budget explain`, `gosplan budget calibrate`, the report block and
// `gosplan replay --check` over budget decisions.
package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// v1DDL is the documented schema v1 (user_version 1).
const v1DDL = `PRAGMA journal_mode = WAL;
PRAGMA user_version = 1;
CREATE TABLE specs (id INTEGER PRIMARY KEY, dir TEXT NOT NULL, content_sha256 TEXT NOT NULL, imported_at TEXT NOT NULL, UNIQUE (dir, content_sha256));
CREATE TABLE runs (id TEXT PRIMARY KEY, spec_id INTEGER NOT NULL REFERENCES specs(id), project TEXT NOT NULL, base_sha TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('running','complete','halted','aborted')), started_at TEXT NOT NULL, finished_at TEXT,
  integration_branch TEXT, integration_sha TEXT);
CREATE TABLE tasks (run_id TEXT NOT NULL REFERENCES runs(id), id TEXT NOT NULL, tier INTEGER NOT NULL, prompt_path TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','running','verifying','passed','failed','blocked','abandoned')),
  deps_source TEXT NOT NULL CHECK (deps_source IN ('tiers','sidecar')), scope_json TEXT NOT NULL DEFAULT 'null',
  allow_new_protected INTEGER NOT NULL DEFAULT 0, not_before TEXT, base_sha TEXT, branch TEXT, worktree TEXT, head_sha TEXT,
  attempts_used INTEGER NOT NULL DEFAULT 0, verify_errors INTEGER NOT NULL DEFAULT 0, last_failure TEXT, blocked_reason TEXT,
  PRIMARY KEY (run_id, id));
CREATE TABLE task_deps (run_id TEXT NOT NULL, task_id TEXT NOT NULL, dep_id TEXT NOT NULL, PRIMARY KEY (run_id, task_id, dep_id),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id), FOREIGN KEY (run_id, dep_id) REFERENCES tasks(run_id, id));
CREATE TABLE attempts (id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT NOT NULL, n INTEGER NOT NULL, session_id TEXT NOT NULL,
  model TEXT NOT NULL, started_at TEXT NOT NULL, ended_at TEXT,
  outcome TEXT CHECK (outcome IN ('exited','stalled','killed','start_failed','lost')), exit_code INTEGER, result_subtype TEXT,
  is_error INTEGER, num_turns INTEGER, duration_ms INTEGER, cost_usd_estimate REAL, usage_json TEXT, stream_path TEXT,
  blocked_request TEXT, head_sha TEXT, verdict_result TEXT CHECK (verdict_result IN ('pass','fail','error')), verdict_json TEXT,
  failed_gates_json TEXT, attestation_path TEXT, mutation_json TEXT, UNIQUE (run_id, task_id, n),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id));
CREATE TABLE leases (run_id TEXT NOT NULL, task_id TEXT NOT NULL, attempt_id INTEGER NOT NULL UNIQUE REFERENCES attempts(id),
  owner TEXT NOT NULL, pid INTEGER, pgid INTEGER, acquired_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL, PRIMARY KEY (run_id, task_id));
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '', type TEXT NOT NULL,
  data_json TEXT NOT NULL, ts TEXT NOT NULL, flushed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX events_unflushed ON events(run_id, flushed, id);
CREATE TABLE window_observations (id INTEGER PRIMARY KEY, window TEXT NOT NULL, utilization REAL, resets_at INTEGER, status TEXT,
  source TEXT NOT NULL, documented INTEGER NOT NULL, observed_at TEXT NOT NULL, attempt_id INTEGER, raw_json TEXT NOT NULL);
CREATE INDEX window_latest ON window_observations(window, observed_at);
INSERT INTO specs VALUES (1, '/old/spec', 'aaaa', '2026-10-30T00:00:00Z');
INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES ('20261030-000000-old1', 1, '/old/project', '0000000000000000000000000000000000000000', 'complete', '2026-10-30T00:00:00Z');
INSERT INTO window_observations (window, utilization, status, source, documented, observed_at, raw_json) VALUES ('seven_day', 0.2, 'allowed', 'rate_limit_event.unifiedWindows', 0, '2026-10-30T00:00:00Z', '{}');
`

// TestBudgetMigratesV1DB: a schema v1 (user_version 1) gosplan.db is migrated in
// place to user_version 2 by adding tables only; old rows survive.
// Clause: budget#BG-SCHEMA
func TestBudgetMigratesV1DB(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	res := run(t, time.Minute, f.state, nil, sqlite3Bin(t), f.db(), v1DDL)
	if res.code != 0 {
		t.Fatalf("infra: build v1 gosplan.db: %s", res)
	}
	mustExit(t, "gosplan run on a v1 gosplan.db", f.runNR(), 0)
	if v := sql(t, f.db(), "PRAGMA user_version;"); v != "2" {
		t.Fatalf("after gosplan run on a v1 gosplan.db: user_version want 2, got %q", v)
	}
	if got := sql(t, f.db(), "SELECT count(*) FROM runs WHERE id = '20261030-000000-old1';"); got != "1" {
		t.Fatalf("the migration lost the old run row")
	}
	if got := sql(t, f.db(), "SELECT count(*) FROM window_observations WHERE observed_at = '2026-10-30T00:00:00Z';"); got != "1" {
		t.Fatalf("the migration lost the old observation")
	}
	for table, cols := range v1Columns {
		if got := sql(t, f.db(), "SELECT group_concat(name, ',') FROM pragma_table_info('"+table+"');"); got != cols {
			t.Fatalf("migration changed v1 table %s: want %s, got %s", table, cols, got)
		}
	}
}

// TestBudgetShowAndReport: `gosplan budget` shows each pool with source,
// documented flag, age and staleness; the run report carries the budget
// block with decisions and the claim-limit notes; `gosplan status` prints a
// budget line.
// Clause: budget#BG-SURFACE
// Clause: budget#BG-REPORT
// Clause: budget#BG-CLAIMS
func TestBudgetShowAndReport(t *testing.T) {
	f, cfg := bgParkFix(t, 20)
	mustExit(t, "gosplan run --budget-config", f.runNR("--budget-config", cfg), 0)
	m := bgJSON(t, f, "gosplan budget --json", "budget", "--state-dir", f.state, "--json")
	if m["schema"] != "gosplan.budget/v1" {
		t.Fatalf("gosplan budget --json schema: want gosplan.budget/v1, got %v", m["schema"])
	}
	if miss := keysMissing(m, "mode", "pools", "reservations", "breakers", "estimates", "fits", "notes"); len(miss) > 0 {
		t.Fatalf("gosplan budget --json lacks %v", miss)
	}
	seen := map[string]map[string]any{}
	for _, p := range bgList(m, "pools") {
		name, _ := p["pool"].(string)
		seen[name] = p
		if miss := keysMissing(p, "level", "observed_at", "age_seconds", "source", "documented", "stale", "resets_at",
			"rate_per_hour", "rate_basis", "projected", "headroom", "open_reserved"); len(miss) > 0 {
			t.Fatalf("pool %s lacks %v", name, miss)
		}
	}
	fh, ok := seen["five_hour"]
	if !ok || seen["seven_day"] == nil {
		t.Fatalf("gosplan budget pools: want five_hour and seven_day, got %v", m["pools"])
	}
	if fh["documented"] != false || fh["stale"] != true {
		t.Fatalf("five_hour after its reset: want documented=false stale=true, got %v", fh)
	}
	txt := gosplan(t, f.project, f.env(), "budget", "--state-dir", f.state)
	if txt.code != 0 || !strings.Contains(txt.stdout, "five_hour") || !strings.Contains(txt.stdout, "(undocumented)") {
		t.Fatalf("gosplan budget text: want five_hour marked (undocumented): %s", txt)
	}

	_, raw := f.status()
	b, _ := raw["budget"].(map[string]any)
	if b == nil || b["mode"] != "enforce" {
		t.Fatalf("report budget.mode: want enforce, got %v", raw["budget"])
	}
	dec, _ := b["decisions"].(map[string]any)
	byRule, _ := dec["by_rule"].(map[string]any)
	if n, _ := bgNum(dec, "refused"); n < 1 || byRule["headroom"] == nil {
		t.Fatalf("report budget.decisions: want >=1 refusal with by_rule.headroom, got %v", dec)
	}
	notes, _ := b["notes"].([]any)
	var all []string
	for _, n := range notes {
		s, _ := n.(string)
		all = append(all, s)
	}
	bgContainsAll(t, "report budget.notes", strings.Join(all, "\n"), "estimate", "undocumented", "does not cap plan usage", "policy")
	st := gosplan(t, f.project, f.env(), "status", "--state-dir", f.state)
	if st.code != 0 || !strings.Contains(st.stdout, "budget:") {
		t.Fatalf("gosplan status text: want a budget: line: %s", st)
	}
}

// TestBudgetExplainParked: `gosplan budget explain` on a parked task says why it
// is not admitted now, when it would be, and the pool numbers.
// Clause: budget#BG-EXPLAIN
func TestBudgetExplainParked(t *testing.T) {
	f, cfg := bgParkFix(t, 300)
	cmd, done := bgStartWatched(f, "--budget-config", cfg)
	defer bgStop(cmd)
	parked := func() bool {
		if _, err := os.Stat(f.db()); err != nil {
			return false
		}
		r := run(t, time.Minute, "", nil, sqlite3Bin(t), "-readonly", f.db(), "SELECT count(*) FROM budget_parks WHERE task_id = '2.a';")
		return r.code == 0 && strings.TrimSpace(r.stdout) == "1"
	}
	if !eventually(60*time.Second, func() bool { return bgExited(done) || parked() }) || !parked() {
		t.Fatalf("gosplan run exited or never parked 2.a")
	}
	m := bgJSON(t, f, "gosplan budget explain 2.a --json", "budget", "explain", "2.a", "--state-dir", f.state, "--json")
	if m["schema"] != "gosplan.budget-explain/v1" || m["admit"] != false || m["rule"] != "headroom" {
		t.Fatalf("explain: want schema gosplan.budget-explain/v1, admit false, rule headroom; got %v", m)
	}
	if m["admit_at"] == nil || m["last_decision"] == nil {
		t.Fatalf("explain must give admit_at and the last recorded decision: %v", m)
	}
	found := false
	for _, p := range bgList(m, "pools") {
		if p["pool"] == "five_hour" {
			h, _ := bgNum(p, "headroom")
			r, _ := bgNum(p, "reserve")
			found = h < r && r > 0
		}
	}
	if !found {
		t.Fatalf("explain pools: want five_hour with headroom < reserve: %v", m["pools"])
	}
}

// TestBudgetCalibrateShape: `gosplan budget calibrate` reports fits with basis,
// estimate quantiles with overrun rates, refusal counts by rule, projection
// error and alias evidence.
// Clause: budget#BG-CALIBRATE
func TestBudgetCalibrateShape(t *testing.T) {
	f, cfg := bgParkFix(t, 20)
	mustExit(t, "gosplan run --budget-config", f.runNR("--budget-config", cfg), 0)
	m := bgJSON(t, f, "gosplan budget calibrate --json", "budget", "calibrate", "--state-dir", f.state, "--json")
	if m["schema"] != "gosplan.budget-calibrate/v1" {
		t.Fatalf("calibrate schema: got %v", m["schema"])
	}
	if miss := keysMissing(m, "fits", "estimates", "refusals", "projection_error", "alias_evidence", "notes"); len(miss) > 0 {
		t.Fatalf("calibrate lacks %v", miss)
	}
	ref, _ := m["refusals"].(map[string]any)
	if n, _ := bgNum(ref, "headroom"); n < 1 {
		t.Fatalf("calibrate refusals: want headroom >= 1, got %v", ref)
	}
	pe, _ := m["projection_error"].(map[string]any)
	if _, ok := pe["n"]; !ok {
		t.Fatalf("calibrate projection_error lacks n: %v", pe)
	}
	ok := false
	for _, e := range bgList(m, "estimates") {
		if e["model"] == "sonnet" {
			n, _ := bgNum(e, "n")
			exp, _ := bgNum(e, "expected_overrun_rate")
			_, hasRate := e["overrun_rate"]
			ok = n >= 1 && hasRate && exp > 0.0999 && exp < 0.1001
		}
	}
	if !ok {
		t.Fatalf("calibrate estimates: want sonnet with n, overrun_rate and expected_overrun_rate 0.1: %v", m["estimates"])
	}
}

// TestBudgetReplayDetectsDrift: `gosplan replay --check` recomputes every budget
// decision from its recorded snapshot; a tampered decision fails it.
// Clause: budget#BG-REPLAY
func TestBudgetReplayDetectsDrift(t *testing.T) {
	f, cfg := bgParkFix(t, 20)
	mustExit(t, "gosplan run --budget-config", f.runNR("--budget-config", cfg), 0)
	rd := filepath.Join(f.state, "runs", f.onlyRun())
	ok := gosplan(t, f.project, nil, "replay", "--log", filepath.Join(rd, "events.jsonl"), "--state", filepath.Join(rd, "state.json"), "--check")
	if ok.code != 0 || !strings.Contains(ok.stdout, "budget decisions recomputed") {
		t.Fatalf("gosplan replay --check on an honest run: want exit 0 and 'budget decisions recomputed', got %s", ok)
	}
	lines := strings.Split(readFile(t, filepath.Join(rd, "budget.jsonl")), "\n")
	admitFalse := regexp.MustCompile(`"admit"\s*:\s*false`)
	tampered := false
	for i, l := range lines {
		if strings.Contains(l, `"budget_decision"`) && admitFalse.MatchString(l) {
			loc := admitFalse.FindStringIndex(l)
			lines[i] = l[:loc[0]] + `"admit":true` + l[loc[1]:]
			tampered = true
			break
		}
	}
	if !tampered {
		t.Fatalf("no refused budget_decision to tamper with")
	}
	bad := filepath.Join(t.TempDir(), "budget.jsonl")
	writeFile(t, filepath.Dir(bad), "budget.jsonl", strings.Join(lines, "\n"))
	res := gosplan(t, f.project, nil, "replay", "--log", filepath.Join(rd, "events.jsonl"), "--state", filepath.Join(rd, "state.json"),
		"--budget-log", bad, "--check")
	if res.code != 1 || !strings.Contains(res.stdout+res.stderr, "budget drift") {
		t.Fatalf("gosplan replay --check on a tampered decision: want exit 1 and 'budget drift', got %s", res)
	}
}
