package reconcile

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Preamble is the harness text DefaultPrompt puts before every task prompt.
const Preamble = `You are a worker in an automated gosplan run. There is no human at the
terminal. An independent verifier checks your branch after you exit; its
verdict is the only one that counts.

Rules:
- Stay in scope: change only what the task below asks for.
- Stage files by name (git add <paths>). Never use git add -A or git add .
- Never modify or delete an existing test file or testdata file from an
  earlier task (*_test.go, tests/**, testdata/**). You may add new ones.
- Commit your work on the current branch before you finish.
- If the task cannot be done honestly, end your final message with a line
  GOSPLAN_BLOCKED: <reason>
  and stop. Do not fake a result. If you finished, do not write an
  GOSPLAN_BLOCKED line at all, not even "GOSPLAN_BLOCKED: none".
`

// failureHeader introduces the previous attempt's failure summary. The
// summary says how it failed (verification, exit code, stall, ...); the
// header must not claim it was verification.
const failureHeader = "Previous attempt did not pass:"

// DefaultPrompt returns Preamble, then (for attempt > 1 with a non-empty
// lastFailure) a "Previous attempt did not pass:" section, then the
// task prompt file read from t.PromptPath (the spec directory, never the
// worktree).
func DefaultPrompt(t state.TaskRow, attempt int, lastFailure string) (string, error) {
	body, err := os.ReadFile(t.PromptPath)
	if err != nil {
		return "", fmt.Errorf("reconcile: prompt for %s: %w", t.ID, err)
	}
	var b strings.Builder
	b.WriteString(Preamble)
	b.WriteString("\n")
	if attempt > 1 && strings.TrimSpace(lastFailure) != "" {
		fmt.Fprintf(&b, "%s\n%s\n\n", failureHeader, strings.TrimRight(lastFailure, "\n"))
	}
	fmt.Fprintf(&b, "Task %s (attempt %d):\n\n", t.ID, attempt)
	b.Write(body)
	return b.String(), nil
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}
