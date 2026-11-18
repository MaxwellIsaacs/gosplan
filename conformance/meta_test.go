// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// meta_test.go: GREEN NOW. The suite's own rules: every test names a clause
// that exists in clauses.md; RED.md lists only real tests with clauses they
// name; red tests name only red-kind clauses and green tests only green-kind
// ones; every clause is covered; every file carries the protected header;
// nothing calls t.Skip.
package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	clauseLine = regexp.MustCompile(`^Clause:\s*(\S+)\s*$`)
	clauseRow  = regexp.MustCompile("^\\|\\s*`([A-Za-z0-9_./-]+\\.md#[A-Z][A-Z0-9-]*|[a-z-]+#[A-Z][A-Z0-9-]*)`\\s*\\|\\s*([GR])\\s*\\|")
	redRow     = regexp.MustCompile("^\\|\\s*`?(Test[A-Za-z0-9_]+)`?\\s*\\|\\s*`?([^|`]+?)`?\\s*\\|\\s*`(.+)`\\s*\\|\\s*$")
)

// loadClauses parses clauses.md: clause id -> kind (G or R).
func loadClauses(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range strings.Split(readFile(t, filepath.Join(moduleRoot, "conformance", "clauses.md")), "\n") {
		if m := clauseRow.FindStringSubmatch(l); m != nil {
			if _, dup := out[m[1]]; dup {
				t.Fatalf("clauses.md: duplicate clause %s", m[1])
			}
			out[m[1]] = m[2]
		}
	}
	if len(out) == 0 {
		t.Fatal("clauses.md: no clause rows parsed")
	}
	return out
}

// redEntry is one RED.md row.
type redEntry struct{ clause, expect string }

// loadRED parses conformance/RED.md: test name -> row.
func loadRED(t *testing.T) map[string]redEntry {
	t.Helper()
	out := map[string]redEntry{}
	for _, l := range strings.Split(readFile(t, filepath.Join(moduleRoot, "conformance", "RED.md")), "\n") {
		if m := redRow.FindStringSubmatch(l); m != nil {
			if _, dup := out[m[1]]; dup {
				t.Fatalf("RED.md: duplicate row for %s", m[1])
			}
			if _, err := regexp.Compile(m[3]); err != nil {
				t.Fatalf("RED.md: %s: bad regexp %q: %v", m[1], m[3], err)
			}
			out[m[1]] = redEntry{clause: strings.TrimSpace(m[2]), expect: m[3]}
		}
	}
	return out
}

// suiteTests parses every *_test.go in conformance/: test name -> clauses.
func suiteTests(t *testing.T) (map[string][]string, []string) {
	t.Helper()
	dir := filepath.Join(moduleRoot, "conformance")
	files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	tests := map[string][]string{}
	var problems []string
	fset := token.NewFileSet()
	for _, f := range files {
		src := readFile(t, f)
		if !strings.HasPrefix(src, "// PROTECTED.") || !strings.Contains(src, "must not edit, weaken, skip or delete") {
			problems = append(problems, filepath.Base(f)+": missing the PROTECTED / no-weakening header")
		}
		file, err := parser.ParseFile(fset, f, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow") {
				problems = append(problems, fset.Position(sel.Pos()).String()+": t.Skip is not allowed in the suite")
			}
			return true
		})
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			var cl []string
			if fn.Doc != nil {
				for _, c := range fn.Doc.List {
					if m := clauseLine.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))); m != nil {
						cl = append(cl, m[1])
					}
				}
			}
			if len(cl) == 0 {
				problems = append(problems, fn.Name.Name+": no `// Clause: <file>#<id>` line in its doc comment")
			}
			tests[fn.Name.Name] = cl
		}
	}
	return tests, problems
}

// Clause: suite#CONF-META
func TestMetaClausesAndRedTable(t *testing.T) {
	clauses := loadClauses(t)
	red := loadRED(t)
	tests, problems := suiteTests(t)
	// GOSPLAN_CONF_META_PARTIAL=1 is for building the suite in parallel tasks:
	// it drops only the two whole-suite
	// checks (every RED.md row has a test, every clause has a test).
	// conformance/run.sh never sets it.
	partial := os.Getenv("GOSPLAN_CONF_META_PARTIAL") == "1"
	covered := map[string]bool{}
	for name, cl := range tests {
		_, isRed := red[name]
		for _, c := range cl {
			kind, ok := clauses[c]
			if !ok {
				problems = append(problems, name+": names clause "+c+" which is not in clauses.md")
				continue
			}
			covered[c] = true
			if isRed && kind != "R" {
				problems = append(problems, name+": listed in RED.md but names green clause "+c)
			}
			if !isRed && kind == "R" {
				problems = append(problems, name+": names red clause "+c+" but has no RED.md row")
			}
		}
	}
	for name, e := range red {
		cl, ok := tests[name]
		if !ok {
			if !partial {
				problems = append(problems, "RED.md: row for "+name+", which is not a test in conformance/")
			}
			continue
		}
		for _, rc := range strings.Split(e.clause, ",") {
			rc = strings.TrimSpace(rc)
			found := false
			for _, c := range cl {
				found = found || c == rc
			}
			if !found {
				problems = append(problems, "RED.md: "+name+" row names clause "+rc+", which the test does not name")
			}
		}
	}
	for c := range clauses {
		if !covered[c] && !partial {
			problems = append(problems, "clause "+c+" has no test")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("conformance meta-check failed:\n  %s", strings.Join(problems, "\n  "))
	}
}

// The fake claude and the core probe carry the protected header too.
//
// Clause: suite#CONF-META
func TestMetaProtectedSupportFiles(t *testing.T) {
	for _, rel := range []string{"fakeclaude/main.go", "redcheck/main.go", "RED.md", "README.md", "clauses.md", "run.sh"} {
		b, err := os.ReadFile(filepath.Join(moduleRoot, "conformance", rel))
		if err != nil {
			t.Fatalf("conformance/%s: %v", rel, err)
		}
		if !strings.Contains(string(b), "PROTECTED") {
			t.Fatalf("conformance/%s lacks the PROTECTED marker", rel)
		}
	}
}
