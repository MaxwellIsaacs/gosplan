package window

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Prober runs the $0 `/usage` fallback (no model call). The text layout it
// parses is undocumented. Zero fields take the defaults noted below.
type Prober struct {
	ClaudeBin   string        // default "claude"
	Timeout     time.Duration // default 60s
	MaxAge      time.Duration // probe only if the newest observation is older; default 30m
	MinInterval time.Duration // and the last probe is older; default 15m
	Env         []string      // environment for the child; default os.Environ()
	Now         func() time.Time
}

func (p Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func orDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Due reports whether a probe should run now: the newest observation is
// older than MaxAge and the last probe is older than MinInterval. No
// observation (or probe) counts as infinitely old.
func (p Prober) Due(ctx context.Context, s *state.Store) (bool, error) {
	obs, err := s.LastObservedAt(ctx)
	if err != nil {
		return false, err
	}
	probe, err := s.LastProbeAt(ctx)
	if err != nil {
		return false, err
	}
	now := p.now()
	stale := func(t time.Time, max time.Duration) bool { return t.IsZero() || now.Sub(t) > max }
	return stale(obs, orDur(p.MaxAge, 30*time.Minute)) && stale(probe, orDur(p.MinInterval, 15*time.Minute)), nil
}

// Probe runs `<ClaudeBin> -p /usage --output-format json --no-session-persistence`
// in its own process group under Timeout, parses the JSON result text and
// records the rows. On failure it records one _probe row with Status "error"
// and the cause in Raw, and returns that row together with the error.
func (p Prober) Probe(ctx context.Context, s *state.Store) ([]state.WindowObservation, error) {
	now := p.now()
	text, err := p.run(ctx)
	var obs []state.WindowObservation
	if err != nil {
		obs = []state.WindowObservation{{
			Window: "_probe", Status: "error", Source: "usage_text", Documented: false,
			ObservedAt: now, Raw: truncate(err.Error()),
		}}
	} else {
		obs = ParseUsageText(text, now)
	}
	if rerr := s.RecordWindows(ctx, obs); rerr != nil {
		return obs, rerr
	}
	return obs, err
}

func (p Prober) run(ctx context.Context) (string, error) {
	bin := p.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, orDur(p.Timeout, 60*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", "/usage", "--output-format", "json", "--no-session-persistence")
	cmd.Env = p.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("window: /usage probe: %w", ctx.Err())
		}
		return "", fmt.Errorf("window: /usage probe: %w: %s", err, strings.TrimSpace(truncate(stderr.String())))
	}
	return usageResultText(stdout.Bytes())
}

// usageResultText returns the "result" string of the last JSON line that has one.
func usageResultText(out []byte) (string, error) {
	var text string
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var w struct {
			Result *string `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &w) == nil && w.Result != nil {
			text, found = *w.Result, true
		}
	}
	if !found {
		return "", fmt.Errorf("window: /usage probe: no result in output")
	}
	return text, nil
}
