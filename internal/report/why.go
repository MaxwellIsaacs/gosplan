package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Why is the text status's explanation of a run that did not pass: the
// reason the run ended (from the run_finished event in events.jsonl), and
// for each task that stopped, its reason and the last line of the failing
// gate's log, so nobody has to dig through verify-logs/. lastFailure maps a
// task to its last_failure text. It returns "" when there is nothing to say.
func Why(r Report, runDir string, lastFailure map[string]string) string {
	var b strings.Builder
	if reason := finishReason(filepath.Join(runDir, "events.jsonl")); reason != "" && r.Status != "complete" {
		fmt.Fprintf(&b, "%s: %s\n", r.Status, reason)
		if r.Status == "halted" {
			fmt.Fprintf(&b, "  continue it: gosplan run --spec %s --project %s --continue %s\n", r.SpecDir, r.Project, r.Run)
		}
	}
	for _, t := range r.Tasks {
		if t.Status != "failed" && t.Status != "blocked" {
			continue
		}
		why := t.BlockedReason
		if why == "" {
			why = lastFailure[t.ID]
		}
		fmt.Fprintf(&b, "%s %s: %s\n", t.ID, t.Status, firstLineOf(why))
		if line, path := lastGateLine(runDir, t); line != "" {
			fmt.Fprintf(&b, "  last log line: %s\n  (%s)\n", line, path)
		}
	}
	return b.String()
}

// finishReason is the reason of the newest run_finished event, or "".
func finishReason(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	reason := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "run_finished" {
			reason = ev.Data.Reason
		}
	}
	return reason
}

// lastGateLine finds the newest attempt with failed cmd gates and returns
// the last non-empty line of the first failing gate's log, with its path.
func lastGateLine(runDir string, t Task) (line, path string) {
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		a := t.Attempts[i]
		if a.Verdict == nil {
			continue
		}
		for _, g := range a.Verdict.FailedGates {
			name, ok := strings.CutPrefix(g, "cmd:")
			if !ok {
				continue
			}
			dir := filepath.Join(runDir, "verify-logs", t.ID, strconv.Itoa(a.N))
			seqs, _ := os.ReadDir(dir)
			sort.Slice(seqs, func(i, j int) bool { return seqNum(seqs[i].Name()) > seqNum(seqs[j].Name()) })
			for _, s := range seqs {
				p := filepath.Join(dir, s.Name(), t.ID+"."+name+".log")
				if l := lastLine(p); l != "" {
					return l, p
				}
			}
		}
		return "", ""
	}
	return "", ""
}

func seqNum(s string) int { n, _ := strconv.Atoi(s); return n }

func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 64<<10 {
		b = b[len(b)-64<<10:]
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			return l
		}
	}
	return ""
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "-"
	}
	return s
}
