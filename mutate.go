package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// osenv proof mutate: prove the tests catch bugs. osenv plants one small bug at a time in a source file (a
// flipped comparison, and/or, true/false, +/-, a number off by one), runs the tests, and names every bug the
// tests let through. It works in a throwaway copy of the project inside .osenv/, so the real file is never
// touched and a hybrid working next door never sees a planted bug. On the Linux ladder the desk built this by
// hand for task 7, and deepseek twice found tests that could never fail.
//
//	osenv proof mutate <out.txt> <source file> [--n 10] [--allow 0] [--timeout 120s] -- <test command> [args]

type mutant struct {
	line     int
	col      int // byte offset in the line
	from, to string
	text     string // the original line
}

var mutOps = []struct{ from, to string }{
	{" is not ", " is "}, {" is ", " is not "}, {" not in ", " in "}, {" not ", " "},
	{"==", "!="}, {"!=", "=="}, {"<=", ">"}, {">=", "<"}, {" < ", " >= "}, {" > ", " <= "},
	{" and ", " or "}, {" or ", " and "}, {"&&", "||"}, {"||", "&&"},
	{"True", "False"}, {"False", "True"}, {"true", "false"}, {"false", "true"},
	{" + ", " - "}, {" - ", " + "},
}

var mutNumRe = regexp.MustCompile(`\b\d+\b`)

// mutants: at most n planted bugs, spread evenly over the file's code lines, and how many code lines there were.
// Comments (inline ones too), blank lines, docstrings and text inside quotes are skipped (Windows 0.3 run: bugs
// planted in docstring text "survived" and read as gaps).
func mutants(src string, n int) ([]mutant, int) {
	var all []mutant
	code := 0
	inDoc := false
	for i, l := range strings.Split(src, "\n") {
		t := strings.TrimSpace(l)
		if k := strings.Count(l, `"""`) + strings.Count(l, "'''"); inDoc || k > 0 {
			if k%2 == 1 {
				inDoc = !inDoc
			}
			continue // a docstring line, or one that opens or closes one
		}
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") ||
			strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "from ") {
			continue
		}
		code++
		found := false
		for _, op := range mutOps {
			if c := codeIndex(l, op.from); c >= 0 {
				all = append(all, mutant{i + 1, c, op.from, op.to, l})
				found = true
				break
			}
		}
		if !found {
			for _, loc := range mutNumRe.FindAllStringIndex(l, -1) {
				if !inQuotes(l, loc[0]) && loc[0] < commentAt(l) {
					v, _ := strconv.Atoi(l[loc[0]:loc[1]])
					all = append(all, mutant{i + 1, loc[0], l[loc[0]:loc[1]], strconv.Itoa(v + 1), l})
					break
				}
			}
		}
	}
	if len(all) <= n {
		return all, code
	}
	var out []mutant
	for k := 0; k < n; k++ {
		out = append(out, all[k*len(all)/n])
	}
	return out, code
}

func apply(src string, m mutant) string {
	lines := strings.Split(src, "\n")
	l := lines[m.line-1]
	lines[m.line-1] = l[:m.col] + m.to + l[m.col+len(m.from):]
	return strings.Join(lines, "\n")
}

// codeIndex: the first place op appears in code (outside quotes, before an inline comment), or -1.
func codeIndex(l, op string) int {
	end := commentAt(l)
	for from := 0; ; {
		i := strings.Index(l[from:], op)
		if i < 0 || from+i >= end {
			return -1
		}
		if !inQuotes(l, from+i) {
			return from + i
		}
		from += i + len(op)
	}
}

// commentAt: where an inline comment starts (# or //, outside quotes), or the line's length.
func commentAt(l string) int {
	for i := 0; i < len(l); i++ {
		if (l[i] == '#' || strings.HasPrefix(l[i:], "//")) && !inQuotes(l, i) {
			return i
		}
	}
	return len(l)
}

// inQuotes: whether byte i of the line sits inside a '...' or "..." string (a rough, one-line reading).
func inQuotes(l string, i int) bool {
	var q byte
	for j := 0; j < i; j++ {
		switch c := l[j]; {
		case c == '\\':
			j++
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case c == q:
			q = 0
		}
	}
	return q != 0
}

func proofMutateArgs(args []string) (int, error) {
	i := indexOf(args, "--")
	if i < 0 || len(args) < i+2 {
		return 2, fmt.Errorf("usage: osenv proof mutate <out.txt> <source file> [--n 10] [--allow 0] [--timeout 120s] -- <test command> [args]")
	}
	fs := flag.NewFlagSet("proof mutate", flag.ContinueOnError)
	n := fs.Int("n", 10, "how many bugs to plant")
	allow := fs.Int("allow", 0, "how many survivors the job allows (each still listed, to explain)")
	timeout := fs.Duration("timeout", 120*time.Second, "the longest one test run may take")
	pos := parseAnyOrder(fs, args[:i])
	if len(pos) != 2 {
		return 2, fmt.Errorf("usage: osenv proof mutate <out.txt> <source file> [--n 10] -- <test command>")
	}
	ok, report, err := proofMutate(".", pos[0], pos[1], args[i+1:], *n, *allow, *timeout, os.Stdout)
	if err != nil {
		return 1, err
	}
	fmt.Print(tail(report, 1) + "\n") // the numbered lines above already listed every planted bug
	if !ok {
		return 1, nil
	}
	return 0, nil
}

