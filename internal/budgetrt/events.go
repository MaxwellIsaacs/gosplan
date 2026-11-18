package budgetrt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
)

// Event payloads the gate writes to budget.jsonl. FlushBudgetEvents
// adds an "eid" key to each.

// Input kinds of a budget_input event.
const (
	InputConfig = "config"
	InputObs    = "obs"
	InputSpan   = "span"
	InputSample = "sample"
)

// InputEvent is one budget_input line: an input row, logged once per run
// file the first time a decision uses it (a span again when it changed).
// ID is the config sha256 (string) for kind config, else the row id.
type InputEvent struct {
	Kind string          `json:"kind"`
	ID   json.RawMessage `json:"id"`
	Row  json.RawMessage `json:"row"`
}

// DecisionEvent is one budget_decision line: the decision, the ids of the
// input rows it used (each logged before it as a budget_input) and the small
// inputs inline.
type DecisionEvent struct {
	Now          time.Time             `json:"now"`
	Mode         string                `json:"mode"`
	Enforced     bool                  `json:"enforced"`
	Attempt      int                   `json:"attempt"`
	ConfigSHA256 string                `json:"config_sha256"`
	ObsIDs       []int64               `json:"obs_ids"`
	SpanIDs      []int64               `json:"span_ids"`
	SampleIDs    []int64               `json:"sample_ids"`
	Floors       map[string]budget.Obs `json:"floors"`
	Open         []budget.Reservation  `json:"open"`
	Recent       []budget.Reservation  `json:"recent"`
	Breakers     []budget.Breaker      `json:"breakers"`
	Candidates   []budget.Candidate    `json:"candidates"`
	Runs         []budget.RunState     `json:"runs"`
	For          budget.Key            `json:"for"`
	Decision     budget.Decision       `json:"decision"`
	Suppressed   int                   `json:"suppressed"` // identical refusals of this task not logged since its previous logged decision
}

// SettleEvent is one budget_settle line. ActualBasis is usage, unknown or
// orphan; Overrun is actual > estimate (both known).
type SettleEvent struct {
	ReservationID int64                `json:"reservation_id"`
	AttemptID     int64                `json:"attempt_id"`
	EstTokens     *float64             `json:"est_tokens"`
	ActualTokens  *int64               `json:"actual_tokens"`
	ActualBasis   string               `json:"actual_basis"`
	Overrun       bool                 `json:"overrun"`
	Pools         []budget.PoolReserve `json:"pools"`
}

// BreakerEvent is one budget_breaker line: action open | close | count.
type BreakerEvent struct {
	Action  string              `json:"action"`
	Breaker budget.Breaker      `json:"breaker"`
	Inputs  budget.BreakerInput `json:"inputs"`
}

// FitEvent is one budget_fit line, written when a pool's fit changed.
type FitEvent struct {
	Pool      string            `json:"pool"`
	Intervals []budget.Interval `json:"intervals"`
	Fit       budget.Fit        `json:"fit"`
}

// ProbeEvent is one budget_probe line.
type ProbeEvent struct {
	Due   bool   `json:"due"`
	Ran   bool   `json:"ran"`
	Rows  int    `json:"rows"`
	Error string `json:"error"`
}

// Digest is the sha256 of an event's data in canonical form: the JSON is
// decoded into generic values with the "eid" key removed and re-encoded
// (object keys sorted, Go's shortest float form), so the bytes the gate
// wrote and the line budget.jsonl holds compare equal.
func Digest(data []byte) (string, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return "", err
	}
	delete(v, "eid")
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func digestBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Canonical is the canonical JSON of v after a generic round trip (sorted
// keys), as used for comparisons.
func Canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, err
	}
	return json.Marshal(x)
}
