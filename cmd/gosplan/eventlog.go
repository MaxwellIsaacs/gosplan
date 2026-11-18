package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/verify"
)

// newRunID returns a run id in the schema's YYYYMMDD-HHMMSS-<rand4> form.
func newRunID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// appendLaunch writes the `launch` event for this attempt. Only the fields
// `gosplan verify` knows are set; the rest stay empty rather than guessed.
func appendLaunch(l *events.Log, task, taskBase string, attempt int) error {
	_, err := l.Append(task, events.Launch, events.LaunchData{Attempt: attempt, BaseSHA: taskBase})
	return err
}

// appendVerdict writes one `gate` event per gate, in gate order, then the
// `verdict` event. A verdict is written for cache hits too.
func appendVerdict(l *events.Log, v verify.Verdict) error {
	for _, g := range v.Gates {
		if _, err := l.Append(v.Task, events.Gate, g); err != nil {
			return fmt.Errorf("gate %s: %w", g.Name, err)
		}
	}
	if _, err := l.Append(v.Task, events.Verdict, v.Event()); err != nil {
		return fmt.Errorf("verdict: %w", err)
	}
	return nil
}
