package budget

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// budgetForbiddenImports may not appear in a non-test file of this package:
// the package does no I/O and reads no clock or randomness.
var budgetForbiddenImports = map[string]bool{
	"os": true, "io": true, "net": true, "database/sql": true,
	"math/rand": true, "math/rand/v2": true, "os/exec": true, "syscall": true,
}

// budgetForbiddenTime are the clock-reading or sleeping calls of package time.
var budgetForbiddenTime = map[string]bool{
	"Now": true, "Since": true, "Until": true, "Sleep": true, "After": true,
	"Tick": true, "NewTimer": true, "NewTicker": true,
}

func budgetImportForbidden(path string) bool {
	return budgetForbiddenImports[path] || strings.HasPrefix(path, "io/") || strings.HasPrefix(path, "net/")
}

// budgetPurityViolations parses every non-test .go file in dir and returns
// one line per forbidden import or forbidden time.* selector.
func budgetPurityViolations(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	parsed := 0
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		timeNames := map[string]bool{}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if budgetImportForbidden(p) {
				out = append(out, fset.Position(im.Pos()).String()+": forbidden import "+p)
			}
			if p == "time" {
				name := "time"
				if im.Name != nil {
					name = im.Name.Name
				}
				timeNames[name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && timeNames[id.Name] && budgetForbiddenTime[sel.Sel.Name] {
				out = append(out, fset.Position(sel.Pos()).String()+": forbidden time."+sel.Sel.Name)
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatalf("no non-test .go files parsed in %s", dir)
	}
	return out
}

// TestBudgetPackageIsPure: internal/budget imports no I/O, network, SQL or
// randomness package and never reads the clock.
func TestBudgetPackageIsPure(t *testing.T) {
	for _, v := range budgetPurityViolations(t, ".") {
		t.Error(v)
	}
}

// TestBudgetPurityCheckerCatches: the checker itself flags each forbidden
// import and each forbidden time call, also under an import alias, so
// TestBudgetPackageIsPure cannot pass vacuously.
func TestBudgetPurityCheckerCatches(t *testing.T) {
	dir := t.TempDir()
	src := `package x

import (
	"io/fs"
	clock "time"
	_ "net/http"
	"os"
)

var _ fs.FS
var _ = os.Args

func f() { _ = clock.Now(); clock.Sleep(0); _ = clock.Duration(1) }
`
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\nimport \"os\"\nvar _ = os.Args\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(budgetPurityViolations(t, dir), "\n")
	for _, want := range []string{"forbidden import io/fs", "forbidden import net/http", "forbidden import os", "forbidden time.Now", "forbidden time.Sleep"} {
		if !strings.Contains(got, want) {
			t.Errorf("checker missed %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Duration") || strings.Contains(got, "x_test.go") {
		t.Errorf("checker flagged an allowed use or a test file:\n%s", got)
	}
}
