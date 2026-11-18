-- Contract file: changes only by human-reviewed commits.
PRAGMA user_version = 1;

CREATE TABLE specs (
  id             INTEGER PRIMARY KEY,
  dir            TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  imported_at    TEXT NOT NULL,
  UNIQUE (dir, content_sha256)
);

CREATE TABLE runs (
  id                 TEXT PRIMARY KEY,               -- YYYYMMDD-HHMMSS-<rand4>
  spec_id            INTEGER NOT NULL REFERENCES specs(id),
  project            TEXT NOT NULL,                  -- absolute path
  base_sha           TEXT NOT NULL,                  -- full SHA the run starts from
  status             TEXT NOT NULL CHECK (status IN ('running','complete','halted','aborted')),
  started_at         TEXT NOT NULL,
  finished_at        TEXT,
  integration_branch TEXT,
  integration_sha    TEXT
);

CREATE TABLE tasks (
  run_id              TEXT NOT NULL REFERENCES runs(id),
  id                  TEXT NOT NULL,                 -- "2.b"
  tier                INTEGER NOT NULL,              -- 1-based position in sequence.txt
  prompt_path         TEXT NOT NULL,                 -- absolute path to prompts/<id>.md
  status              TEXT NOT NULL CHECK (status IN
                        ('pending','running','verifying','passed','failed','blocked','abandoned')),
  deps_source         TEXT NOT NULL CHECK (deps_source IN ('tiers','sidecar')),
  scope_json          TEXT NOT NULL DEFAULT 'null',  -- JSON array of globs, or null = unrestricted
  allow_new_protected INTEGER NOT NULL DEFAULT 0,
  not_before          TEXT,                          -- retry backoff: not dispatchable before this
  base_sha            TEXT,                          -- set once by the first wt.Prepare
  branch              TEXT,
  worktree            TEXT,
  head_sha            TEXT,                          -- last resolved head
  attempts_used       INTEGER NOT NULL DEFAULT 0,
  verify_errors       INTEGER NOT NULL DEFAULT 0,    -- consecutive gosplan verify exit-2 results
  last_failure        TEXT,                          -- summary fed to the next attempt's prompt
  blocked_reason      TEXT,
  PRIMARY KEY (run_id, id)
);

CREATE TABLE task_deps (
  run_id  TEXT NOT NULL,
  task_id TEXT NOT NULL,
  dep_id  TEXT NOT NULL,
  PRIMARY KEY (run_id, task_id, dep_id),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id),
  FOREIGN KEY (run_id, dep_id)  REFERENCES tasks(run_id, id)
);

CREATE TABLE attempts (
  id                 INTEGER PRIMARY KEY,
  run_id             TEXT NOT NULL,
  task_id            TEXT NOT NULL,
  n                  INTEGER NOT NULL,               -- 1-based
  session_id         TEXT NOT NULL,                  -- UUID passed as --session-id
  model              TEXT NOT NULL,
  started_at         TEXT NOT NULL,
  ended_at           TEXT,
  outcome            TEXT CHECK (outcome IN ('exited','stalled','killed','start_failed','lost')),
  exit_code          INTEGER,
  result_subtype     TEXT,
  is_error           INTEGER,
  num_turns          INTEGER,
  duration_ms        INTEGER,
  cost_usd_estimate  REAL,                           -- client-side list-price estimate, not billing
  usage_json         TEXT,
  stream_path        TEXT,                           -- raw stream-json capture
  blocked_request    TEXT,                           -- reason from an GOSPLAN_BLOCKED line
  head_sha           TEXT,
  verdict_result     TEXT CHECK (verdict_result IN ('pass','fail','error')),
  verdict_json       TEXT,                           -- stdout of gosplan verify --json
  failed_gates_json  TEXT,
  attestation_path   TEXT,
  mutation_json      TEXT,                           -- advisory, never decides
  UNIQUE (run_id, task_id, n),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id)
);

CREATE TABLE leases (
  run_id       TEXT NOT NULL,
  task_id      TEXT NOT NULL,
  attempt_id   INTEGER NOT NULL UNIQUE REFERENCES attempts(id),
  owner        TEXT NOT NULL,                        -- "<hostname>:<pid>:<nonce>" of the orchestrator
  pid          INTEGER,
  pgid         INTEGER,
  acquired_at  TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  -- Added after v1 shipped; last, in the order migrateLeases adds them, so a
  -- fresh and a migrated DB have the same column order.
  nonce        TEXT,                                 -- GOSPLAN_ATTEMPT_NONCE given to the worker, set in the dispatch tx
  start_time   INTEGER,                              -- /proc/<pid>/stat field 22 of the leader
  PRIMARY KEY (run_id, task_id)
);

CREATE TABLE events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id    TEXT NOT NULL,
  task_id   TEXT NOT NULL DEFAULT '',
  type      TEXT NOT NULL,                           -- an events.Type value
  data_json TEXT NOT NULL,
  ts        TEXT NOT NULL,
  flushed   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX events_unflushed ON events(run_id, flushed, id);

CREATE TABLE window_observations (
  id          INTEGER PRIMARY KEY,
  window      TEXT NOT NULL,     -- five_hour | seven_day | session | week_all | week_<model> | unknown | _probe
  utilization REAL,              -- fraction 0..1, NULL if not given
  resets_at   INTEGER,           -- unix seconds, NULL if not given
  status      TEXT,              -- allowed | allowed_warning | rejected | rate_limited | parse_failed | error
  source      TEXT NOT NULL,     -- rate_limit_event | rate_limit_event.unifiedWindows | api_retry | usage_text
  documented  INTEGER NOT NULL,  -- 1 only if every field used is in Anthropic's docs
  observed_at TEXT NOT NULL,
  attempt_id  INTEGER,
  raw_json    TEXT NOT NULL      -- the source line or text, for audit
);
CREATE INDEX window_latest ON window_observations(window, observed_at);
