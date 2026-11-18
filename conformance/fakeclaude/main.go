// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit it to make a test pass.
//
// Command fakeclaude is the conformance suite's stand-in for the `claude`
// CLI. It never calls a model. It speaks just enough stream-json for an
// orchestrator to supervise it, and it records what it was asked to do in a
// ledger the tests read.
//
// Environment:
//
//	CONF_FAKE_DIR  directory for scenario.json, usage.json and the ledger
//	GOSPLAN_TASK, GOSPLAN_ATTEMPT  set by the orchestrator
//
// `claude -p /usage ...` prints $CONF_FAKE_DIR/usage.json (the recorded
// /usage result) and exits 0.
//
// Otherwise it runs the steps for "<task>.<attempt>" or "<task>" from
// scenario.json, default [{"op":"commit"}]. Ops:
//
//	commit      {file, content}  write file (default fake-<task>.txt) and git commit it
//	write       {file, content}  write a file in the worktree, no commit
//	emit        {line}           print one raw stdout line
//	emit_file   {path}           print every line of $CONF_FAKE_DIR/<path>
//	sleep       {seconds, heartbeat}  sleep; heartbeat prints one line per second
//	hang        {}               print nothing and sleep for an hour
//	spawn_child {}               start `sleep 3600` in the same process group
//	write_env   {}               write the environment to $CONF_FAKE_DIR/env-<task>-<attempt>.txt
//	result      {text, is_error, subtype}  set the final result line
//	exit        {code}           finish now with this exit code
//	rate_limit  {window, utilization, resets_in, status, unified}  print a
//	            rate_limit_event; resetsAt = now + resets_in (unix seconds);
//	            unified = {name: {utilization, resets_in}}
//	api_retry   {attempt, retry_delay_ms, error, error_status}  print a
//	            system/api_retry line (error default rate_limit, status 429)
//	usage       {input, output, cache_read, cache_create, cost, num_turns,
//	            duration_ms}  set these fields of the final result line
//
// The ledger ($CONF_FAKE_DIR/ledger.jsonl, one JSON object per line, flocked)
// gets "start", "child", "overlap", "end" and "usage" records. "overlap" means
// another fake for the same task held the task lock when this one started.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type step struct {
	Op        string `json:"op"`
	File      string `json:"file"`
	Content   string `json:"content"`
	Line      string `json:"line"`
	Path      string `json:"path"`
	Seconds   int    `json:"seconds"`
	Heartbeat bool   `json:"heartbeat"`
	Text      string `json:"text"`
	IsError   bool   `json:"is_error"`
	Subtype   string `json:"subtype"`
	Code      int    `json:"code"`

	// Budget ops.
	Window      string                `json:"window"`
	Utilization *float64              `json:"utilization"`
	ResetsIn    int64                 `json:"resets_in"`
	Status      string                `json:"status"`
	Unified     map[string]unifiedWin `json:"unified"`
	Attempt     int                   `json:"attempt"`
	RetryDelay  int64                 `json:"retry_delay_ms"`
	Error       string                `json:"error"`
	ErrorStatus int                   `json:"error_status"`
	Input       *int64                `json:"input"`
	Output      *int64                `json:"output"`
	CacheRead   *int64                `json:"cache_read"`
	CacheCreate *int64                `json:"cache_create"`
	Cost        *float64              `json:"cost"`
	NumTurns    *int                  `json:"num_turns"`
	DurationMS  *int64                `json:"duration_ms"`
}

type unifiedWin struct {
	Utilization *float64 `json:"utilization"`
	ResetsIn    int64    `json:"resets_in"`
}

// final holds the result-line fields the usage op can override.
var final = struct {
	input, output, cacheRead, cacheCreate int64
	cost                                  float64
	numTurns                              int
	durationMS                            int64
}{input: 2, output: 4, cost: 0.001, numTurns: 1, durationMS: 10}

var (
	dir     = os.Getenv("CONF_FAKE_DIR")
	task    = os.Getenv("GOSPLAN_TASK")
	attempt = os.Getenv("GOSPLAN_ATTEMPT")
	out     = bufio.NewWriter(os.Stdout)
	argv    = os.Args[1:]
	prompt  string
	session string
	model   string
)

