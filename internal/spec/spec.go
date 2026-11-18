// Package spec imports a spec directory (sequence.txt, prompts/,
// per-task sidecars) and resolves its dependency edges.
// It must not import internal/state.
package spec

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Task is one task of a spec, after edges are resolved.
type Task struct {
	ID                string
	Tier              int      // 1-based
	PromptPath        string   // absolute path to prompts/<id>.md
	Deps              []string // sorted
	DepsSource        string   // "tiers" | "sidecar"
	Scope             []string // nil = unrestricted
	AllowNewProtected bool
	// Budget keys. Class names are
	// checked against the budget config at run start, not here.
	Class    string   // sidecar class; "" in the sidecar becomes DefaultClass
	Prefer   []string // sidecar prefer: models in preference order; nil = none
	MaxTurns int      // sidecar max_turns; 0 = the run's --max-turns
}

// DefaultClass is the budget class of a task whose sidecar names none.
const DefaultClass = "normal"

// Spec is a loaded and validated spec directory.
type Spec struct {
	Dir           string // absolute
	Tasks         []Task // in sequence order (tier, then position)
	ContentSHA256 string // sha256 over sequence.txt, every prompt and sidecar, in a fixed order
	Warnings      []string
}

var idRE = regexp.MustCompile(`^[0-9]+\.[a-z]+$`)

// ParseSequence parses the sequence.txt grammar into tiers.
func ParseSequence(r io.Reader) ([][]string, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sequence: %w", err)
	}
	if len(lines) == 0 {
		return nil, errors.New("sequence: empty")
	}
	text := strings.NewReplacer("(", " ", ")", " ").Replace(strings.Join(lines, " "))
	seen := map[string]bool{}
	var tiers [][]string
	for k, seg := range strings.Split(text, "->") {
		if strings.TrimSpace(seg) == "" {
			return nil, fmt.Errorf("sequence: tier %d is empty", k+1)
		}
		var tier []string
		for _, id := range strings.Split(seg, "||") {
			id = strings.TrimSpace(id)
			if id == "" {
				return nil, fmt.Errorf("sequence: empty task in tier %d", k+1)
			}
			if !idRE.MatchString(id) {
				return nil, fmt.Errorf("sequence: bad task id %q (want %s)", id, idRE)
			}
			if seen[id] {
				return nil, fmt.Errorf("sequence: duplicate task id %q", id)
			}
			seen[id] = true
			tier = append(tier, id)
		}
		tiers = append(tiers, tier)
	}
	return tiers, nil
}

// Sidecar is the parsed prompts/<id>.task.toml.
type Sidecar struct {
	Scope             []string // nil when missing or []
	AllowNewProtected bool
	Deps              []string
	HasDeps           bool     // key present, even if empty
	Class             string   // budget class as written ("" when absent)
	Prefer            []string // nil when missing or []
	MaxTurns          int      // 0 when absent
	Unknown           []string // unknown keys (warnings)
}

// ReadSidecar reads prompts/<id>.task.toml. ok=false if the file is absent.
func ReadSidecar(path string) (sc Sidecar, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Sidecar{}, false, nil
	}
	if err != nil {
		return Sidecar{}, false, err
	}
	var raw struct {
		Scope             []string `toml:"scope"`
		AllowNewProtected bool     `toml:"allow_new_protected"`
		Deps              []string `toml:"deps"`
		Class             string   `toml:"class"`
		Prefer            []string `toml:"prefer"`
		MaxTurns          int      `toml:"max_turns"`
	}
	md, err := toml.Decode(string(b), &raw)
	if err != nil {
		return Sidecar{}, true, fmt.Errorf("%s: %w", path, err)
	}
	if raw.MaxTurns < 0 {
		return Sidecar{}, true, fmt.Errorf("%s: max_turns must be >= 0, got %d", path, raw.MaxTurns)
	}
	sc.Class = raw.Class
	if len(raw.Prefer) > 0 {
		sc.Prefer = raw.Prefer
	}
	sc.MaxTurns = raw.MaxTurns
	sc.AllowNewProtected = raw.AllowNewProtected
	if len(raw.Scope) > 0 {
		sc.Scope = raw.Scope
	}
	sc.HasDeps = md.IsDefined("deps")
	sc.Deps = raw.Deps
	for _, k := range md.Undecoded() {
		sc.Unknown = append(sc.Unknown, k.String())
	}
	return sc, true, nil
}

