// Package window observes the plan's usage windows (5-hour and weekly) and
// stores what it saw with its source and age. It only observes: nothing here
// or in the store makes a scheduling decision.
//
// Nothing read from unifiedWindows, rateLimitType, /api/oauth/usage or the
// layout of the /usage text is an official API. Rows built from them are
// stored with documented = false (undocumented), and the package never calls
// /api/oauth/usage or reads credentials.
package window

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/streamjson"
)

const rawLimit = 8 << 10

func truncate(b string) string {
	if len(b) > rawLimit {
		return b[:rawLimit]
	}
	return b
}

// FromLine turns one stream-json line into observations. Unrelated or unparsable lines return nil.
func FromLine(line []byte, attemptID *int64, now time.Time) []state.WindowObservation {
	l, err := streamjson.Decode(line)
	if err != nil {
		return nil
	}
	raw := truncate(strings.TrimSpace(string(line)))
	switch {
	case l.Type == "rate_limit_event":
		rl, err := streamjson.ParseRateLimit(l)
		if err != nil {
			return nil
		}
		name := rl.UndocumentedType
		documented := name == ""
		if documented {
			name = "unknown"
		}
		out := []state.WindowObservation{{
			Window: name, Utilization: rl.Utilization, ResetsAt: rl.ResetsAt, Status: rl.Status,
			Source: "rate_limit_event", Documented: documented, ObservedAt: now, AttemptID: attemptID, Raw: raw,
		}}
		// unifiedWindows is undocumented: every row from it is documented = false.
		keys := make([]string, 0, len(rl.UndocumentedWindows))
		for k := range rl.UndocumentedWindows {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w := rl.UndocumentedWindows[k]
			out = append(out, state.WindowObservation{
				Window: k, Utilization: w.Utilization, ResetsAt: w.ResetsAt, Status: rl.Status,
				Source: "rate_limit_event.unifiedWindows", Documented: false, ObservedAt: now, AttemptID: attemptID, Raw: raw,
			})
		}
		return out
	case l.Type == "system" && l.Subtype == "api_retry":
		r, ok, err := streamjson.ParseAPIRetry(l)
		if err != nil || !ok || r.Error != "rate_limit" {
			return nil
		}
		return []state.WindowObservation{{
			Window: "unknown", Status: "rate_limited", Source: "api_retry", Documented: true,
			ObservedAt: now, AttemptID: attemptID, Raw: raw,
		}}
	}
	return nil
}

const pct = `(\d+(?:\.\d+)?)%\s*used`

var (
	usageSessionRe = regexp.MustCompile(`(?i)^Current\s+session\s*:\s*` + pct)
	usageAllRe     = regexp.MustCompile(`(?i)^Current\s+week\s*\(all\s+models\)\s*:\s*` + pct)
	usageModelRe   = regexp.MustCompile(`(?i)^Current\s+week\s*\(([^)]+)\)\s*:\s*` + pct)
)

// ParseUsageText parses /usage result text. The text
// layout is undocumented, so rows have Documented false. No recognised line
// yields a single _probe row with Status "parse_failed".
func ParseUsageText(text string, now time.Time) []state.WindowObservation {
	var out []state.WindowObservation
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		var name, num string
		if m := usageSessionRe.FindStringSubmatch(ln); m != nil {
			name, num = "session", m[1]
		} else if m := usageAllRe.FindStringSubmatch(ln); m != nil {
			name, num = "week_all", m[1]
		} else if m := usageModelRe.FindStringSubmatch(ln); m != nil {
			model := strings.Join(strings.Fields(strings.ToLower(m[1])), "_")
			name, num = "week_"+model, m[2]
		} else {
			continue
		}
		p, err := strconv.ParseFloat(num, 64)
		if err != nil {
			continue
		}
		u := p / 100
		out = append(out, state.WindowObservation{
			Window: name, Utilization: &u, Source: "usage_text", Documented: false,
			ObservedAt: now, Raw: truncate(ln),
		})
	}
	if len(out) == 0 {
		return []state.WindowObservation{{
			Window: "_probe", Status: "parse_failed", Source: "usage_text", Documented: false,
			ObservedAt: now, Raw: truncate(text),
		}}
	}
	return out
}

// Age formats now - observed for display: "12m", "3h", "2d".
func Age(now, observed time.Time) string {
	d := now.Sub(observed)
	switch {
	case d < time.Hour:
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