// proofMutate: true when every planted bug made the tests fail. The report is saved to out either way.
func proofMutate(root, out, file string, test []string, n, allow int, timeout time.Duration, progress io.Writer) (bool, string, error) {
	root, _ = filepath.Abs(root)
	rel, err := filepath.Rel(root, mustAbs(file))
	if err != nil || strings.HasPrefix(rel, "..") {
		return false, "", fmt.Errorf("%s is not inside the project (%s)", file, root)
	}
	orig, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return false, "", err
	}
	box, err := os.MkdirTemp(filepath.Join(root, ".osenv"), "mutate-")
	if err != nil {
		if err = os.MkdirAll(filepath.Join(root, ".osenv"), 0o755); err == nil {
			box, err = os.MkdirTemp(filepath.Join(root, ".osenv"), "mutate-")
		}
		if err != nil {
			return false, "", err
		}
	}
	defer os.RemoveAll(box)
	if err := copyProject(root, box); err != nil {
		return false, "", err
	}
	run := func() (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c := exec.CommandContext(ctx, test[0], test[1:]...)
		c.Dir = box
		c.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONDONTWRITEBYTECODE=1")
		b, err := c.CombinedOutput()
		if ctx.Err() != nil {
			return -1, "timed out"
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(b)
		} else if err != nil {
			return 127, err.Error()
		}
		return 0, string(b)
	}
	// the command as run, flags included, so pasting it back runs the same gate (Windows light rc8: --allow was dropped)
	argv := []string{"osenv", "proof", "mutate", out, filepath.ToSlash(rel)}
	if n != 10 {
		argv = append(argv, "--n", strconv.Itoa(n))
	}
	if allow != 0 {
		argv = append(argv, "--allow", strconv.Itoa(allow))
	}
	if timeout != 120*time.Second {
		argv = append(argv, "--timeout", timeout.String())
	}
	head := "$ " + shellLine(append(append(argv, "--"), test...)) + "\n"
	if code, o := run(); code != 0 {
		r := head + fmt.Sprintf("the tests fail before any bug is planted (exit %d), so nothing can be measured:\n%s\nresult: FAIL (the tests must pass first; osenv proof mutate, %s)\n", code, tail(o, 15), now())
		os.MkdirAll(filepath.Dir(mustAbs(out)), 0o755)
		return false, r, os.WriteFile(out, []byte(r), 0o644)
	}
	ms, codeLines := mutants(string(orig), n)
	var lines []string
	survived := 0
	target := filepath.Join(box, rel)
	for k, m := range ms {
		os.WriteFile(target, []byte(apply(string(orig), m)), 0o644)
		code, _ := run()
		verdict := "caught  "
		if code == 0 {
			verdict, survived = "SURVIVED", survived+1
		}
		lines = append(lines, fmt.Sprintf("%s line %d: %q -> %q in: %s", verdict, m.line, m.from, m.to, strings.TrimSpace(m.text)))
		fmt.Fprintf(progress, "%d/%d %s\n", k+1, len(ms), lines[len(lines)-1])
	}
	os.WriteFile(target, orig, 0o644)
	result, pass := "PASS (every planted bug made a test fail)", true
	switch {
	case len(ms) == 0:
		result, pass = "FAIL (no line to plant a bug in)", false
	case survived > allow:
		result, pass = fmt.Sprintf("FAIL (%d of %d planted bugs went unnoticed: the tests don't check those lines)", survived, len(ms)), false
	case survived > 0: // a job may allow a few, each explained (an equivalent mutant can't be caught)
		result = fmt.Sprintf("PASS (%d of %d planted bugs went unnoticed, within the %d allowed: explain each SURVIVED line)", survived, len(ms), allow)
	}
	spots := fmt.Sprintf("planted %d bugs across %d code lines (at most one per line; lines with no comparison, and/or, not, is, true/false, +/- or number have no spot)\n", len(ms), codeLines)
	r := head + spots + strings.Join(lines, "\n") + "\n" + fmt.Sprintf("result: %s (osenv proof mutate, %s)\n", result, now())
	os.MkdirAll(filepath.Dir(mustAbs(out)), 0o755)
	return pass, r, os.WriteFile(out, []byte(r), 0o644)
}

// copyProject copies the project into dst, leaving out .osenv, .git and dependency folders.
func copyProject(root, dst string) error {
	skip := map[string]bool{".osenv": true, ".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true, ".pytest_cache": true}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		info, _ := d.Info()
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode().Perm())
	})
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