// Load reads a spec directory, resolves edges and validates.
func Load(dir string) (*Spec, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	seqPath := filepath.Join(abs, "prompts", "sequence.txt")
	seqBytes, err := os.ReadFile(seqPath)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	tiers, err := ParseSequence(strings.NewReader(string(seqBytes)))
	if err != nil {
		return nil, fmt.Errorf("spec: %s: %w", seqPath, err)
	}

	h := sha256.New()
	hashPart := func(name string, b []byte) {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
		h.Write(b)
	}
	hashPart("sequence.txt", seqBytes)

	sp := &Spec{Dir: abs}
	tierOf := map[string]int{}
	sidecars := map[string]Sidecar{}
	for k, tier := range tiers {
		for _, id := range tier {
			tierOf[id] = k + 1
			prompt := filepath.Join(abs, "prompts", id+".md")
			pb, err := os.ReadFile(prompt)
			if err != nil {
				return nil, fmt.Errorf("spec: task %s: missing prompt %s: %w", id, prompt, err)
			}
			hashPart(id+".md", pb)
			sidecarPath := filepath.Join(abs, "prompts", id+".task.toml")
			sc, ok, err := ReadSidecar(sidecarPath)
			if err != nil {
				return nil, fmt.Errorf("spec: task %s: malformed sidecar: %w", id, err)
			}
			if ok {
				sb, err := os.ReadFile(sidecarPath)
				if err != nil {
					return nil, fmt.Errorf("spec: task %s: %w", id, err)
				}
				hashPart(id+".task.toml", sb)
				for _, u := range sc.Unknown {
					sp.Warnings = append(sp.Warnings, fmt.Sprintf("%s: unknown key %q", sidecarPath, u))
				}
			}
			sidecars[id] = sc
			class := sc.Class
			if class == "" {
				class = DefaultClass
			}
			sp.Tasks = append(sp.Tasks, Task{
				ID: id, Tier: k + 1, PromptPath: prompt,
				Scope: sc.Scope, AllowNewProtected: sc.AllowNewProtected,
				Class: class, Prefer: sc.Prefer, MaxTurns: sc.MaxTurns,
			})
		}
	}

	for i := range sp.Tasks {
		t := &sp.Tasks[i]
		sc := sidecars[t.ID]
		if sc.HasDeps {
			t.DepsSource = "sidecar"
			set := map[string]bool{}
			for _, d := range sc.Deps {
				dt, known := tierOf[d]
				switch {
				case d == t.ID:
					return nil, fmt.Errorf("spec: task %s (%s.task.toml): depends on itself", t.ID, t.ID)
				case !known:
					return nil, fmt.Errorf("spec: task %s (%s.task.toml): dep %q is not in sequence.txt", t.ID, t.ID, d)
				case dt >= t.Tier:
					return nil, fmt.Errorf("spec: task %s (%s.task.toml): dep %s is in tier %d, not lower than tier %d", t.ID, t.ID, d, dt, t.Tier)
				}
				set[d] = true
			}
			for d := range set {
				t.Deps = append(t.Deps, d)
			}
		} else {
			t.DepsSource = "tiers"
			if t.Tier > 1 {
				t.Deps = append(t.Deps, tiers[t.Tier-2]...)
			}
		}
		sort.Strings(t.Deps)
	}
	sp.ContentSHA256 = hex.EncodeToString(h.Sum(nil))
	return sp, nil
}

// Ready returns, in (tier, id) order, the ids of tasks whose status is
// pending and whose deps all have status passed. status maps id -> status.
// A pure function: no clock, no not_before (the state query adds that).
func (s *Spec) Ready(status map[string]string) []string {
	var out []string
	for _, t := range s.Tasks {
		if status[t.ID] != "pending" {
			continue
		}
		ok := true
		for _, d := range t.Deps {
			if status[d] != "passed" {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, t.ID)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return s.less(out[i], out[j]) })
	return out
}

func (s *Spec) less(a, b string) bool {
	var ta, tb int
	for _, t := range s.Tasks {
		if t.ID == a {
			ta = t.Tier
		}
		if t.ID == b {
			tb = t.Tier
		}
	}
	if ta != tb {
		return ta < tb
	}
	return a < b
}

// Descendants returns every task that transitively depends on id, in
// (tier, id) order.
func (s *Spec) Descendants(id string) []string {
	hit := map[string]bool{id: true}
	var out []string
	// Tasks are in tier order and deps point to lower tiers, so one pass suffices.
	for _, t := range s.Tasks {
		for _, d := range t.Deps {
			if hit[d] {
				hit[t.ID] = true
				out = append(out, t.ID)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return s.less(out[i], out[j]) })
	return out
}
