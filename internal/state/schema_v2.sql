-- Contract file: changes only by human-reviewed commits.
PRAGMA user_version = 2;

-- Per-task budget metadata from the sidecar (class, prefer, max_turns).
CREATE TABLE budget_tasks (
  run_id      TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  class       TEXT NOT NULL DEFAULT 'normal',
  prefer_json TEXT NOT NULL DEFAULT 'null',   -- JSON array of models, or null
  max_turns   INTEGER,                        -- NULL = the run's --max-turns
  PRIMARY KEY (run_id, task_id),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id)
);

-- One row per gosplan run process that has touched the run (liveness for shared accounting).
CREATE TABLE budget_runs (
  run_id        TEXT PRIMARY KEY REFERENCES runs(id),
  owner         TEXT NOT NULL,                -- lease owner string of the orchestrator
  mode          TEXT NOT NULL CHECK (mode IN ('off','observe','enforce')),
  max_parallel  INTEGER NOT NULL,
  deadline      TEXT,                         -- RFC 3339 UTC or NULL
  caps_json     TEXT NOT NULL DEFAULT '{}',   -- {"usd_estimate":x,"window":{"five_hour":0.3}}
  config_sha256 TEXT NOT NULL,
  config_json   TEXT NOT NULL,                -- effective budget config
  heartbeat_at  TEXT NOT NULL,
  halt          TEXT CHECK (halt IN ('drain','stop')),
  halt_reason   TEXT
);

CREATE TABLE budget_reservations (
  id             INTEGER PRIMARY KEY,
  run_id         TEXT NOT NULL,
  task_id        TEXT NOT NULL,
  attempt_id     INTEGER NOT NULL UNIQUE REFERENCES attempts(id),
  model          TEXT NOT NULL,
  class          TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  est_tokens     REAL,                        -- NULL when the estimate is unknown
  est_basis      TEXT NOT NULL,
  pools_json     TEXT NOT NULL,               -- [{"pool","reserve","basis":"ratio"|"unit"}]
  belt_usd       REAL,                        -- --max-budget-usd passed (estimate), NULL if none
  enforced       INTEGER NOT NULL,
  state          TEXT NOT NULL CHECK (state IN ('open','settled')),
  settled_at     TEXT,
  actual_tokens  INTEGER,                     -- sum of the four usage classes; NULL if unknown
  actual_json    TEXT,                        -- {"input","output","cache_read","cache_create"}
  actual_basis   TEXT CHECK (actual_basis IN ('usage','unknown','orphan')), -- orphan: settled as not live
  decision_eid   INTEGER                      -- budget_events.id of the admitting decision
);
CREATE INDEX budget_reservations_open ON budget_reservations(state, run_id);

CREATE TABLE budget_parks (
  run_id       TEXT NOT NULL,
  task_id      TEXT NOT NULL,
  rule         TEXT NOT NULL,                 -- budget.Rule* of the refusal
  reason       TEXT NOT NULL,
  parked_at    TEXT NOT NULL,
  admit_at     TEXT,                          -- projected admit time, NULL if unknown
  not_before   TEXT NOT NULL,
  decision_eid INTEGER NOT NULL,
  PRIMARY KEY (run_id, task_id)
);

CREATE TABLE budget_breakers (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL CHECK (kind IN ('RETRY_STORM','SAME_FAILURE','VERIFIER_ERROR','RATE_LIMITED')), -- VERIFIER_ERROR: logged only
  scope       TEXT NOT NULL CHECK (scope IN ('global','run','task')),
  run_id      TEXT NOT NULL DEFAULT '',
  task_id     TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL CHECK (state IN ('open','closed')),
  count       INTEGER NOT NULL DEFAULT 0,     -- consecutive count (VERIFIER_ERROR, RATE_LIMITED openings)
  opened_at   TEXT,
  closes_at   TEXT,                           -- NULL = stays open until a human/run end
  closed_at   TEXT,
  reason      TEXT NOT NULL DEFAULT '',
  enforced    INTEGER NOT NULL,
  inputs_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX budget_breakers_open ON budget_breakers(state, kind);

-- Outbox for budget.jsonl (same pattern as `events`, separate file).
CREATE TABLE budget_events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id    TEXT NOT NULL,
  task_id   TEXT NOT NULL DEFAULT '',
  type      TEXT NOT NULL,                    -- budget_*
  data_json TEXT NOT NULL,
  ts        TEXT NOT NULL,
  flushed   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX budget_events_unflushed ON budget_events(run_id, flushed, id);