func main() {
	for i := 0; i < len(argv); i++ {
		next := func() string {
			if i+1 < len(argv) {
				i++
				return argv[i]
			}
			return ""
		}
		switch argv[i] {
		case "-p", "--print":
			prompt = next()
		case "--session-id":
			session = next()
		case "--model":
			model = next()
		}
	}
	if strings.TrimSpace(prompt) == "/usage" {
		ledger(map[string]any{"event": "usage"})
		b, err := os.ReadFile(filepath.Join(dir, "usage.json"))
		if err != nil {
			b = []byte(`{"type":"result","subtype":"success","is_error":false,"result":""}`)
		}
		os.Stdout.Write(b)
		os.Exit(0)
	}

	key := task + "-" + attempt
	if dir != "" {
		_ = os.MkdirAll(filepath.Join(dir, "locks"), 0o755)
		_ = os.MkdirAll(filepath.Join(dir, "live"), 0o755)
		lf, err := os.OpenFile(filepath.Join(dir, "locks", task+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
		if err == nil {
			if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
				ledger(map[string]any{"event": "overlap"})
			}
			// lf stays open (and locked) until this process dies.
		}
		_ = os.WriteFile(filepath.Join(dir, "live", strconv.Itoa(os.Getpid())), nil, 0o644)
	}
	cwd, _ := os.Getwd()
	ledger(map[string]any{"event": "start", "argv": argv, "cwd": cwd, "live": liveCount(), "prompt_len": len(prompt), "prompt": prompt})

	emitJSON(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": model, "cwd": cwd})

	steps := scenario()
	res := map[string]any{"text": "done", "is_error": false, "subtype": "success"}
	code := 0
	for _, s := range steps {
		switch s.Op {
		case "commit":
			f := s.File
			if f == "" {
				f = "fake-" + task + ".txt"
			}
			c := s.Content
			if c == "" {
				c = "ok " + key + "\n"
			}
			must(os.WriteFile(f, []byte(c), 0o644))
			gitRun("add", "--", f)
			gitRun("commit", "-q", "-m", "fake work for "+key)
			emitJSON(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "committed " + f}}}})
		case "write":
			must(os.MkdirAll(filepath.Dir(s.File), 0o755))
			must(os.WriteFile(s.File, []byte(s.Content), 0o644))
		case "emit":
			out.WriteString(s.Line + "\n")
			out.Flush()
		case "emit_file":
			b, err := os.ReadFile(filepath.Join(dir, s.Path))
			must(err)
			for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
				out.WriteString(l + "\n")
			}
			out.Flush()
		case "sleep":
			for i := 0; i < s.Seconds; i++ {
				if s.Heartbeat {
					emitJSON(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "working"}}}})
				}
				time.Sleep(time.Second)
			}
		case "hang":
			time.Sleep(time.Hour)
		case "spawn_child":
			c := exec.Command("sleep", "3600")
			must(c.Start())
			ledger(map[string]any{"event": "child", "child_pid": c.Process.Pid})
		case "write_env":
			must(os.WriteFile(filepath.Join(dir, "env-"+key+".txt"), []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o644))
		case "result":
			res = map[string]any{"text": s.Text, "is_error": s.IsError, "subtype": orDefault(s.Subtype, "success")}
		case "exit":
			code = s.Code
			goto finish
		case "rate_limit":
			info := map[string]any{"status": orDefault(s.Status, "allowed"), "resetsAt": time.Now().Unix() + s.ResetsIn}
			if s.Window != "" {
				info["rateLimitType"] = s.Window
			}
			if s.Utilization != nil {
				info["utilization"] = *s.Utilization
			}
			if len(s.Unified) > 0 {
				uw := map[string]any{}
				for name, w := range s.Unified {
					e := map[string]any{"resetsAt": time.Now().Unix() + w.ResetsIn}
					if w.Utilization != nil {
						e["utilization"] = *w.Utilization
					}
					uw[name] = e
				}
				info["unifiedWindows"] = uw
			}
			emitJSON(map[string]any{"type": "rate_limit_event", "rate_limit_info": info, "uuid": "00000000-0000-4000-8000-000000000000", "session_id": session})
		case "api_retry":
			st := s.ErrorStatus
			if st == 0 {
				st = 429
			}
			emitJSON(map[string]any{"type": "system", "subtype": "api_retry", "attempt": s.Attempt, "max_retries": 10,
				"retry_delay_ms": s.RetryDelay, "error_status": st, "error": orDefault(s.Error, "rate_limit"), "session_id": session})
		case "usage":
			set64 := func(dst *int64, v *int64) {
				if v != nil {
					*dst = *v
				}
			}
			set64(&final.input, s.Input)
			set64(&final.output, s.Output)
			set64(&final.cacheRead, s.CacheRead)
			set64(&final.cacheCreate, s.CacheCreate)
			set64(&final.durationMS, s.DurationMS)
			if s.Cost != nil {
				final.cost = *s.Cost
			}
			if s.NumTurns != nil {
				final.numTurns = *s.NumTurns
			}
		default:
			fmt.Fprintln(os.Stderr, "fakeclaude: unknown op", s.Op)
			os.Exit(97)
		}
	}
finish:
	emitJSON(map[string]any{
		"type": "result", "subtype": res["subtype"], "is_error": res["is_error"],
		"num_turns": final.numTurns, "duration_ms": final.durationMS, "duration_api_ms": 5, "total_cost_usd": final.cost,
		"usage": map[string]any{"input_tokens": final.input, "output_tokens": final.output,
			"cache_read_input_tokens": final.cacheRead, "cache_creation_input_tokens": final.cacheCreate},
		"result": res["text"], "session_id": session, "stop_reason": "end_turn",
	})
	ledger(map[string]any{"event": "end", "code": code})
	if dir != "" {
		_ = os.Remove(filepath.Join(dir, "live", strconv.Itoa(os.Getpid())))
	}
	os.Exit(code)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func scenario() []step {
	def := []step{{Op: "commit"}}
	if dir == "" {
		return def
	}
	b, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		return def
	}
	var m map[string][]step
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude: scenario.json:", err)
		os.Exit(98)
	}
	if s, ok := m[task+"."+attempt]; ok {
		return s
	}
	if s, ok := m[task]; ok {
		return s
	}
	return def
}

func emitJSON(v any) {
	b, _ := json.Marshal(v)
	out.Write(append(b, '\n'))
	out.Flush()
}

func gitRun(args ...string) {
	c := exec.Command("git", append([]string{"-c", "user.name=fake", "-c", "user.email=fake@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if b, err := c.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: git %v: %v\n%s", args, err, b)
		os.Exit(96)
	}
}

func liveCount() int {
	if dir == "" {
		return 0
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "live"))
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if syscall.Kill(pid, 0) == nil {
			n++
		}
	}
	return n
}

func ledger(rec map[string]any) {
	if dir == "" {
		return
	}
	rec["task"], rec["attempt"] = task, attempt
	rec["pid"], rec["pgid"] = os.Getpid(), pgid()
	rec["session_id"], rec["model"] = session, model
	rec["t_ms"] = time.Now().UnixMilli()
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync()
}

func pgid() int {
	g, _ := syscall.Getpgid(0)
	return g
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(99)
	}
}
