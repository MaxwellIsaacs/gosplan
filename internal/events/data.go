package events

// Payload types for each event Type. Field names and meanings follow
// the M1 event schema; JSON tags are the wire format.

type RunStartedData struct {
	Spec           string `json:"spec"`
	Project        string `json:"project"`
	ConfigSHA256   string `json:"config_sha256"`
	GosplanVersion string `json:"gosplan_version"`
}

type TaskStateData struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

type LaunchData struct {
	Attempt    int    `json:"attempt"`
	Model      string `json:"model"`
	SessionID  string `json:"session_id"`
	BaseSHA    string `json:"base_sha"`
	Branch     string `json:"branch"`
	ArgvSHA256 string `json:"argv_sha256"`
}

type Usage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
}

type AttemptResultData struct {
	Attempt    int     `json:"attempt"`
	ExitCode   int     `json:"exit_code"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Usage      Usage   `json:"usage"`
}

type GateStatus string

const (
	GatePass GateStatus = "pass"
	GateFail GateStatus = "fail"
	GateSkip GateStatus = "skip"
)

type GateData struct {
	Name       string     `json:"name"`
	Status     GateStatus `json:"status"`
	Detail     string     `json:"detail,omitempty"`
	DurationMS int64      `json:"duration_ms"`
}

type CommandResult struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
}

type VerdictData struct {
	TreeSHA     string          `json:"tree_sha"`
	CacheHit    bool            `json:"cache_hit"`
	Commands    []CommandResult `json:"commands"`
	Result      string          `json:"result"` // "pass" | "fail"
	FailedGates []string        `json:"failed_gates"`
}

type BlockerData struct {
	Reason     string `json:"reason"`
	QuestionID string `json:"question_id"`
}

type RunFinishedData struct {
	Status string `json:"status"` // complete | halted | aborted
	Reason string `json:"reason,omitempty"`
}
