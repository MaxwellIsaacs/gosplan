package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseSequenceForms(t *testing.T) {
	want := [][]string{{"1.a"}, {"2.a", "2.b"}, {"3.a"}}
	for name, in := range map[string]string{
		"parens":    "1.a -> (2.a || 2.b) -> 3.a",
		"no parens": "1.a -> 2.a || 2.b -> 3.a\n",
		"multiline": "# header\n1.a ->  # first\n\n  (2.a ||\n 2.b)\n-> 3.a # last\n",
	} {
		got, err := ParseSequence(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
}

func TestParseSequenceErrors(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate":  "1.a -> 2.a || 1.a",
		"bad upper":  "1.a -> 2.B",
		"bad order":  "a.1",
		"empty tier": "1.a -> -> 2.a",
		"empty file": "# only a comment\n\n",
		"empty task": "1.a -> 2.a || ",
	} {
		if got, err := ParseSequence(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want error, got %v", name, got)
		}
	}
}

func depsOf(t *testing.T, sp *Spec, id string) Task {
	t.Helper()
	for _, tk := range sp.Tasks {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("no task %s", id)
	return Task{}
}

func TestLoadTierDerivedEdges(t *testing.T) {
	sp, err := Load("testdata/tiers-only")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][]string{"1.a": nil, "2.a": {"1.a"}, "2.x": {"1.a"}, "3.a": {"2.a", "2.x"}} {
		tk := depsOf(t, sp, id)
		if !reflect.DeepEqual(tk.Deps, want) {
			t.Errorf("%s deps = %v, want %v", id, tk.Deps, want)
		}
		if tk.DepsSource != "tiers" {
			t.Errorf("%s DepsSource = %q", id, tk.DepsSource)
		}
	}
	if !filepath.IsAbs(sp.Dir) || !filepath.IsAbs(depsOf(t, sp, "1.a").PromptPath) {
		t.Errorf("paths not absolute: %s %s", sp.Dir, depsOf(t, sp, "1.a").PromptPath)
	}
	if got := depsOf(t, sp, "1.a").Scope; !reflect.DeepEqual(got, []string{"internal/a/**"}) {
		t.Errorf("1.a scope = %v", got)
	}
}

func TestLoadSidecarDepsOverride(t *testing.T) {
	sp, err := Load("testdata/explicit-deps")
	if err != nil {
		t.Fatal(err)
	}
	if tk := depsOf(t, sp, "2.b"); len(tk.Deps) != 0 || tk.DepsSource != "sidecar" {
		t.Errorf("2.b = %+v, want root with source sidecar", tk)
	}
	if tk := depsOf(t, sp, "3.a"); !reflect.DeepEqual(tk.Deps, []string{"2.a"}) || tk.DepsSource != "sidecar" {
		t.Errorf("3.a = %+v", tk)
	}
	if tk := depsOf(t, sp, "2.a"); !reflect.DeepEqual(tk.Deps, []string{"1.a"}) || tk.DepsSource != "tiers" {
		t.Errorf("2.a = %+v", tk)
	}
}

// writeSpec builds a temp spec dir: seq is sequence.txt, files maps
// "<name>" under prompts/ to content. Every id in seq gets an empty-ish
// prompt unless listed in skipPrompt.
func writeSpec(t *testing.T, seq string, files map[string]string, skipPrompt ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(p, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sequence.txt", seq)
	skip := map[string]bool{}
	for _, s := range skipPrompt {
		skip[s] = true
	}
	for _, id := range regexp.MustCompile(`[0-9]+\.[a-z]+`).FindAllString(seq, -1) {
		if !skip[id] {
			write(id+".md", "# "+id+"\n")
		}
	}
	for n, b := range files {
		write(n, b)
	}
	return dir
}

func TestLoadRejects(t *testing.T) {
	seq := "1.a -> (2.a || 2.b) -> 3.a"
	for name, c := range map[string]struct {
		files map[string]string
		skip  []string
		want  string
	}{
		"unknown dep":  {map[string]string{"2.a.task.toml": `deps = ["9.z"]`}, nil, "9.z"},
		"self dep":     {map[string]string{"2.a.task.toml": `deps = ["2.a"]`}, nil, "itself"},
		"same tier":    {map[string]string{"2.a.task.toml": `deps = ["2.b"]`}, nil, "not lower"},
		"later tier":   {map[string]string{"2.a.task.toml": `deps = ["3.a"]`}, nil, "not lower"},
		"missing":      {nil, []string{"2.b"}, "2.b"},
		"scope string": {map[string]string{"2.a.task.toml": `scope = "x"`}, nil, "2.a"},
		"deps string":  {map[string]string{"2.a.task.toml": `deps = "1.a"`}, nil, "2.a"},
		"bool string":  {map[string]string{"2.a.task.toml": `allow_new_protected = "yes"`}, nil, "2.a"},
	} {
		sp, err := Load(writeSpec(t, seq, c.files, c.skip...))
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if sp != nil {
			t.Errorf("%s: spec returned with error", name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
}

func TestSidecarScopeEmptyIsUnrestricted(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		body string
		want []string
	}{
		"empty":   {"scope = []\n", nil},
		"missing": {"allow_new_protected = true\n", nil},
		"set":     {`scope = ["a/**"]`, []string{"a/**"}},
	} {
		p := filepath.Join(dir, name+".toml")
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		sc, ok, err := ReadSidecar(p)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
		if !reflect.DeepEqual(sc.Scope, c.want) || (c.want == nil && sc.Scope != nil) {
			t.Errorf("%s: scope = %#v, want %#v", name, sc.Scope, c.want)
		}
	}
	if _, ok, err := ReadSidecar(filepath.Join(dir, "absent.toml")); ok || err != nil {
		t.Errorf("absent: ok=%v err=%v", ok, err)
	}
}

func TestSidecarUnknownKeyWarns(t *testing.T) {
	dir := writeSpec(t, "1.a", map[string]string{"1.a.task.toml": `model = "opus"`})
	sp, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Warnings) != 1 || !strings.Contains(sp.Warnings[0], "model") {
		t.Errorf("warnings = %v", sp.Warnings)
	}
}

func TestSidecarBashCompatible(t *testing.T) {
	re := regexp.MustCompile(`^scope\s*=\s*\[("[^"]+"(\s*,\s*"[^"]+")*)?\]$`)
	n := 0
	err := filepath.WalkDir("testdata", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".task.toml") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "scope") {
				n++
				if !re.MatchString(line) {
					t.Errorf("%s: %q is not bash-parseable", p, line)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Errorf("only %d scope lines checked", n)
	}
}

// diamondSpec: 1.a -> {2.a,2.b} -> 3.a, plus independent 2.c.
func diamondSpec(t *testing.T) *Spec {
	t.Helper()
	sp, err := Load(writeSpec(t, "1.a -> (2.a || 2.b || 2.c) -> 3.a", map[string]string{
		"2.c.task.toml": "deps = []\n",
		"3.a.task.toml": `deps = ["2.a", "2.b"]`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestReadyNoTierBarrier(t *testing.T) {
	sp := diamondSpec(t)
	st := map[string]string{"1.a": "passed", "2.a": "failed", "2.b": "running", "2.c": "pending", "3.a": "pending"}
	if got := sp.Ready(st); !reflect.DeepEqual(got, []string{"2.c"}) {
		t.Errorf("Ready = %v, want [2.c]", got)
	}
	if got := sp.Descendants("2.a"); !reflect.DeepEqual(got, []string{"3.a"}) {
		t.Errorf("Descendants(2.a) = %v", got)
	}
	if got := sp.Descendants("2.c"); len(got) != 0 {
		t.Errorf("Descendants(2.c) = %v", got)
	}
	st = map[string]string{"1.a": "pending", "2.a": "pending", "2.b": "pending", "2.c": "pending", "3.a": "pending"}
	if got := sp.Ready(st); !reflect.DeepEqual(got, []string{"1.a", "2.c"}) {
		t.Errorf("initial Ready = %v, want [1.a 2.c]", got)
	}
}

func TestContentHashChanges(t *testing.T) {
	dir := writeSpec(t, "1.a -> 2.a", nil)
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Load(dir)
	if a.ContentSHA256 != again.ContentSHA256 {
		t.Fatal("hash not stable")
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "2.a.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := Load(dir)
	if a.ContentSHA256 == b.ContentSHA256 {
		t.Error("hash did not change after editing a prompt")
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "2.a.task.toml"), []byte("deps = []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(dir)
	if b.ContentSHA256 == c.ContentSHA256 {
		t.Error("hash did not change after adding a sidecar")
	}
}
