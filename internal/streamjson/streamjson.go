// Package streamjson parses the lines of `claude -p --output-format
// stream-json --verbose` that the orchestrator cares about. Unknown fields and
// unknown line types are ignored; a line that is not JSON is an error for that
// line only.
package streamjson

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

// Line is one decoded stream line. Raw holds the whole line.
type Line struct {
	Type    string
	Subtype string
	Raw     json.RawMessage
}

// Decode parses the envelope of a single stream line.
func Decode(line []byte) (Line, error) {
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return Line{}, fmt.Errorf("streamjson: decode line: %w", err)
	}
	raw := make(json.RawMessage, len(line))
	copy(raw, line)
	return Line{Type: env.Type, Subtype: env.Subtype, Raw: raw}, nil
}

// Result is the terminal `result` line. CostUSDEstimate is claude's
// client-side list-price estimate (total_cost_usd), not billing or plan usage.
type Result struct {
	Subtype, Result, SessionID, StopReason, TerminalReason string
	IsError                                                bool
	NumTurns                                               int
	DurationMS                                             int64
	CostUSDEstimate                                        float64
	Usage                                                  events.Usage
	PermissionDenials                                      int
	// Undocumented: api_error_status, api_error_type and api_error are not in
	// the claude docs. They are what a usage-limit refusal carries.
	APIErrorStatus int
	APIErrorType   string
	APIError       string
}

// ParseResult parses a `result` line. It errors if l.Type != "result".
func ParseResult(l Line) (Result, error) {
	if l.Type != "result" {
		return Result{}, fmt.Errorf("streamjson: line type %q is not result", l.Type)
	}
	var w struct {
		Subtype        string  `json:"subtype"`
		Result         string  `json:"result"`
		SessionID      string  `json:"session_id"`
		StopReason     string  `json:"stop_reason"`
		TerminalReason string  `json:"terminal_reason"`
		IsError        bool    `json:"is_error"`
		NumTurns       int     `json:"num_turns"`
		DurationMS     int64   `json:"duration_ms"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Usage          struct {
			Input       int64 `json:"input_tokens"`
			Output      int64 `json:"output_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		PermissionDenials []json.RawMessage `json:"permission_denials"`
		APIErrorStatus    int               `json:"api_error_status"`
		APIErrorType      string            `json:"api_error_type"`
		APIError          string            `json:"api_error"`
	}
	if err := json.Unmarshal(l.Raw, &w); err != nil {
		return Result{}, fmt.Errorf("streamjson: parse result: %w", err)
	}
	return Result{
		Subtype:         w.Subtype,
		Result:          w.Result,
		SessionID:       w.SessionID,
		StopReason:      w.StopReason,
		TerminalReason:  w.TerminalReason,
		IsError:         w.IsError,
		NumTurns:        w.NumTurns,
		DurationMS:      w.DurationMS,
		CostUSDEstimate: w.TotalCostUSD,
		Usage: events.Usage{
			Input:       w.Usage.Input,
			Output:      w.Usage.Output,
			CacheRead:   w.Usage.CacheRead,
			CacheCreate: w.Usage.CacheCreate,
		},
		PermissionDenials: len(w.PermissionDenials),
		APIErrorStatus:    w.APIErrorStatus,
		APIErrorType:      w.APIErrorType,
		APIError:          w.APIError,
	}, nil
}

// UsageLimited reports whether the session ended because the plan's usage
// limit refused it (a five-hour or weekly window is full), not because of
// anything the worker did. Claude reports that as an error result with
// api_error "usage_limit_reached", or a 429 rate_limit_error.
func (r Result) UsageLimited() bool {
	if !r.IsError {
		return false
	}
	return r.APIError == "usage_limit_reached" ||
		(r.APIErrorStatus == 429 && r.APIErrorType == "rate_limit_error")
}

const blockedPrefix = "GOSPLAN_BLOCKED:"

// BlockedRequest returns the reason from the last line of r.Result that starts
// with "GOSPLAN_BLOCKED:" (after trimming spaces) and states a real blocker. A line
// whose reason only says there is none ("GOSPLAN_BLOCKED: none - task complete") is
// not a block and is skipped, so an earlier real block still counts. ok is
// false if there is none.
func (r Result) BlockedRequest() (reason string, ok bool) {
	lines := strings.Split(r.Result, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if rest, found := strings.CutPrefix(t, blockedPrefix); found {
			reason := strings.TrimSpace(rest)
			if noBlocker(reason) {
				continue
			}
			return reason, true
		}
	}
	return "", false
}

var negations = []string{"none", "n/a", "na", "nil", "nothing", "no", "no blocker", "no blockers", "no issues", "not blocked"}

