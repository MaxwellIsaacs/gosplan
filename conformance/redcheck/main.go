// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit it to make the suite pass.
//
// Command redcheck classifies a `go test -json` stream of the conformance
// package against conformance/RED.md.
//
//	go test -json ./conformance/ | redcheck --red conformance/RED.md [--green-out FILE] [--green-only]
//
// A test with a RED.md row must FAIL, its output must match the row's
// regexp, and it must not panic or time out ("red ok"); if it passes it is
// NEWLY GREEN. Every other test must PASS ("green ok"); a fail or skip is
// GREEN FAILED. A RED.md row whose test never ran, a package build failure,
// or a test binary panic is an ERROR.
//
// Exit: 0 everything as expected; 3 only newly-green tests besides that
// (a later operation turned a red test green: a human moves its RED.md row in a
// reviewed commit); 1 anything else.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type ev struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type rec struct {
	runs, fails, passes, skips int
	out                        strings.Builder
}

var redRow = regexp.MustCompile("^\\|\\s*`?(Test[A-Za-z0-9_]+)`?\\s*\\|\\s*`?([^|`]+?)`?\\s*\\|\\s*`(.+)`\\s*\\|\\s*$")

func main() {
	redPath := flag.String("red", "conformance/RED.md", "RED.md path")
	greenOut := flag.String("green-out", "", "write the names of green tests here, one per line")
	greenOnly := flag.Bool("green-only", false, "the stream holds only green tests (repeat pass): red rows are not required to run")
	flag.Parse()

	red := map[string]*regexp.Regexp{}
	b, err := os.ReadFile(*redPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "redcheck:", err)
		os.Exit(1)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if m := redRow.FindStringSubmatch(l); m != nil {
			re, err := regexp.Compile(m[3])
			if err != nil {
				fmt.Fprintf(os.Stderr, "redcheck: RED.md %s: %v\n", m[1], err)
				os.Exit(1)
			}
			red[m[1]] = re
		}
	}

	tests := map[string]*rec{}
	var pkgOut strings.Builder
	pkgFailed := false
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			pkgOut.WriteString(sc.Text() + "\n") // build errors arrive as plain text
			continue
		}
		top := e.Test
		if i := strings.IndexByte(top, '/'); i >= 0 {
			top = top[:i]
		}
		if top == "" {
			pkgOut.WriteString(e.Output)
			if e.Action == "fail" {
				pkgFailed = true
			}
			continue
		}
		r := tests[top]
		if r == nil {
			r = &rec{}
			tests[top] = r
		}
		r.out.WriteString(e.Output)
		if e.Test != top { // subtest results roll up into the parent
			continue
		}
		switch e.Action {
		case "run":
			r.runs++
		case "pass":
			r.passes++
		case "fail":
			r.fails++
		case "skip":
			r.skips++
		}
	}

	var redOK, green, newlyGreen, wrong []string
	var greenNames []string
	names := make([]string, 0, len(tests))
	for n := range tests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := tests[n]
		out := r.out.String()
		bad := strings.Contains(out, "panic:") || strings.Contains(out, "test timed out")
		if re, isRed := red[n]; isRed {
			switch {
			case r.fails == 0 && r.passes > 0 && r.skips == 0:
				newlyGreen = append(newlyGreen, n)
			case r.fails > 0 && !bad && re.MatchString(out):
				redOK = append(redOK, n)
			default:
				wrong = append(wrong, fmt.Sprintf("WRONG RED %s: want a failure matching %q (no panic/timeout)\n%s", n, re.String(), indent(out)))
			}
			continue
		}
		greenNames = append(greenNames, n)
		if r.fails == 0 && r.skips == 0 && r.passes > 0 && r.passes == r.runs {
			green = append(green, n)
		} else {
			wrong = append(wrong, fmt.Sprintf("GREEN FAILED %s (runs %d, passes %d, fails %d, skips %d)\n%s", n, r.runs, r.passes, r.fails, r.skips, indent(out)))
		}
	}
	if !*greenOnly {
		for n := range red {
			if tests[n] == nil {
				wrong = append(wrong, "ERROR RED.md row "+n+": the test did not run")
			}
		}
	}
	if pkgFailed && len(tests) == 0 || strings.Contains(pkgOut.String(), "[build failed]") || strings.Contains(pkgOut.String(), "panic:") {
		wrong = append(wrong, "ERROR package level failure:\n"+indent(pkgOut.String()))
	}
	if *greenOut != "" {
		_ = os.WriteFile(*greenOut, []byte(strings.Join(greenNames, "\n")+"\n"), 0o644)
	}

	sort.Strings(wrong)
	for _, w := range wrong {
		fmt.Println(w)
	}
	for _, n := range newlyGreen {
		fmt.Println("NEWLY GREEN", n, "(passes now; a human moves its RED.md row in a reviewed commit)")
	}
	if *greenOnly {
		fmt.Printf("conformance (green repeat): green %d/%d ok, unexpected %d\n", len(green), len(greenNames), len(wrong))
	} else {
		fmt.Printf("conformance: green %d/%d ok, red %d/%d failing as RED.md expects, newly green %d, unexpected %d\n",
			len(green), len(greenNames), len(redOK), len(red), len(newlyGreen), len(wrong))
	}
	switch {
	case len(wrong) > 0:
		os.Exit(1)
	case len(newlyGreen) > 0:
		os.Exit(3)
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 60 {
		lines = append(lines[:30], append([]string{"    ..."}, lines[len(lines)-30:]...)...)
	}
	return "    " + strings.Join(lines, "\n    ")
}
