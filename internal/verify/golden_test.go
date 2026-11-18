package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
)

// goldenDir holds the fixture sources: build.sh, shas.txt and per-fixture
// expect.json and uncommitted/ (testdata/golden/README.md). The repos
// themselves are built at test time by goldenBuild.
const goldenDir = "../../testdata/golden"

// goldenBuild runs build.sh once per test binary into a temp dir and
// checks every fixture's base and head SHA against shas.txt, so a build
// that drifts fails every golden test rather than testing something else.
var goldenBuild = sync.OnceValues(func() (string, error) {
	out, err := os.MkdirTemp("", "gosplan-golden-")
	if err != nil {
		return "", err
	}
	goldenBuildDir = out
	if b, err := exec.Command("bash", filepath.Join(goldenDir, "build.sh"), out).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build.sh: %v\n%s", err, b)
	}
	want, err := readGoldenSHAs(filepath.Join(goldenDir, "shas.txt"))
	if err != nil {
		return "", err
	}
	if err := checkGoldenSHAs(out, want); err != nil {
		return "", err
	}
	return out, nil
})

// checkGoldenSHAs compares each built fixture's main and task commits with
// want (from shas.txt).
func checkGoldenSHAs(built string, want map[string][2]string) error {
	got := map[string][2]string{}
	for _, fx := range goldenFixtures {
		var shas [2]string
		for i, ref := range []string{"main", "task"} {
			b, err := exec.Command("git", "-C", filepath.Join(built, fx.Name), "rev-parse", "--verify", ref+"^{commit}").Output()
			if err != nil {
				return fmt.Errorf("fixture %s: rev-parse %s: %v", fx.Name, ref, err)
			}
			shas[i] = strings.TrimSpace(string(b))
		}
		got[fx.Name] = shas
	}
	if !maps.Equal(got, want) {
		return fmt.Errorf("build.sh output drifted from shas.txt:\nbuilt:    %v\nexpected: %v", got, want)
	}
	return nil
}

// goldenBuildDir is removed by TestMain after the tests.
var goldenBuildDir string

func TestMain(m *testing.M) {
	code := m.Run()
	if goldenBuildDir != "" {
		os.RemoveAll(goldenBuildDir)
	}
	os.Exit(code)
}

// readGoldenSHAs parses shas.txt: "name base head" per line, # comments.
func readGoldenSHAs(path string) (map[string][2]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string][2]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || gitx.RequireFullSHA(f[1]) != nil || gitx.RequireFullSHA(f[2]) != nil {
			return nil, fmt.Errorf("%s:%d: want \"name base head\" with full SHAs, got %q", path, i+1, line)
		}
		if _, dup := out[f[0]]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate fixture %s", path, i+1, f[0])
		}
		out[f[0]] = [2]string{f[1], f[2]}
	}
	return out, nil
}

// goldenRepos returns the built fixture repos, failing the test if the
// build failed or drifted.
func goldenRepos(t *testing.T) string {
	t.Helper()
	dir, err := goldenBuild()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// goldenFixture is one fixture the test requires. Every path it implies
// (the built repo, expect.json, a shas.txt line, and uncommitted/ when
// Uncommitted is set) must exist: deleting any of them fails the test instead of silently changing
// what the fixture checks.
type goldenFixture struct {
	Name        string
	Uncommitted bool // the fixture copies uncommitted/ into the worker tree
}

var goldenFixtures = []goldenFixture{
	{Name: "tampered-protected-test"},
	{Name: "uncommitted-only-pass", Uncommitted: true},
	{Name: "out-of-scope"},
	{Name: "honest-pass"},
}

type goldenExpect struct {
	Scope       []string `json:"scope"`
	Result      string   `json:"result"`
	FailedGates []string `json:"failed_gates"`
	// DetailContains maps a gate name to text its detail must contain
	// (e.g. the path a failed gate names).
	DetailContains map[string]string `json:"detail_contains"`
}

// prepareGolden clones the built repo <built>/<name> into a temp dir with
// branch task checked out, copies <root>/<name>/uncommitted/ into the
// worktree without committing it, and returns the clone with its base and
// head SHAs. A missing fixture path, or an uncommitted/ the fixture does not
// declare, is an error.
func prepareGolden(t *testing.T, root, built string, fx goldenFixture) (dir, base, head string, err error) {
	t.Helper()
	src := filepath.Join(root, fx.Name)
	repo := filepath.Join(built, fx.Name)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return "", "", "", fmt.Errorf("fixture %s: built repo: %w", fx.Name, err)
	}
	if _, err := os.Stat(filepath.Join(src, "expect.json")); err != nil {
		return "", "", "", fmt.Errorf("fixture %s: %w", fx.Name, err)
	}
	extra := filepath.Join(src, "uncommitted")
	fi, statErr := os.Stat(extra)
	switch {
	case fx.Uncommitted && statErr != nil:
		return "", "", "", fmt.Errorf("fixture %s: uncommitted/ is required: %w", fx.Name, statErr)
	case fx.Uncommitted && !fi.IsDir():
		return "", "", "", fmt.Errorf("fixture %s: uncommitted is not a directory", fx.Name)
	case !fx.Uncommitted && statErr == nil:
		return "", "", "", fmt.Errorf("fixture %s: has uncommitted/ but does not declare it", fx.Name)
	}
	dir = filepath.Join(t.TempDir(), "worker")
	if out, err := exec.Command("git", "clone", "-q", "--branch", "task", repo, dir).CombinedOutput(); err != nil {
		return "", "", "", fmt.Errorf("clone %s: %v\n%s", fx.Name, err, out)
	}
	base = git(t, dir, "rev-parse", "origin/main")
	head = git(t, dir, "rev-parse", "task")
	if fx.Uncommitted {
		if err := os.CopyFS(dir, os.DirFS(extra)); err != nil {
			return "", "", "", err
		}
		// The copy must leave the worker tree dirty, or the fixture would
		// not test uncommitted work at all.
		if st := git(t, dir, "status", "--porcelain"); st == "" {
			return "", "", "", fmt.Errorf("fixture %s: uncommitted/ left the worker tree clean", fx.Name)
		}
	}
	return dir, base, head, nil
}