var completions = map[string]bool{
	"": true, "complete": true, "completed": true, "done": true, "finished": true,
	"all done": true, "task complete": true, "task completed": true, "task done": true,
	"task finished": true, "work complete": true, "required": true,
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// noBlocker reports whether a reason only says there is no blocker: empty, a
// negation word alone, or a negation followed by separators and a short
// completion phrase ("none - task complete", "(none)", "n/a, done"). Anything
// else after the negation ("none; tests cannot run") is a real reason.
// "task complete" alone also counts. Separators are any non-letter, non-digit.
func noBlocker(reason string) bool {
	core := strings.ToLower(strings.TrimFunc(reason, func(r rune) bool { return !isAlnum(r) && r != '/' }))
	core = strings.Join(strings.Fields(core), " ")
	if completions[core] {
		return true
	}
	for _, n := range negations {
		rest, found := strings.CutPrefix(core, n)
		if !found {
			continue
		}
		if rest != "" {
			if first, _ := utf8.DecodeRuneInString(rest); isAlnum(first) {
				continue // "nonexistent", "nothing"-prefix of a longer word
			}
		}
		rest = strings.Join(strings.Fields(strings.TrimFunc(rest, func(r rune) bool { return !isAlnum(r) })), " ")
		if completions[rest] {
			return true
		}
	}
	return false
}

// Window is one entry of the undocumented unifiedWindows map.
type Window struct {
	Utilization *float64 // fraction 0..1
	ResetsAt    *int64   // unix seconds
}

// RateLimit is a parsed rate_limit_event. Fields prefixed Undocumented come
// from keys the claude docs do not describe and may change without notice.
type RateLimit struct {
	Status              string
	ResetsAt            *int64
	Utilization         *float64
	UndocumentedType    string            // rateLimitType
	UndocumentedWindows map[string]Window // unifiedWindows
}

// ParseRateLimit parses a rate_limit_event. It reads rate_limit_info and also
// accepts the same fields at the top level of the line.
func ParseRateLimit(l Line) (RateLimit, error) {
	if l.Type != "rate_limit_event" {
		return RateLimit{}, fmt.Errorf("streamjson: line type %q is not rate_limit_event", l.Type)
	}
	type info struct {
		Status      string   `json:"status"`
		ResetsAt    *int64   `json:"resetsAt"`
		Utilization *float64 `json:"utilization"`
		Type        string   `json:"rateLimitType"`
		Windows     map[string]struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
	}
	var w struct {
		info
		Info *info `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(l.Raw, &w); err != nil {
		return RateLimit{}, fmt.Errorf("streamjson: parse rate_limit_event: %w", err)
	}
	in := w.info
	if w.Info != nil {
		in = *w.Info
	}
	rl := RateLimit{
		Status:           in.Status,
		ResetsAt:         in.ResetsAt,
		Utilization:      in.Utilization,
		UndocumentedType: in.Type,
	}
	if in.Windows != nil {
		rl.UndocumentedWindows = make(map[string]Window, len(in.Windows))
		for k, v := range in.Windows {
			rl.UndocumentedWindows[k] = Window{Utilization: v.Utilization, ResetsAt: v.ResetsAt}
		}
	}
	return rl, nil
}

// APIRetry is a system/api_retry line.
type APIRetry struct {
	Attempt, MaxRetries int
	RetryDelayMS        int64
	ErrorStatus         int
	Error               string
}

// ParseAPIRetry parses a system/api_retry line; ok is false for any other line.
func ParseAPIRetry(l Line) (APIRetry, bool, error) {
	if l.Type != "system" || l.Subtype != "api_retry" {
		return APIRetry{}, false, nil
	}
	var w struct {
		Attempt      int    `json:"attempt"`
		MaxRetries   int    `json:"max_retries"`
		RetryDelayMS int64  `json:"retry_delay_ms"`
		ErrorStatus  int    `json:"error_status"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(l.Raw, &w); err != nil {
		return APIRetry{}, false, fmt.Errorf("streamjson: parse api_retry: %w", err)
	}
	return APIRetry{w.Attempt, w.MaxRetries, w.RetryDelayMS, w.ErrorStatus, w.Error}, true, nil
}

// LastRejectedReset scans a captured stream file for rate_limit_event lines
// with status "rejected" and returns the latest resetsAt among them (unix
// seconds). ok is false if no rejected event carries a reset time.
func LastRejectedReset(path string) (resetsAt int64, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		b, rerr := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(b))) > 0 {
			if l, derr := Decode(b); derr == nil && l.Type == "rate_limit_event" {
				if rl, perr := ParseRateLimit(l); perr == nil && rl.Status == "rejected" && rl.ResetsAt != nil && *rl.ResetsAt > resetsAt {
					resetsAt, ok = *rl.ResetsAt, true
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return 0, false, rerr
			}
			return resetsAt, ok, nil
		}
	}
}

// LastResult scans a captured stream file and returns the last result line.
// Lines that are not JSON are skipped; ok is false if there is no result line.
func LastResult(path string) (Result, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, false, err
	}
	defer f.Close()
	var last Result
	found := false
	r := bufio.NewReader(f)
	for {
		b, rerr := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(b))) > 0 {
			if l, derr := Decode(b); derr == nil && l.Type == "result" {
				res, perr := ParseResult(l)
				if perr != nil {
					return Result{}, false, perr
				}
				last, found = res, true
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return Result{}, false, rerr
			}
			break
		}
	}
	return last, found, nil
}
