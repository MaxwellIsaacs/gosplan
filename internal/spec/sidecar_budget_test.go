package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// budgetSpecDir writes a two-task spec (1.a -> 2.a) whose 1.a sidecar is
// sidecar, and returns its directory.
func budgetSpecDir(t *testing.T, sidecar string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"sequence.txt":  "1.a -> 2.a\n",
		"1.a.md":        "do a\n",
		"2.a.md":        "do b\n",
		"1.a.task.toml": sidecar,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(p, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBudgetSidecarKeys: class, prefer and max_turns parse into Sidecar and
// Task and are not reported as unknown keys; a task without them gets class
// "normal", no prefer list and max_turns 0.
func TestBudgetSidecarKeys(t *testing.T) {
	dir := budgetSpecDir(t, "scope = [\"a/**\"]\nclass = \"critical\"\nprefer = [\"opus\", \"sonnet\"]\nmax_turns = 40\nallow_new_protected = true\n")
	sc, ok, err := ReadSidecar(filepath.Join(dir, "prompts", "1.a.task.toml"))
	if err != nil || !ok {
		t.Fatalf("ReadSidecar: ok=%v err=%v", ok, err)
	}
	if sc.Class != "critical" || !reflect.DeepEqual(sc.Prefer, []string{"opus", "sonnet"}) || sc.MaxTurns != 40 {
		t.Fatalf("sidecar budget keys: %+v", sc)
	}
	if len(sc.Unknown) != 0 {
		t.Fatalf("budget keys reported unknown: %v", sc.Unknown)
	}
	sp, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Warnings) != 0 {
		t.Fatalf("warnings for budget keys: %v", sp.Warnings)
	}
	a, b := sp.Tasks[0], sp.Tasks[1]
	if a.ID != "1.a" || a.Class != "critical" || !reflect.DeepEqual(a.Prefer, []string{"opus", "sonnet"}) || a.MaxTurns != 40 {
		t.Fatalf("task 1.a: %+v", a)
	}
	if b.ID != "2.a" || b.Class != "normal" || b.Prefer != nil || b.MaxTurns != 0 {
		t.Fatalf("task 2.a (no sidecar): %+v", b)
	}
	// An empty class and an empty prefer list are the defaults.
	sp, err = Load(budgetSpecDir(t, "class = \"\"\nprefer = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c := sp.Tasks[0]; c.Class != "normal" || c.Prefer != nil {
		t.Fatalf("empty class/prefer: %+v", c)
	}
	// Unknown class names are not spec's to judge (run start checks them
	// against the budget config).
	sp, err = Load(budgetSpecDir(t, "class = \"vip\"\n"))
	if err != nil || sp.Tasks[0].Class != "vip" {
		t.Fatalf("custom class: %v %+v", err, sp)
	}
}

// TestBudgetSidecarBadValues: a negative max_turns or a wrongly typed key is
// a malformed sidecar.
func TestBudgetSidecarBadValues(t *testing.T) {
	for _, sc := range []string{"max_turns = -1\n", "class = 3\n", "prefer = \"opus\"\n", "max_turns = \"40\"\n"} {
		if _, err := Load(budgetSpecDir(t, sc)); err == nil {
			t.Errorf("Load accepted sidecar %q", sc)
		}
	}
}

// TestBudgetSidecarV1Unchanged: schema v1 sidecars (no budget keys)
// parse exactly as before: same scope/deps/flags, no unknown keys, and the
// budget fields at their zero values; a truly unknown key still warns.
func TestBudgetSidecarV1Unchanged(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "dynamo-sidecars", "*.task.toml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no v1 sidecars: %v", err)
	}
	for _, f := range files {
		sc, ok, err := ReadSidecar(f)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", f, ok, err)
		}
		if len(sc.Unknown) != 0 {
			t.Errorf("%s: unknown keys %v", f, sc.Unknown)
		}
		if sc.Class != "" || sc.Prefer != nil || sc.MaxTurns != 0 {
			t.Errorf("%s: budget fields set: %+v", f, sc)
		}
	}
	sp, err := Load(budgetSpecDir(t, "scope = [\"a/**\"]\ndeps = []\nclas = \"critical\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Warnings) != 1 {
		t.Fatalf("misspelt key: want one warning, got %v", sp.Warnings)
	}
	if a := sp.Tasks[0]; !reflect.DeepEqual(a.Scope, []string{"a/**"}) || a.DepsSource != "sidecar" || a.Class != "normal" {
		t.Fatalf("task 1.a: %+v", a)
	}
}