func TestGoldenFixtures(t *testing.T) {
	for _, fx := range goldenFixtures {
		name := fx.Name
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(goldenDir, name, "expect.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want goldenExpect
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			dir, base, head, err := prepareGolden(t, goldenDir, goldenRepos(t), fx)
			if err != nil {
				t.Fatal(err)
			}

			v, err := Run(context.Background(), Options{
				Repo: gitx.Repo{Dir: dir}, Task: name, Base: base, Head: head, Scope: want.Scope,
				LogDir: t.TempDir(), WorkDir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Run error (infra, not verdict): %v", err)
			}
			if v.Result != want.Result || !slices.Equal(v.FailedGates, want.FailedGates) {
				t.Fatalf("got %s %v, want %s %v; gates: %+v", v.Result, v.FailedGates, want.Result, want.FailedGates, v.Gates)
			}
			for gate, text := range want.DetailContains {
				i := slices.IndexFunc(v.Gates, func(g events.GateData) bool { return g.Name == gate })
				if i < 0 {
					t.Fatalf("no gate %s in %+v", gate, v.Gates)
				}
				if !strings.Contains(v.Gates[i].Detail, text) {
					t.Fatalf("gate %s detail %q does not name %q", gate, v.Gates[i].Detail, text)
				}
			}
			if v.Unrestricted != (want.Scope == nil) {
				t.Fatalf("unrestricted = %v with scope %v", v.Unrestricted, want.Scope)
			}
		})
	}
}

// build.sh must reproduce exactly the SHAs in shas.txt (checked inside
// goldenBuild), and shas.txt must list exactly the fixtures the test uses.
func TestGoldenFixturesMatchBuildScript(t *testing.T) {
	goldenRepos(t)
	want, err := readGoldenSHAs(filepath.Join(goldenDir, "shas.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fx := range goldenFixtures {
		names = append(names, fx.Name)
	}
	listed := slices.Sorted(maps.Keys(want))
	slices.Sort(names)
	if !slices.Equal(listed, names) {
		t.Fatalf("shas.txt lists %v, test uses %v", listed, names)
	}
	// No built repo may be committed: the fixtures are text only.
	bundles, _ := filepath.Glob(filepath.Join(goldenDir, "*", "*.bundle"))
	if len(bundles) != 0 {
		t.Fatalf("binary bundles committed under testdata/golden: %v", bundles)
	}
}

// A drifted build must fail: the check has to notice one changed SHA, a
// missing fixture line, and an extra one.
func TestGoldenSHAsDetectDrift(t *testing.T) {
	built := goldenRepos(t)
	want, err := readGoldenSHAs(filepath.Join(goldenDir, "shas.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGoldenSHAs(built, want); err != nil {
		t.Fatalf("intact build: %v", err)
	}
	changed := maps.Clone(want)
	changed["honest-pass"] = [2]string{want["honest-pass"][0], strings.Repeat("0", 40)}
	missing := maps.Clone(want)
	delete(missing, "out-of-scope")
	extra := maps.Clone(want)
	extra["new-fixture"] = want["honest-pass"]
	for name, w := range map[string]map[string][2]string{"changed head": changed, "missing line": missing, "extra line": extra} {
		if err := checkGoldenSHAs(built, w); err == nil {
			t.Errorf("%s: drift not detected", name)
		}
	}
}

// A fixture with a missing path must fail, not pass for the wrong reason.
// With uncommitted/ deleted, uncommitted-only-pass used to still pass
// (cmd:test fails either way), so the test checked nothing.
func TestGoldenFixtureMissingPathFails(t *testing.T) {
	copyFixtures := func(t *testing.T) string {
		root := t.TempDir()
		if err := os.CopyFS(root, os.DirFS(goldenDir)); err != nil {
			t.Fatal(err)
		}
		return root
	}
	built := goldenRepos(t)
	cases := []struct {
		name   string
		fx     goldenFixture
		remove string // path under root to delete
	}{
		{"uncommitted dir", goldenFixture{Name: "uncommitted-only-pass", Uncommitted: true}, "uncommitted-only-pass/uncommitted"},
		{"expect.json", goldenFixture{Name: "honest-pass"}, "honest-pass/expect.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := copyFixtures(t)
			if _, _, _, err := prepareGolden(t, root, built, c.fx); err != nil {
				t.Fatalf("intact fixture: %v", err)
			}
			if err := os.RemoveAll(filepath.Join(root, c.remove)); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := prepareGolden(t, root, built, c.fx); err == nil {
				t.Fatalf("%s deleted, but the fixture still prepared", c.remove)
			}
		})
	}
	t.Run("built repo", func(t *testing.T) {
		if _, _, _, err := prepareGolden(t, goldenDir, t.TempDir(), goldenFixture{Name: "honest-pass"}); err == nil {
			t.Fatal("missing built repo was accepted")
		}
	})
	t.Run("undeclared uncommitted dir", func(t *testing.T) {
		root := copyFixtures(t)
		if err := os.CopyFS(filepath.Join(root, "honest-pass", "uncommitted"), os.DirFS(filepath.Join(root, "uncommitted-only-pass", "uncommitted"))); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := prepareGolden(t, root, built, goldenFixture{Name: "honest-pass"}); err == nil {
			t.Fatal("undeclared uncommitted/ was accepted")
		}
	})
}
