package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// TestMain lets this test binary act as a fake engine (sleep, or touch a marker), so the engine tests run
// on Windows as well as Linux.
func TestMain(m *testing.M) {
	switch os.Getenv("OSENV_FAKE") {
	case "sleep":
		time.Sleep(2 * time.Second)
		os.Exit(0)
	case "sleep1":
		time.Sleep(time.Second)
		os.Exit(0)
	case "touch":
		os.WriteFile(os.Getenv("OSENV_FAKE_MARKER"), nil, 0o644)
		os.Exit(0)
	case "utf16": // what PowerShell's > produces: UTF-16LE with a BOM
		os.Stdout.Write([]byte{0xff, 0xfe, 'h', 0, 0xe9, 0, 'l', 0, 'l', 0, 'o', 0, '\n', 0})
		os.Exit(3)
	case "mutcheck": // a test suite that only checks add(): it fails when add subtracts
		b, _ := os.ReadFile("lib.py")
		if strings.Contains(string(b), "a - b") {
			os.Exit(1)
		}
		os.Exit(0)
	case "breakfile": // a bad run: corrupts one file, creates one, deletes one, and edits a shared one
		os.WriteFile("app.py", []byte("garbage\x00\xff"), 0o644)
		os.WriteFile("scratch.txt", []byte("left behind"), 0o644)
		os.MkdirAll(filepath.Join("tmpdir", "deep"), 0o755)
		os.WriteFile(filepath.Join("tmpdir", "deep", "x.txt"), []byte("x"), 0o644)
		os.Remove("config.ini")
		os.WriteFile("shared.md", []byte("the run's edit"), 0o644)
		os.Exit(0)
	case "orphan": // an engine that starts a server-like child, which outlives it and keeps writing
		c := exec.Command(os.Args[0], "-test.run=none")
		c.Env = append(os.Environ(), "OSENV_FAKE=lateprint")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		c.Start()
		fmt.Println("ENGINE-SAID-HI")
		os.Exit(0)
	case "lateprint":
		time.Sleep(3 * time.Second)
		fmt.Println("LATE-OUTPUT")
		os.Exit(0)
	case "args": // saves the prompt it was run with, so a test can read a real brief
		os.WriteFile(os.Getenv("OSENV_FAKE_MARKER"), []byte(strings.Join(os.Args[1:], "\n")), 0o644)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The loop's invariants, over thousands of simulated picks with a stubbed Jev (no tokens).
func TestLoopInvariants(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		r := simLoop(3000, seed)
		if r.Violations > 0 {
			t.Fatalf("seed %d: %d violations: %v", seed, r.Violations, r.Examples)
		}
		if r.ByEngine["opus"] == 0 || r.ByEngine["qwen"] == 0 || r.ByEngine["deepseek"] == 0 {
			t.Fatalf("seed %d: a remedy never ran: %v", seed, r.ByEngine)
		}
	}
}

func TestMarkers(t *testing.T) {
	n := "# hybrid-x NOTES\nFEEDBACK:\n- (qwen) then park WAITING: desk review\n\n## STATE\n- ESCALATE: visual review of pass 2\n* ESCALATE: code review of herds.gd - ANSWERED\n- job\n\n## LOG\n- ESCALATE: old one in the log\n"
	if a := asks(n); len(a) != 2 || !strings.Contains(a[0], "visual review") {
		t.Fatalf("asks: %q", a) // the LOG line counts too: it starts with ESCALATE:, and only ANSWERED/RESOLVED close an ask
	}
	if waiting(n) {
		t.Fatal("a quoted 'WAITING: desk review' inside feedback must not park the seat")
	}
	if !waiting("## STATE\n- WAITING: desk review\n") || waiting("## STATE\n- WAITING: desk review\n- ESCALATE: code review of x\n") {
		t.Fatal("WAITING parks only when no ask is open")
	}
	if !strings.HasPrefix(strings.TrimSpace(orders(n)), "ESCALATE: visual review") {
		t.Fatalf("orders must put the asks first: %q", orders(n))
	}
	m := sectionAfterState(n, "## FEEDBACK (qwen)\n- x")
	if strings.Index(m, "## FEEDBACK (qwen)") < strings.Index(m, "## STATE") {
		t.Fatal("feedback must land after STATE, never above it")
	}
}

func TestReadOnly(t *testing.T) {
	for _, c := range []string{"ls -la", "grep -n x a.go | head", "cd x && sed -n 1,9p b", "pwd; ls /p/osenv 2>&1; realpath pages/c.png", "sha256sum a b | sort", "osenv proof check .osenv/tasks/x/out", "osenv proof --help 2>&1", `/p/osenv proof --help 2>&1; echo "==="; /p/osenv proof run --help 2>&1 | head -n 40`, "which chromium google-chrome; python3 --version; node -V", `curl -s http://127.0.0.1:8811/v1 -d '{"do":"say","task":"x","text":"a > b"}'`,
		// video bench, Windows: leftover-process checks were kicked back as creep
		`Get-CimInstance Win32_Process -Filter "Name='python.exe'" | Where-Object {$_.CommandLine -like '*stage_app*'} | Measure-Object`,
		`wmic process where "name='python.exe'" get ProcessId,CommandLine /format:list 2>&1 | Out-String`, "tasklist /FI \"IMAGENAME eq ffmpeg.exe\"", "Get-Process ffmpeg", "ps aux | grep ffmpeg", "pgrep -a ffmpeg",
		// Windows light run: a printing loop and port checks were scored, and a chain of reads was kicked back
		`Get-CimInstance Win32_Process -Filter 'Name=''python.exe''' | ForEach-Object { Write-Output ($_.ProcessId.ToString() + ' :: ' + $_.CommandLine) }`,
		`Get-ChildItem tmp -Force; osenv --help 2>&1 | Select-Object -First 20; Get-NetTCPConnection -LocalPort 8871 -State Listen -ErrorAction SilentlyContinue`,
		"netstat -ano | findstr 8871", "ss -ltnp | grep 8834", "lsof -i :8834", "Get-Process python | % { $_.Id }",
		`osenv do say task=window text="gates 1-2 green; working on the screenshots"`} {
		if !readOnly(c) {
			t.Errorf("should be read-only: %s", c)
		}
	}
	for _, c := range []string{"echo x > f", "sed -i s/a/b/ f", "find . -delete", "rm -rf x", "cat <<E > f", "ls $(rm x)", "realpath a > f", "osenv proof run out/t.txt -- rm -rf x", "osenv proof shot out/p.png p.html", "curl -s http://127.0.0.1:8811/v1 -o f",
		`wmic process call create "calc.exe"`, `wmic process where "name='ffmpeg.exe'" delete`, "Get-Process ffmpeg | Stop-Process",
		"wmic process get name & wmic process where name='ffmpeg.exe' delete", "wmic process get name & wmic process where name='x' call terminate", "wmic process get name & wmic environment create name=X,variablevalue=1",
		"wmic process get name & wmic process where name='x' set priority=64", "wmic /output:procs.txt process get name", "wmic /append:procs.txt process get name",
		"Get-Process python | ForEach-Object { Stop-Process -Id $_.Id }", "Get-ChildItem *.tmp | ForEach-Object { Remove-Item $_ }", "ls | % { Set-Content $_ x }", "Get-Process | ForEach-Object", "Get-Process python | % { $_.Kill() }", "$p = Start-Process python -PassThru",
		`osenv do say task=window text="STEP DONE. Gates green."`, `/p/osenv do say task=window text=@.osenv/tasks/window/stepdone.txt STEP DONE`} {
		if readOnly(c) && !strings.Contains(c, "-o f") {
			t.Errorf("should be a write: %s", c)
		}
	}
}

// The shortlist only narrows; with a short list everything goes to Jev.
func TestShortlistNarrowsOnly(t *testing.T) {
	l := &Learn{}
	for i, x := range simLessons {
		l.items = append(l.items, &Item{ID: "L" + x.ID, Kind: "lesson", Scope: "global", Status: "active", Text: x.Text, Detect: x.Detect, Tags: tagsOf(x.Text + x.Detect)})
		_ = i
	}
	if got := l.shortlist("anything", nil, "t", "lesson", 24); len(got) != len(simLessons) {
		t.Fatalf("a list shorter than the cap must go to Jev whole: %d", len(got))
	}
	got := l.shortlist("write a probe script under /tmp", tagsOf("/tmp"), "t", "lesson", 3)
	ids := []string{}
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	if !contains(ids, "Ltmp") {
		t.Fatalf("the shortlist dropped the obvious lesson: %v", ids)
	}
}

// task.cancel retires a task without a verdict and keeps its record, marked cancelled.
func TestCancel(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("abandon me", "# job\nwork"); err != nil {
		t.Fatal(err)
	}
	r, err := s.taskCancel("abandon-me", "restarted with a new job")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.tasks["abandon-me"]; ok {
		t.Fatal("a cancelled task must leave the board")
	}
	rec := r["record"].(string)
	if b, err := os.ReadFile(filepath.Join(rec, "CANCELLED.md")); err != nil || !strings.Contains(string(b), "restarted") {
		t.Fatalf("the record must say it was cancelled and why: %v", err)
	}
	var c Config
	if readJSON(filepath.Join(dir, ".osenv", "config.json"), &c) != nil || !strings.Contains(c.Help["opus_per_day"], "NO CAP") {
		t.Fatal("the config file must explain opus_per_day in words")
	}
}

// Windows: PowerShell reads aren't scored, the desk-only guard catches the key=value form, and a
// payload piped in by hand with a BOM or as UTF-16 (PowerShell 5.1's >) still parses.
func TestWindowsBits(t *testing.T) {
	for _, c := range []string{"Get-Content a.py | Select-Object -First 20", "gci -Recurse out 2>$null", "Select-String -Path *.py -Pattern def"} {
		if !readOnly(c) {
			t.Errorf("should be read-only: %s", c)
		}
	}
	if isWrite("bash_input", map[string]any{"session_id": 5.0, "terminate": true}) {
		t.Error("closing a shell session is not a write")
	}
	if isWrite("read_file", map[string]any{"file_path": "a.py"}) || !isWrite("write_file", map[string]any{"file_path": "a.py"}) {
		t.Error("a read tool with a file path is a read; a write tool is a write")
	}
	for c, want := range map[string]bool{`cd /p && '/p/osenv' do say task=x text="a && b"`: true, "osenv do say task=x text=hi; rm -rf out": false,
		"/h/osenv view --help 2>&1; echo ---; /h/osenv --help 2>&1 | head -40": true, `& 'C:\osenv-0.2\osenv.exe' view a.png`: true} {
		if readOnly(c) != want {
			t.Errorf("readOnly(%s) should be %v", c, want)
		}
	}
	if readOnly("Get-Content a | Out-File b") || readOnly("python -m pytest > log.txt") {
		t.Error("a PowerShell write must be scored")
	}
	for c, want := range map[string]bool{"C:/osenv/osenv.exe do task.verdict task=x pass=true": true, `curl -s u/v1 -d '{"do":"task.cancel"}'`: true, "C:/osenv/osenv.exe do say task=x text=done": false} {
		if got := deskVerbRe.MatchString(c); got != want {
			t.Errorf("desk verb in %q: got %v", c, got)
		}
	}
	js := `{"tool_name":"Bash"}`
	u16 := []byte{0xff, 0xfe}
	for _, r := range js {
		u16 = append(u16, byte(r), 0)
	}
	for _, b := range [][]byte{[]byte("\xef\xbb\xbf" + js), u16, []byte(js)} {
		if string(plainText(b)) != js {
			t.Errorf("plainText(% x...) = %q", b[:4], plainText(b))
		}
	}
}

// The tripwire: a run long enough to have made tool calls, with no hook call reaching osenv,
// blocks the task instead of letting it run unscored (VM test, 0.1: a whole night was).
func TestTripwire(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSENV_FAKE", "sleep1")
	s.st.Cfg.Muse.Cmd = os.Args[0]
	oldAsk, oldGrace := ask, hookGrace
	defer func() { ask, hookGrace = oldAsk, oldGrace }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	hookGrace = 300 * time.Millisecond
	for _, hooked := range []bool{false, true} {
		name := fmt.Sprintf("trip-%v", hooked)
		if _, err := s.taskNew(name, "# job\nwork"); err != nil {
			t.Fatal(err)
		}
		if hooked {
			go func() { time.Sleep(300 * time.Millisecond); s.hook(HookIn{Task: name, Event: "post"}) }()
		}
		s.run(name)
		if st := s.tasks[name].State; (st == "blocked") == hooked {
			t.Fatalf("hook seen %v: state %s", hooked, st)
		}
	}
}

// learn.edit fixes how Jev filed an item (VM test: 3 lessons filed as routes) and keeps its record.
func TestLearnEdit(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.learn.items = append(s.learn.items, &Item{ID: "R1", Kind: "route", To: "deepseek", Scope: "task:x", Severity: "kick", Escapes: 2, Status: "active"})
	edit := func(js string) (any, error) { return verbs["learn.edit"].run(s, json.RawMessage(js)) }
	if _, err := edit(`{"id":"R1","kind":"lesson","scope":"global","severity":"nudge"}`); err != nil {
		t.Fatal(err)
	}
	if it := s.learn.get("R1"); it.Kind != "lesson" || it.To != "" || it.Scope != "global" || it.Escapes != 2 {
		t.Fatalf("edit went wrong: %+v", it)
	}
	if _, err := edit(`{"id":"R1","escapes":0}`); err != nil || s.learn.get("R1").Escapes != 0 {
		t.Fatalf("escapes must be correctable: %v", err)
	}
	s.learn.get("R1").Catches = 3 // VM 0.2.3 #10: a false catch must be correctable
	if _, err := edit(`{"id":"R1","catches":2}`); err != nil || s.learn.get("R1").Catches != 2 {
		t.Fatalf("catches must be correctable: %v", err)
	}
	for _, bad := range []string{`{"id":"R1","kind":"rule"}`, `{"id":"R1","kind":"route"}`, `{"id":"R1","scope":"everywhere"}`, `{"id":"nope"}`, `{"id":"R1","escapes":-1}`, `{"id":"R1","catches":-1}`} {
		if _, err := edit(bad); err == nil {
			t.Errorf("should be refused: %s", bad)
		}
	}
}

// Verdicts: "risky" needs Jev to lean toward failure, not just be unsure, and the seat's own
// folder is exempt with Windows paths too (VM run 0.2: 4 of 4 risky verdicts were correct work).
func TestRiskyVerdict(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.st.Cfg.Kickback = false
	if _, err := s.taskNew("debug", "# debug\nfix it"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var succeeds float64
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": succeeds}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	verdict := func(path string) string {
		s.hook(HookIn{Task: "debug", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file", "tool_input": map[string]any{"file_path": path}}})
		var last map[string]any
		for _, l := range readLines(s.st.path("acts.jsonl"), 1) {
			json.Unmarshal([]byte(l), &last)
		}
		return last["verdict"].(string)
	}
	for _, c := range []struct {
		p    float64
		path string
		want string
	}{{0.47, `C:\p\api\tests\test_new.py`, "allow"}, {0.2, `C:\p\api\app.py`, "risky"}, {0.2, `C:\p\.osenv\tasks\debug\out\probe2.py`, "allow"}} {
		succeeds = c.p
		if got := verdict(c.path); got != c.want {
			t.Errorf("succeeds %.2f on %s: got %s, want %s", c.p, c.path, got, c.want)
		}
	}
	s.st.Cfg.Kickback = true // video bench #6: even with kickbacks on, "risky" is a note and the action runs
	s.kicks = map[string]time.Time{}
	succeeds = 0.2
	r := fmt.Sprint(s.hook(HookIn{Task: "debug", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file", "tool_input": map[string]any{"file_path": `C:\p\api\smoke.json`}}}))
	if strings.Contains(r, "deny") || !strings.Contains(r, "this may not work") {
		t.Fatalf("risky must be a note, not a kickback: %s", r)
	}
}

// A hand-fed --test call gets the real answer but leaves the record alone (VM issue #32).
func TestHandFedHook(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("slug", "# slug\nsave the test output to a file"); err != nil {
		t.Fatal(err)
	}
	s.learn.items = append(s.learn.items, &Item{ID: "L3", Kind: "lesson", Scope: "global", Severity: "kick", Status: "active",
		Text: "save output through cmd /c", Detect: "The action saves command output with a PowerShell redirect."})
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "l_L3": 0.9}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	call := func(test bool) map[string]any {
		return s.hook(HookIn{Task: "slug", Event: "pre", Role: "muse", Test: test,
			Payload: map[string]any{"tool_name": "powershell", "tool_input": map[string]any{"command": "python -m pytest slug > out.txt"}}})
	}
	if r := call(true); r == nil || !strings.Contains(fmt.Sprint(r), "KNOWN MISTAKE") {
		t.Fatalf("a hand-fed bad action must still be denied: %v", r)
	}
	if it := s.learn.get("L3"); it.Catches != 0 || s.hookN["slug"] != 0 || len(s.recent("slug", 6)) != 0 {
		t.Fatalf("a test call must leave the record alone: catches %d, hooks %d, recent %v", it.Catches, s.hookN["slug"], s.recent("slug", 6))
	}
	call(false)
	if it := s.learn.get("L3"); it.Catches != 1 || s.hookN["slug"] != 1 {
		t.Fatalf("a real call counts: catches %d, hooks %d", it.Catches, s.hookN["slug"])
	}
}

// view takes its flags on either side of the file (Linux run BUG-2: "view <file> --q ..." failed).
func TestViewArgsAnyOrder(t *testing.T) {
	for _, args := range [][]string{{"a.png", "--crop", "0,0,.5,.5", "--q", "the legend"}, {"--q", "the legend", "a.png", "--crop", "0,0,.5,.5"}} {
		fs := flag.NewFlagSet("view", flag.ContinueOnError)
		q, crop := fs.String("q", "", ""), fs.String("crop", "", "")
		pos := parseAnyOrder(fs, args)
		if len(pos) != 1 || pos[0] != "a.png" || *q != "the legend" || *crop != "0,0,.5,.5" {
			t.Errorf("%v: got file %v, q %q, crop %q", args, pos, *q, *crop)
		}
	}
}

// Two copies of one finding in the same review are one occurrence, not an escape (Linux run).
func TestSameReviewDuplicate(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	target := "" // only the lesson this test creates counts as the same mistake (the store starts with core routes)
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := 0.0
			if k == "learnable" || k == "general" || (target != "" && k == "same_"+target) {
				p = 0.9
			}
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	in := AddIn{Task: "t", Text: "Assert exact values in tests.", Detect: "The action writes a test with a tautological assert.", Source: "deepseek"}
	seen := map[string]bool{}
	in.Seen = seen
	first, how, err := s.learn.Add(in)
	if err != nil || how != "added" {
		t.Fatalf("first copy: %v %s", err, how)
	}
	seen[first.ID], target = true, first.ID
	if it, how, _ := s.learn.Add(in); how != "merged" || it.Escapes != 0 {
		t.Fatalf("a duplicate in the same review must merge without an escape: %s, escapes %d", how, it.Escapes)
	}
	in.Seen = nil
	if it, _, _ := s.learn.Add(in); it.Escapes != 1 {
		t.Fatalf("a later repeat is an escape: escapes %d", it.Escapes)
	}
}

// The plain check reads deepseek's verdict line and fails open when there is nothing to check.
func TestPlainCheck(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("dash", "# dash\na page with 3 charts"); err != nil {
		t.Fatal(err)
	}
	reply := "1) FAIL: dash.png stops mid-histogram\nVERDICT: FAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, reply)
	}))
	defer srv.Close()
	s.st.Cfg.Plan.URL = srv.URL
	t.Setenv("OSENV_PLAN_KEY", "test-key")
	if v, _ := s.plainCheck("dash"); v != "" {
		t.Fatalf("no screenshots means no check, got %q", v)
	}
	img := image.NewRGBA(image.Rect(0, 0, 40, 90))
	fh, _ := os.Create(s.tdir("dash", "out", "dash.png"))
	png.Encode(fh, img)
	fh.Close()
	if v, txt := s.plainCheck("dash"); v != "fail" || !strings.Contains(txt, "mid-histogram") {
		t.Fatalf("got %q %q", v, txt)
	}
	reply = "all fine\nVERDICT: PASS"
	if v, _ := s.plainCheck("dash"); v != "pass" {
		t.Fatalf("got %q", v)
	}
	reply = "" // a reasoning model that spent its whole budget thinking (Linux 0.3 run): no verdict, and it says so
	if v, txt := s.plainCheck("dash"); v != "" || !strings.Contains(txt, "no verdict: an empty answer") {
		t.Fatalf("an empty answer must be reported, got %q %q", v, txt)
	}
}

// VM 0.2.3 #7: the screenshot is a deliverable in the project, not in out/. Images the task names count;
// a parallel task's new screenshot never does.
func TestTaskImages(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("ladder", "# ladder\nbuild pages/ladder.html and screenshot it to pages/ladder-1024.png"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(s.st.Root, "pages"), 0o755)
	for _, f := range []string{"pages/ladder-1024.png", "pages/other-task.png"} {
		fh, _ := os.Create(filepath.Join(s.st.Root, f))
		png.Encode(fh, image.NewRGBA(image.Rect(0, 0, 4, 4)))
		fh.Close()
	}
	got := s.taskImages("ladder", 4)
	if len(got) != 1 || filepath.Base(got[0]) != "ladder-1024.png" {
		t.Fatalf("want only the named project screenshot, got %v", got)
	}
	old := filepath.Join(s.st.Root, "pages", "ladder-1024.png") // made before the task, still named by it
	os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	if got := s.taskImages("ladder", 4); len(got) != 1 {
		t.Fatalf("with nothing newer, an older named screenshot counts (a review of existing shots): %v", got)
	}
	fh, _ := os.Create(s.tdir("ladder", "out", "new-1024.png")) // the task made one: the old named one drops out (Linux I-13)
	png.Encode(fh, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	fh.Close()
	if got := s.taskImages("ladder", 4); len(got) != 1 || filepath.Base(got[0]) != "new-1024.png" {
		t.Fatalf("screenshots made during the task come first, and stale named ones stay off: %v", got)
	}
	os.Remove(s.tdir("ladder", "out", "new-1024.png"))
	os.Chtimes(old, time.Now(), time.Now())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"VERDICT: PASS"}}]}`)
	}))
	defer srv.Close()
	s.st.Cfg.Plan.URL = srv.URL
	t.Setenv("OSENV_PLAN_KEY", "test-key")
	if v, _ := s.plainCheck("ladder"); v != "pass" {
		t.Fatalf("the plain check must run on the project screenshot, got %q", v)
	}
}

// A plain-only visual review: the plain check passes, Jev is sure nothing more is needed, and qwen
// never runs; the pass counts as the visual review. When Jev isn't sure, qwen runs as before.
func TestPlainOnlySkipsQwen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"1) PASS: complete\nVERDICT: PASS"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("OSENV_PLAN_KEY", "test-key")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	for _, c := range []struct {
		plain    float64
		job      string
		qwenRuns bool
	}{{0.9, "# dash\nbuild the page; screenshot it", false}, {0.4, "# dash\nbuild the page; screenshot it", true},
		{0.9, "# dash\nbuild the page; qwen passes it visually", true}} {
		dir := t.TempDir()
		s, err := newServer(dir)
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "qwen-ran")
		t.Setenv("OSENV_FAKE", "touch")
		t.Setenv("OSENV_FAKE_MARKER", marker)
		s.st.Cfg.Plan.URL, s.st.Cfg.Plan.Cmd = srv.URL, os.Args[0]
		plain := c.plain
		ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
			out := map[string]Ans{}
			for k := range qs {
				p := map[string]float64{"R-visual": 0.9, "plain": plain}[k]
				out[k] = Ans{Noul: &p}
			}
			return out, nil
		}
		if _, err := s.taskNew("dash", c.job); err != nil {
			t.Fatal(err)
		}
		s.setNotes("dash", underState(s.notes("dash"), "- ESCALATE: visual review of the dashboard screenshots"))
		fh, _ := os.Create(s.tdir("dash", "out", "dash.png"))
		png.Encode(fh, image.NewRGBA(image.Rect(0, 0, 20, 40)))
		fh.Close()
		s.run("dash")
		_, err = os.Stat(marker)
		if ran := err == nil; ran != c.qwenRuns {
			t.Fatalf("plain %.1f, job %q: qwen ran %v, want %v", c.plain, c.job, ran, c.qwenRuns)
		}
		// Windows light run: a PASS before qwen left no event of its own; every plain check posts one, with its time
		if ev := s.board.Since(0, "dash", []string{"feedback"}, 5); len(ev) == 0 || !strings.Contains(fmt.Sprint(ev), "plain check PASSED in ") {
			t.Fatalf("plain %.1f: the plain check's own event: %+v", c.plain, ev)
		}
		if tk := s.tasks["dash"]; !c.qwenRuns && (tk.Engines["plain"] != 1 || !strings.Contains(s.notes("dash"), "this was the visual review")) {
			t.Fatalf("a plain-only pass must count as the visual review: %+v", tk.Engines)
		}
		if tk := s.tasks["dash"]; !c.qwenRuns {
			if _, why, _, _ := s.pick(tk, s.notes("dash")); why != "back to muse after the deepseek plain check" { // VM 0.2.3 #16
				t.Fatalf("handback reason: %q", why)
			}
		}
	}
}

// Owner rules: a hard block from day one, always checked first, never swept as noise, and only the
// desk can make one (the fake shadows: a rule written once stops every hybrid in the act).
func TestOwnerRules(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ { // busy lessons with long records must not crowd the owner rule out
		s.learn.items = append(s.learn.items, &Item{ID: fmt.Sprintf("L%d", 100+i), Kind: "lesson", Scope: "global", Severity: "nudge", Status: "active", Catches: 9, Detect: "x"})
	}
	o, how, err := s.learn.Add(AddIn{Source: "owner", Text: "Never fake shadows with blobs, discs or decals.", Detect: "The action adds a fake shadow under an object."})
	if err != nil || how != "added" || o.Severity != "kick" || o.Scope != "global" {
		t.Fatalf("owner rule: %v %s %+v", err, how, o)
	}
	if set := s.learn.checkSet("t", nil, lessonsPerCheck, ""); len(set) == 0 || set[0].ID != o.ID {
		t.Fatalf("the owner rule must be checked first")
	}
	o.Surfaced = 50
	if gone := s.learn.sweep(); contains(gone, o.ID) {
		t.Fatal("an owner rule that is never broken is working, not noise")
	}
	if _, err := s.taskNew("art", "# art\nmake the rock look better"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "l_" + o.ID: 0.92}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	shadow := map[string]any{"tool_name": "write_file", "tool_input": map[string]any{"file_path": "rock_shadow.gd"}}
	if r := s.hook(HookIn{Task: "art", Event: "pre", Role: "muse", Payload: shadow}); !strings.Contains(fmt.Sprint(r), "KNOWN MISTAKE") {
		t.Fatalf("an action breaking an owner rule must be sent back: %v", r)
	}
	ok := map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": `osenv do learn.add source=opus text="respect the owner rule" detect="y"`}}
	if r := s.hook(HookIn{Task: "art", Event: "pre", Role: "opus", Payload: ok}); strings.Contains(fmt.Sprint(r), "desk's call") {
		t.Fatalf("a correction that only mentions the owner must pass: %v", r)
	}
	fake := map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": `osenv do learn.add source=owner text="x" detect="y"`}}
	if r := s.hook(HookIn{Task: "art", Event: "pre", Role: "muse", Payload: fake}); !strings.Contains(fmt.Sprint(r), "desk's call") {
		t.Fatalf("a hybrid must not make owner rules: %v", r)
	}
}

// Build only what was asked: Jev sees what a write adds, and an unasked addition goes back with how to ask.
func TestUnasked(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.st.Cfg.Kickback = true
	if _, err := s.taskNew("rock", "# rock\nmake the rock look better"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var unasked float64
	var seen string
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		seen, _ = state["action"].(string)
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "unasked": unasked}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	write := func(path string) map[string]any {
		s.kicks = map[string]time.Time{} // each case gets a fresh kickback window
		return s.hook(HookIn{Task: "rock", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
			"tool_input": map[string]any{"file_path": path, "content": "extends Decal # dark blob under the rock"}}})
	}
	unasked = 0.9
	if r := write("assets/rock_shadow.gd"); !strings.Contains(fmt.Sprint(r), "NOT ASKED FOR") || !strings.Contains(seen, "dark blob") {
		t.Fatalf("an unasked addition must go back, and Jev must see what it writes: %v / %q", r, seen)
	}
	if a := s.acts("rock", 1); len(a) == 0 || !strings.Contains(fmt.Sprint(a[0]["judged"]), "dark blob") { // Linux 0.2.3: the desk audits what was judged
		t.Fatalf("acts must keep the content Jev judged: %v", a)
	}
	if r := write(s.st.Root + "/.osenv/tasks/rock/out/probe.gd"); r != nil {
		t.Fatalf("a probe in the hybrid's own folder isn't a product addition: %v", r)
	}
	unasked = 0.5
	if r := write("assets/rock_material.gd"); r != nil {
		t.Fatalf("an unsure score must not send work back: %v", r)
	}
	// VM 0.2.3 #12: Jev is told the tool and the shell. #5: a kick lesson leads the reason, never "the next one goes through".
	var st map[string]any
	s.learn.items = append(s.learn.items, &Item{ID: "L1", Kind: "lesson", Scope: "global", Severity: "kick", Status: "active", Text: "no blob shadows", Detect: "adds a blob shadow"})
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		st = state
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "unasked": 0.9, "l_L1": 0.9}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	r := fmt.Sprint(write("assets/rock_shadow2.gd"))
	if st["tool"] != "write_file" || st["platform"] != platformName() {
		t.Fatalf("the ask must carry the tool and the platform: %v", st)
	}
	if !strings.Contains(r, "KNOWN MISTAKE") || strings.Contains(r, "next one goes through") {
		t.Fatalf("a known mistake must lead and promise no free retry: %s", r)
	}
	// Windows 0.3 run: 0.79 on the exact mistake passed silently. Near the bar, it comes back as a note, not a block.
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "l_L1": 0.75}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	if r := fmt.Sprint(write("assets/rock_shadow3.gd")); !strings.Contains(r, "CHECK THIS AGAINST A KNOWN MISTAKE: no blob shadows") || strings.Contains(r, "deny") {
		t.Fatalf("a near-bar lesson is a note, never a block: %s", r)
	}
	if a := s.acts("rock", 1); len(a) == 0 || !strings.Contains(fmt.Sprint(a[0]["note"]), "CHECK THIS AGAINST A KNOWN MISTAKE") {
		t.Fatalf("acts keep the note the engine was given (Linux I-9): %v", a)
	}
	// Linux I-12: AUDIT FIRST can't apply to a file that doesn't exist yet
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "guess": 0.72}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	if r := fmt.Sprint(write("cli/new_tool.py")); strings.Contains(r, "AUDIT FIRST") {
		t.Fatalf("a brand-new file is never sent back to be audited: %s", r)
	}
	os.WriteFile(filepath.Join(s.st.Root, "existing.py"), []byte("x = 1\n"), 0o644)
	if r := fmt.Sprint(write("existing.py")); !strings.Contains(r, "AUDIT FIRST") {
		t.Fatalf("an existing file still gets the audit check: %s", r)
	}
	// video bench, Windows: muse's write_file names its file in "path"
	museWrite := func(path string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "rock", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
			"tool_input": map[string]any{"path": filepath.Join(s.st.Root, path), "content": "import os\n"}}}))
	}
	if r := museWrite("videoedit/ffrun.py"); strings.Contains(r, "AUDIT FIRST") {
		t.Fatalf("muse's first write of a brand-new file is never sent back to be audited: %s", r)
	}
	if r := museWrite("existing.py"); !strings.Contains(r, "AUDIT FIRST") {
		t.Fatalf("muse rewriting an existing file still gets the audit check: %s", r)
	}
}

// A shipped file that points into the task folder holds the park (Linux run, task 6).
func TestParkHeldForTaskDirRefs(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("refactor", "# refactor\nsplit the game into modules"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	wrote := func(rel string) { // the task's own write, as its hook recorded it
		s.hook(HookIn{Task: "refactor", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file", "tool_input": map[string]any{"file_path": rel, "content": "x"}}})
	}
	os.MkdirAll(filepath.Join(s.st.Root, "tests"), 0o755)
	for _, c := range []struct{ file, body, where string }{
		{"test_refactor.py", "\"\"\"Checks the refactor.\"\"\"\n  BASE = '.osenv/tasks/refactor/baseline/tetris.py'\nx = 1\n", "tests/test_refactor.py:2: BASE = '.osenv/tasks/refactor/baseline/tetris.py'"},
		{"test_win.py", `BASE = r".osenv\tasks\refactor\baseline"`, `tests/test_win.py:1: BASE = r".osenv\tasks\refactor\baseline"`},
		// Windows video bench: a path built from parts slipped past, and every later pytest run recreated the retired task's folder
		{"test_gui.py", "import os\nOUTDIR = os.path.join(REPO, \".osenv\", \"tasks\", \"refactor\", \"out\")\n", `tests/test_gui.py:2: OUTDIR = os.path.join(REPO, ".osenv", "tasks", "refactor", "out")`},
		{"test_lib.py", "BASE = Path('.osenv') / 'tasks' / 'refactor' / 'baseline'\n", `tests/test_lib.py:1: BASE = Path('.osenv') / 'tasks' / 'refactor' / 'baseline'`},
		{"test_esc.py", `BASE = ".osenv\\tasks\\refactor"` + "\n", `tests/test_esc.py:1: BASE = ".osenv\\tasks\\refactor"`},
	} {
		os.WriteFile(filepath.Join(s.st.Root, "tests", c.file), []byte(c.body), 0o644)
		wrote("tests/" + c.file)
		s.setNotes("refactor", underState(s.notes("refactor"), "- WAITING: desk review"))
		s.gateAtPark("refactor")
		n := s.notes("refactor")
		if waiting(n) || !strings.Contains(n, "FIX BEFORE STEP DONE: ") || !strings.Contains(n, c.file) {
			t.Fatalf("%s: the park must be held and name the file:\n%s", c.file, n)
		}
		// Windows video bench #12: the line that names it, so muse needn't hunt for it
		if ev := s.board.Since(0, "refactor", []string{"gate"}, 1); !strings.Contains(n, c.where) || len(ev) == 0 || !strings.Contains(ev[0].Text, c.where) {
			t.Fatalf("%s: the hold must show the line %q:\n%s\n%+v", c.file, c.where, n, ev)
		}
		os.Remove(filepath.Join(s.st.Root, "tests", c.file))
	}
	s.setNotes("refactor", underState(s.notes("refactor"), "- WAITING: desk review"))
	s.gateAtPark("refactor")
	if !waiting(s.notes("refactor")) {
		t.Fatal("with no references left, the seat parks")
	}
	old := filepath.Join(s.st.Root, "NOTES-desk.md") // written before the task: never holds the park
	os.WriteFile(old, []byte("see .osenv/tasks/refactor/\n"), 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	s.gateAtPark("refactor")
	if !waiting(s.notes("refactor")) {
		t.Fatal("a file older than the task must not hold the park")
	}
	// Windows light rc8: the desk's own file (no action of the task wrote it) goes to the desk at once, and muse's
	// NOTES are left alone
	desk := filepath.Join(s.st.Root, "tests", "test_panel_out.py")
	os.WriteFile(desk, []byte("OUT = os.path.join(ROOT, \".osenv\", \"tasks\", \"refactor\", \"out\")\n"), 0o644)
	before := s.notes("refactor")
	s.gateAtPark("refactor")
	ev := s.board.Since(0, "refactor", []string{"gate"}, 1)
	if n := s.notes("refactor"); !waiting(n) || n != before || len(ev) == 0 || !strings.Contains(ev[0].Text, "the desk decides: tests/test_panel_out.py:1:") {
		t.Fatalf("a file the task never wrote is the desk's call:\n%s\n%+v", n, ev)
	}
	os.Remove(desk)
	stuck := filepath.Join(s.st.Root, "jobs", "refactor.md") // VM 0.2.3 #14: a file muse can't fix never loops
	os.MkdirAll(filepath.Dir(stuck), 0o755)
	os.WriteFile(stuck, []byte("save the screenshot to .osenv/tasks/refactor/out/\n"), 0o644)
	wrote("jobs/refactor.md")
	s.gateAtPark("refactor")
	if waiting(s.notes("refactor")) {
		t.Fatal("the first time, the park is held")
	}
	s.setNotes("refactor", underState(s.notes("refactor"), "- WAITING: desk review"))
	s.gateAtPark("refactor")
	if !waiting(s.notes("refactor")) {
		t.Fatal("the same hold twice parks for the desk instead of looping")
	}
}

// A restart never puts a second engine on a hybrid: a run still alive from before waits it out.
func TestRestartWaitsForOrphan(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("alpha", "# alpha\nwork"); err != nil {
		t.Fatal(err)
	}
	orphanRun := exec.Command(os.Args[0])
	orphanRun.Env = append(os.Environ(), "OSENV_FAKE=sleep")
	if err := orphanRun.Start(); err != nil {
		t.Fatal(err)
	}
	s.tasks["alpha"].RunPID = orphanRun.Process.Pid
	s.saveTask(s.tasks["alpha"])

	s2, err := newServer(dir) // the restart
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "second-engine")
	t.Setenv("OSENV_FAKE", "touch")
	t.Setenv("OSENV_FAKE_MARKER", marker)
	s2.st.Cfg.Muse.Cmd = os.Args[0]
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	s2.schedule()
	if _, live := s2.live["alpha"]; live {
		t.Fatal("a second engine started while the run from before the restart was alive")
	}
	orphanRun.Wait()
	s2.schedule() // this pass wraps the old run up (its undo record); a later one starts the next run
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s2.schedule()
		s2.mu.Lock()
		_, live := s2.live["alpha"]
		s2.mu.Unlock()
		if _, err := os.Stat(marker); err == nil && !live { // the run finished: nothing still reads the stubbed ask
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("runs must resume once the old run has ended")
}

// procAlive: this process is alive, and a finished one is not (the orphan wait relies on both).
func TestProcAlive(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("this process must read as alive")
	}
	c := exec.Command(os.Args[0], "-test.run=^$")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if procAlive(c.Process.Pid) {
		t.Fatal("a finished process must not read as alive")
	}
}

// Linux I-7: a new lesson that contradicts an old one is flagged to the desk (both stood, silently).
func TestContradictionFlagged(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := &Item{ID: "L6", Kind: "lesson", Scope: "global", Severity: "nudge", Status: "active", Text: "Validate numbers with try/except around int().", Detect: "parsing a number without catching ValueError", Tags: tagsOf("validate numbers int try except")}
	s.learn.items = append(s.learn.items, old)
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"learnable": 0.9, "contra_L6": 0.6}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	it, how, err := s.learn.Add(AddIn{Task: "api", Text: "int() accepts ' 12 ' and '1_000'; validate numbers with a digits regex, not try/except int().", Detect: "validating numbers only with try/except int()", Source: "deepseek"})
	if err != nil || !strings.Contains(how, "CONTRADICTS L6") || !strings.Contains(it.Why, "contradicts L6") || old.Status != "active" {
		t.Fatalf("flag it, retire nothing: %v %q %q %s", err, how, it.Why, old.Status)
	}
}

// Linux I-4: an in-lane "keep as-is" note (Jev 0.53) reaches muse; an off-lane item doesn't; only a clear
// in-lane finding is taken in as a lesson.
func TestLaneKeepsConfirmations(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("api", "# api\nbuild the books API"); err != nil {
		t.Fatal(err)
	}
	fb := `{"item":"OK (do not fix): PATCH on an unknown id returns 404 before the body is checked; that order is correct.","rule":"r","detect":"d"}
{"item":"The page colors feel muddy; use a teal accent."}
`
	os.WriteFile(s.tdir("api", "feedback-deepseek.jsonl"), []byte(fb), 0o644)
	oldAsk := ask
	defer func() { ask = oldAsk }()
	intake := false
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			if k == "learnable" {
				intake = true
			}
			p := map[string]float64{"i0": 0.53, "i1": 0.05}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	s.laneFilter("api", "deepseek", time.Time{}, true)
	n := s.notes("api")
	if !strings.Contains(n, "that order is correct") || strings.Contains(n, "teal") || intake {
		t.Fatalf("keep the confirmation, drop the off-lane item, learn nothing from an unsure score:\n%s (intake %v)", n, intake)
	}
	if ev := s.board.Since(0, "api", []string{"feedback"}, 5); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Text, "teal accent") { // Windows 0.2.3 #18
		t.Fatalf("the desk must see what was dropped: %+v", ev)
	}
}

// Windows 0.2.3 #19: a correction aimed at osenv itself goes to the desk, never into muse's lessons.
func TestToolCorrectionForDesk(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"learnable": 0.9, "general": 0.9, "tool": 0.84}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	n := len(s.learn.items)
	r, err := verbs["learn.add"].run(s, json.RawMessage(`{"task":"tv","text":"Number formatting is in qwen's lane: keep such fixes.","detect":"the lane filter drops a formatting fix","source":"opus"}`))
	if err != nil || len(s.learn.items) != n || !strings.Contains(fmt.Sprint(r), "for the desk") {
		t.Fatalf("not filed, answered for the desk: %v %v (%d items, was %d)", r, err, len(s.learn.items), n)
	}
	if ev := s.board.Since(0, "tv", []string{"learn"}, 5); len(ev) != 1 || !strings.Contains(ev[0].Text, "for the desk") {
		t.Fatalf("the desk must see it on the board: %+v", ev)
	}
}

// A reviewer's board post names the reviewer, not the seat (Linux run, minor).
func TestSayWho(t *testing.T) {
	if b := string(sayWho([]byte(`{"do":"say","task":"api","text":"hi"}`), "deepseek")); !strings.Contains(b, `"who":"deepseek-api"`) {
		t.Fatal(b)
	}
	for _, c := range []struct{ body, role string }{{`{"do":"say","task":"api","text":"hi"}`, "muse"}, {`{"do":"say","who":"desk"}`, "qwen"}, {`{"do":"status"}`, "qwen"}} {
		if b := string(sayWho([]byte(c.body), c.role)); b != c.body {
			t.Fatalf("%s as %s changed: %s", c.body, c.role, b)
		}
	}
}

// Windows 0.2.3: "off" in the hybrid's own folder is exempt; NOT ASKED FOR outranks the danger warning;
// a detect edit is replayed on the lesson's past catches.
func TestWindows023Verdicts(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.st.Cfg.Kickback = true
	if _, err := s.taskNew("todo", "# todo\nadd a clear command"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var sc map[string]float64
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := sc[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	write := func(path string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "todo", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
			"tool_input": map[string]any{"file_path": path, "content": "x"}}}))
	}
	sc = map[string]float64{"on_task": 0.36, "succeeds": 0.9}
	if r := write(s.st.Root + "/.osenv/tasks/todo/NOTES.md"); r != "map[]" {
		t.Fatalf("off in its own folder must pass: %s", r)
	}
	sc = map[string]float64{"on_task": 0.9, "succeeds": 0.9, "danger": 0.72, "unasked": 0.82}
	if r := write("todo.py"); !strings.Contains(r, "NOT ASKED FOR") {
		t.Fatalf("an unasked confirmation step must be told NOT ASKED FOR: %s", r)
	}
	s.st.appendJSONL("acts.jsonl", map[string]any{"event": "pre", "task": "t", "what": "python -m pytest > out.txt", "learned": []string{"L3"}})
	s.learn.items = append(s.learn.items, &Item{ID: "L3", Kind: "lesson", Scope: "global", Severity: "kick", Status: "active", Text: "t", Detect: "d"})
	sc = map[string]float64{"d": 0.4}
	r, err := verbs["learn.edit"].run(s, json.RawMessage(`{"id":"L3","detect":"a PowerShell redirect"}`))
	if err != nil || !strings.Contains(fmt.Sprint(r), "misses_now:true") {
		t.Fatalf("the edit must say the old catch is now missed: %v %v", r, err)
	}
}

// The tool library: exec/ is the registry, Jev lists fitting tools in muse's brief, a step done by hand
// gets "there's a tool for that" as a note, and saving a tool is never sent back as unasked.
func TestToolLibrary(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(s.st.Root, "exec"), 0o755)
	os.WriteFile(filepath.Join(s.st.Root, "exec", "shot.sh"), []byte("#!/bin/sh\n# usage: exec/shot.sh <page.html> <out.png> - full-page screenshot with headless Firefox\n"), 0o755)
	os.WriteFile(filepath.Join(s.st.Root, "exec", "csvsum.py"), []byte("# usage: python3 exec/csvsum.py <file.csv> <column> - sum a CSV column\n"), 0o644)
	os.WriteFile(filepath.Join(s.st.Root, "exec", "notes.txt"), []byte("no usage line here\n"), 0o644)
	if got := s.tools(); len(got) != 2 || got[1].Path != "exec/shot.sh" || !strings.HasPrefix(got[1].Usage, "exec/shot.sh <page.html>") {
		t.Fatalf("registry: %+v", got)
	}
	s.st.Cfg.Kickback = true
	if _, err := s.taskNew("page", "# page\nbuild pages/c.html; gate: a full-page screenshot"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var sc map[string]float64
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k, q := range qs {
			p := sc[k]
			if strings.Contains(fmt.Sprint(q.Instructions), "includes the step this tool does: exec/shot.sh") {
				p = 0.72
			}
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	fit := s.fitTools("page")
	if len(fit) != 1 || fit[0].Path != "exec/shot.sh" {
		t.Fatalf("Jev lists only the fitting tool: %+v", fit)
	}
	s.tasks["page"].Tools = []string{"exec/shot.sh"}
	cmd := func(c string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "page", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "run_shell_command", "tool_input": map[string]any{"command": c}}}))
	}
	sc = map[string]float64{"on_task": 0.9, "succeeds": 0.9, "t_0": 0.76}
	if r := cmd("firefox --headless --screenshot out/c.png file:///p/pages/c.html"); !strings.Contains(r, "THERE'S A TOOL FOR THAT: exec/shot.sh") || strings.Contains(r, "deny") {
		t.Fatalf("a step done by hand gets a note, not a block: %s", r)
	}
	if r := cmd("exec/shot.sh pages/c.html out/c.png"); strings.Contains(r, "TOOL FOR THAT") {
		t.Fatalf("using the tool is not doing it by hand: %s", r)
	}
	s.kicks = map[string]time.Time{}
	if r := fmt.Sprint(s.hook(HookIn{Task: "page", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
		"tool_input": map[string]any{"file_path": s.st.Root + "/.osenv/tasks/page/NOTES.md", "content": "- screenshot made with exec/shot.sh"}}})); strings.Contains(r, "TOOL FOR THAT") {
		t.Fatalf("writing its own notes never gets the tool note: %s", r)
	}
	sc = map[string]float64{"on_task": 0.9, "succeeds": 0.9, "unasked": 0.9}
	r := fmt.Sprint(s.hook(HookIn{Task: "page", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
		"tool_input": map[string]any{"file_path": s.st.Root + "/exec/pageshot.py", "content": "# usage: python3 exec/pageshot.py <html> <png> - screenshot\n"}}}))
	if strings.Contains(r, "NOT ASKED FOR") {
		t.Fatalf("saving a tool is always allowed: %s", r)
	}
}

// Take-buckets (the 0.3.0 plan card "owned-memory"), with a stub Jev that scores takes the way the real
// one did: gossip and wishes high on "words, opinion or wish", the fake shadow high on "against the job".
func takeJev(scores map[string][4]float64) func(map[string]any, map[string]Q) (map[string]Ans, error) {
	return func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			pre, id, _ := strings.Cut(k, "_")
			sc := scores[id]
			p := map[string]float64{"gos": sc[0], "con": sc[1], "act": sc[2], "req": sc[3]}[pre]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
}

func TestTakeRecall(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	add := func(text, tags string) string {
		tg, _ := parseTags(tags)
		tk, err := s.takes.add(text, tg, "desk", "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	ik := add("hero walk cycle: foot IK added, foot slide 9 cm to 1 cm", "mygame,3d,model,hero")
	adj := add("hero's walk got a much smoother, nicer, more natural feel", "mygame,3d,model,hero")
	moss := add("added the moss material; roughness 0.85; qwen PASS", "mygame,3d,texture")
	pretty := add("added a beautiful, much better moss material", "mygame,3d,texture")
	gossip := add("operator said the moss should be blue", "mygame,texture")
	shadow := add("added a dark blob decal under hero as its shadow", "mygame,3d,model")
	tex := add("cliff texture pass: 2k albedo and normal maps", "mygame,texture")
	other := add("osenv view cropped 4 renders by fraction", "osenv,3d,model")
	for i := 0; i < 12; i++ {
		add(fmt.Sprintf("script pass %d: 3 tests added", i), "mygame,script")
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	// gos, con, act, req
	ask = takeJev(map[string][4]float64{ik: {0.06, 0.31, 0.94, 0.76}, adj: {0.76, 0.49, 0.73, 0.29}, moss: {0.08, 0.22, 0.92, 0.6},
		pretty: {0.27, 0.27, 0.67, 0.6}, gossip: {0.91, 0.3, 0.12, 0.5}, shadow: {0.06, 0.88, 0.68, 0.3}, tex: {0.13, 0.4, 0.8, 0.5},
		other: {0.05, 0.1, 0.95, 0.99}})
	// 1) only mygame, only the 3d and model buckets; never osenv's 3d or model takes; gates hold; the plain take first
	got := s.recall([]string{"mygame", "3d", "model"}, "rework hero's walk", 5)
	if !reflect.DeepEqual(got, []string{ik, moss}) {
		t.Fatalf("3d,model recall: %v (want %s then %s: the gossip, the opinion and the fake shadow never; the adjective-heavy moss take ranks under the floor)", got, ik, moss)
	}
	// 2) another spool at the same time gets its own set; 2c) a gossip take never surfaces
	got2 := s.recall([]string{"mygame", "texture"}, "a wet mud material", 5)
	if contains(got2, gossip) || !contains(got2, tex) || contains(got2, ik) {
		t.Fatalf("texture recall: %v", got2)
	}
	// 2b) the plain moss take outranks its adjective-heavy twin
	if got := s.recall([]string{"mygame", "texture", "3d"}, "moss", 5); !contains(got, moss) || contains(got, pretty) {
		t.Fatalf("adjectives weigh heavily: %v", got)
	}
	// x caps it; a bucket holding only gossip gives none
	if got := s.recall([]string{"mygame", "3d", "model"}, "walk", 1); len(got) != 1 {
		t.Fatalf("x=1: %v", got)
	}
	add2 := add("the desk thinks the old rock was ugly", "mygame,rocks")
	ask = takeJev(map[string][4]float64{add2: {0.6, 0.2, 0.7, 0.8}}) // even when Jev half-reads it as an action, the gate holds
	if got := s.recall([]string{"mygame", "rocks"}, "rocks", 5); len(got) != 0 {
		t.Fatalf("a bucket of gossip gives the hybrid no memories: %v", got)
	}
}

func TestTakeVerbsAndMessage(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	do := func(role, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/v1", strings.NewReader(body))
		if role != "" {
			req.Header.Set("X-Osenv-Role", role)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	// 6) an empty store changes nothing: no message, no memories in the brief
	if m := do("", `{"do":"status"}`); m["take_buckets"] != nil {
		t.Fatalf("empty store: %v", m)
	}
	if s.memories(nil) != "" {
		t.Fatal("no takes, no MEMORIES section")
	}
	// 3) a PASS records exactly one take, under the project ID and the task's buckets; 6b) its bucket appears
	s.taskSpool("rock", "# rock\nmoss", []string{"mygame", "3d", "texture"}, nil)
	s.taskVerdict("rock", true, "moss material added, roughness 0.85")
	if l := s.takes.list("", ""); len(l) != 1 || !reflect.DeepEqual(l[0].Tags, []string{"mygame", "3d", "texture"}) || l[0].Source != "osenv" {
		t.Fatalf("one automatic take: %+v", l)
	}
	recs, _ := filepath.Glob(s.st.path("done", "rock-*"))
	v, _ := os.ReadFile(filepath.Join(recs[0], "VERDICT.md"))
	tj, _ := os.ReadFile(filepath.Join(recs[0], "task.json"))
	if !strings.HasPrefix(string(v), "PASS ") || !strings.Contains(string(v), "roughness 0.85") || !strings.Contains(string(tj), `"state": "passed"`) {
		t.Fatalf("the record keeps its verdict and final state (Windows #30): %q %s", v, tj)
	}
	// 8) every desk response carries the wording in full, with every bucket; an engine's responses don't
	want := takeWordA + "mygame,3d (1), mygame,texture (1)" + takeWordB + " " + pruneWord
	if m := do("", `{"do":"task.list"}`); m["take_buckets"] != want {
		t.Fatalf("desk message:\n%v\nwant\n%s", m["take_buckets"], want)
	}
	if m := do("", `{"do":"nope"}`); m["take_buckets"] != want {
		t.Fatalf("an error response carries it too: %v", m)
	}
	if m := do("muse", `{"do":"read","since":0}`); m["take_buckets"] != nil {
		t.Fatal("a hybrid's responses never carry the desk's message")
	}
	// 4) add, move, remove; a move needs the take's own project ID; a removed take never reaches a hybrid
	id := do("", `{"do":"take.add","text":"cliff texture pass: 2k maps","tags":"mygame,texture"}`)["result"].(map[string]any)["id"].(string)
	if m := do("", `{"do":"take.add","id":"`+id+`","tags":"osenv,+script"}`); m["ok"] != false {
		t.Fatalf("a move under another project ID must be refused: %v", m)
	}
	if m := do("", `{"do":"take.add","id":"`+id+`","tags":"mygame,+script,-texture"}`); m["ok"] != true || fmt.Sprint(m["result"].(map[string]any)["tags"]) != "[mygame script]" {
		t.Fatalf("move: %v", m)
	}
	if !strings.Contains(s.memories([]string{id}), "cliff texture pass") {
		t.Fatal("a picked take is in the brief")
	}
	s.taskSpool("mem", "# mem\nx", []string{"mygame", "script"}, []string{id})
	brief := func(task string) string {
		if _, err := s.command(task, "hybrid-"+task, "muse", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(s.tdir(task, "BRIEF-muse.md"))
		return string(b)
	}
	if b := brief("mem"); !strings.Contains(b, "MEMORIES (takes the desk spooled you with") || !strings.Contains(b, "cliff texture pass: 2k maps ("+id+")") {
		t.Fatal("muse's real brief must carry the take")
	}
	s.taskNew("rock2", "# rock2\nx")
	if strings.Contains(brief("rock2"), "MEMORIES") {
		t.Fatal("a hybrid spooled without takes gets no MEMORIES section")
	}
	do("", `{"do":"take.remove","id":"`+id+`"}`)
	if s.memories([]string{id}) != "" || len(s.takes.list("", "script")) != 0 {
		t.Fatal("a removed take never reaches a hybrid again")
	}
	// 5) a hybrid can't add, move or remove takes: the server refuses, and the hook denies the command
	if m := do("muse", `{"do":"take.add","text":"x","tags":"mygame,a"}`); m["ok"] != false {
		t.Fatalf("server must refuse a hybrid: %v", m)
	}
	s.taskNew("art", "# art\nx")
	r := s.hook(HookIn{Task: "art", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash",
		"tool_input": map[string]any{"command": "osenv do take.remove id=T1"}}})
	if !strings.Contains(fmt.Sprint(r), "desk's call") {
		t.Fatalf("hook must deny a hybrid's take.remove: %v", r)
	}
	// a hybrid never reads the store: the server refuses take.list, and the hook denies it and the raw file (Linux I-8)
	if m := do("muse", `{"do":"take.list"}`); m["ok"] != false {
		t.Fatalf("server must refuse take.list to a hybrid: %v", m)
	}
	for _, body := range []string{`{"do":"batch","ops":[{"do":"status"},{"do":"take.list"}]}`, `{"do":"task.verdict","name":"art","pass":true,"text":"x"}`,
		`{"do":"learn.add","source":"owner","text":"x","detect":"y"}`, `{"do":"task.undo","name":"art"}`} {
		if m := do("qwen", body); m["ok"] != false || !strings.Contains(fmt.Sprint(m["error"]), "the desk's call") {
			t.Fatalf("server must refuse a desk verb from a hybrid, batches included: %s -> %v", body, m)
		}
	}
	if m := do("muse", `{"do":"batch","ops":[{"do":"status"},{"do":"read","since":0}]}`); m["ok"] != true {
		t.Fatalf("a hybrid's own verbs still work in a batch: %v", m)
	}
	for _, p := range []map[string]any{{"tool_name": "bash", "tool_input": map[string]any{"command": "osenv do take.list"}},
		{"tool_name": "bash", "tool_input": map[string]any{"command": "cat .osenv/takes.jsonl"}},
		{"tool_name": "read_file", "tool_input": map[string]any{"file_path": s.st.Root + "/.osenv/takes.jsonl"}}} {
		if r := s.hook(HookIn{Task: "art", Event: "pre", Role: "muse", Payload: p}); !strings.Contains(fmt.Sprint(r), "Takes are the desk's") {
			t.Fatalf("hook must deny a hybrid reading takes: %v -> %v", p, r)
		}
	}
	// 3b) an automatic take is one short record, never the desk's whole verdict (Linux I-7)
	long := "tool works when called as its usage line says. " + strings.Repeat("repro: touch -d 2026-01-01 stale.png; python3 exec/shot.py a b; ", 12)
	s.taskSpool("longv", "# longv\nx", []string{"mygame", "script"}, nil)
	s.taskVerdict("longv", true, long)
	var last Take
	for _, tk := range s.takes.list("", "script") {
		last = tk
	}
	if last.Text != "longv PASS: tool works when called as its usage line says." {
		t.Fatalf("automatic take: %q", last.Text)
	}
	if st := shortTake(strings.Repeat("word ", 200)); len(st) > takeMax+4 || !strings.HasSuffix(st, " ...") {
		t.Fatalf("a long take without sentences is cut at a word: %d %q", len(st), st)
	}
	// the store survives a restart (replayed from takes.jsonl)
	s2, _ := newServer(s.st.Root)
	if len(s2.takes.list("", "")) != len(s.takes.list("", "")) || s2.takes.message() != s.takes.message() {
		t.Fatalf("replay: %+v", s2.takes.list("", ""))
	}
}

// 7) SKILL.md quotes the API's wordings word for word; this fails the build if one changes without the other.
func TestTakeWordingInSkill(t *testing.T) {
	b, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{takeWordA + "x, y, z" + takeWordB, pruneWord} {
		if !strings.Contains(string(b), w) {
			t.Errorf("SKILL.md must quote, word for word: %s", w)
		}
	}
}

// Shorter qwen runs: every screenshot tiled into one sheet at view size, a prompt that says to judge from it,
// and a small turn cap. No screenshots: qwen runs as before. deepseek never gets a sheet.
func TestContactSheet(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSENV_PLAN_KEY", "test-key")
	s.taskNew("dash", "# dash\nrestyle the dashboard")
	args := func(engine string) string { // the command line plus the brief file it points to
		c, err := s.command("dash", "hybrid-dash", engine, "", "route R-visual", "", "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(s.tdir("dash", "BRIEF-"+engine+".md"))
		return strings.Join(c.Args, " ") + "\n" + string(b)
	}
	if a := args("qwen"); strings.Contains(a, "CONTACT SHEET") || !strings.Contains(a, fmt.Sprintf("--max-session-turns %d", s.st.Cfg.Turns)) {
		t.Fatalf("no screenshots, no sheet, the usual turns: %.200s", a)
	}
	for i, c := range []struct {
		n    string
		w, h int
	}{{"a-768.png", 768, 3000}, {"a-1280.png", 1280, 2000}, {"before.png", 400, 300}} {
		p := s.tdir("dash", "out", c.n)
		fh, _ := os.Create(p)
		png.Encode(fh, image.NewRGBA(image.Rect(0, 0, c.w, c.h)))
		fh.Close()
		at := time.Now().Add(-time.Duration(i) * time.Minute) // newest first on the sheet
		os.Chtimes(p, at, at)
	}
	a := args("qwen")
	if !strings.Contains(a, "CONTACT SHEET: "+filepath.FromSlash(".osenv/tasks/dash/views/sheet.jpg")) || !strings.Contains(a, "--max-session-turns 16") ||
		!strings.Contains(a, "a-768.png") || !strings.Contains(a, "before.png") {
		t.Fatalf("qwen gets the sheet and the cap: %.400s", a)
	}
	f, _ := os.Open(s.tdir("dash", "views", "sheet.jpg"))
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || cfg.Width != 2*768+8 || cfg.Height != 1200+8+300 { // row 1: two 768-wide tiles cut to 1200 tall; row 2: 400x300
		t.Fatalf("sheet %dx%d: %v", cfg.Width, cfg.Height, err)
	}
	f2, _ := os.Open(s.tdir("dash", "views", "sheet.jpg")) // the 768x3000 page keeps its full width, and its cut band shows
	sheet, _, _ := image.Decode(f2)
	f2.Close()
	if r, g, _, _ := sheet.At(100, 1200-360-6).RGBA(); r>>8 < 150 || g>>8 > 90 {
		t.Fatalf("a tall page's tile must show the red cut band, got r=%d g=%d", r>>8, g>>8)
	}
	if a := args("deepseek"); strings.Contains(a, "CONTACT SHEET") || strings.Contains(a, "{{SHEET}}") {
		t.Fatalf("deepseek gets no sheet: %.200s", a)
	}
}

// Lesson packs: only lessons that caught a real mistake travel (never owner rules, retired or one-task ones),
// a fresh project loads them, a repeat import skips them, and on the first run of a task that would hit the
// mistake the lesson is in muse's brief.
func TestLessonPack(t *testing.T) {
	src, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src.learn.items = append(src.learn.items,
		&Item{ID: "L3", Kind: "lesson", Scope: "global", Severity: "kick", Status: "active", Source: "opus", Catches: 3, Text: "Save output with Out-File -Encoding utf8, never a bare >.", Detect: "a bare > redirect in PowerShell"},
		&Item{ID: "L4", Kind: "lesson", Scope: "global", Severity: "nudge", Status: "active", Source: "opus", Text: "never caught anything", Detect: "x"},
		&Item{ID: "O10", Kind: "lesson", Scope: "global", Severity: "kick", Status: "active", Source: "owner", Catches: 5, Text: "no CDN", Detect: "y"},
		&Item{ID: "L7", Kind: "lesson", Scope: "global", Severity: "nudge", Status: "retired", Source: "opus", Catches: 2, Text: "retired", Detect: "z"},
		&Item{ID: "L8", Kind: "lesson", Scope: "task:x", Severity: "nudge", Status: "active", Source: "opus", Catches: 1, Text: "one task", Detect: "w"})
	pack := filepath.Join(t.TempDir(), "pack.json")
	r, err := verbs["learn.export"].run(src, json.RawMessage(fmt.Sprintf(`{"file":%q}`, pack)))
	if err != nil || r.(map[string]any)["lessons"] != 1 {
		t.Fatalf("export only the proven global lesson: %v %v", r, err)
	}
	dir := t.TempDir()
	dst, _ := newServer(dir)
	imp := func() map[string]any {
		r, err := verbs["learn.import"].run(dst, json.RawMessage(fmt.Sprintf(`{"file":%q}`, pack)))
		if err != nil {
			t.Fatal(err)
		}
		return r.(map[string]any)
	}
	if got := imp(); len(got["added"].([]string)) != 1 {
		t.Fatalf("import: %v", got)
	}
	if got := imp(); len(got["added"].([]string)) != 0 || len(got["skipped_as_already_here"].([]string)) != 1 {
		t.Fatalf("a repeat import skips it: %v", got)
	}
	if ev := dst.board.Since(0, "", []string{"learn"}, 5); len(ev) != 2 || !strings.HasPrefix(ev[1].Text, "imported no lessons from ") || !strings.HasSuffix(ev[1].Text, "'s pack: all 1 are already here") {
		t.Fatalf("the repeat import says so: %+v", ev)
	}
	var it *Item
	for _, x := range dst.learn.items {
		if x.Source == "pack" {
			it = x
		}
	}
	if it == nil || it.Scope != "global" || it.Severity != "kick" || it.Catches != 0 || !strings.Contains(it.Why, "3 catches") {
		t.Fatalf("imported lesson: %+v", it)
	}
	// the page's test: a task that would hit the mistake gets the lesson in its brief on its first run
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k, q := range qs {
			p := 0.1
			if strings.Contains(fmt.Sprint(q.Instructions), "Out-File -Encoding utf8") {
				p = 0.9
			}
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	marker := filepath.Join(dir, "brief.txt")
	t.Setenv("OSENV_FAKE", "args")
	t.Setenv("OSENV_FAKE_MARKER", marker)
	dst.st.Cfg.Muse.Cmd = os.Args[0]
	dst.taskNew("save", "# save\nrun the tests and save their output to out/pytest.txt")
	dst.run("save")
	if a, _ := os.ReadFile(marker); !strings.Contains(string(a), "BRIEF-muse.md") || strings.Contains(string(a), "save their output") {
		t.Fatalf("the engine's command line points at its brief file and never carries the job (a pkill -f on job words killed muse): %.300s", a)
	}
	if b, _ := os.ReadFile(dst.tdir("save", "BRIEF-muse.md")); !strings.Contains(string(b), "Save output with Out-File -Encoding utf8, never a bare >. (lesson "+it.ID+")") {
		t.Fatalf("the imported lesson must be in the first run's brief:\n%.600s", b)
	}
	r2 := dst.hook(HookIn{Task: "save", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash",
		"tool_input": map[string]any{"command": "osenv do learn.import file=x.json"}}})
	if !strings.Contains(fmt.Sprint(r2), "desk's call") {
		t.Fatalf("a hybrid can't import lessons: %v", r2)
	}
}

// The proof kit: a transcript is always UTF-8 with its exit code, the desk checks every gate file with one
// command, and a broken, blank or failed proof file is named as such.
func TestProofKit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OSENV_FAKE", "utf16")
	code, text, err := proofRun(filepath.Join(dir, "out", "run.txt"), []string{os.Args[0], "-test.run=none"})
	b, _ := os.ReadFile(filepath.Join(dir, "out", "run.txt"))
	if err != nil || code != 3 || string(b) != text || !strings.Contains(text, "h\u00e9llo") || !strings.Contains(text, "exit=3") || bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		t.Fatalf("UTF-16 output must land as UTF-8 with the exit code: %d %v %q", code, err, b)
	}
	t.Setenv("OSENV_FAKE", "")
	t.Setenv("OSENV_FAKE", "utf16")
	if code, _, _ := proofRunExpect(filepath.Join(dir, "neg", "negative.txt"), []string{os.Args[0], "-test.run=none"}, 3); code != 3 || !strings.HasPrefix(checkOne(filepath.Join(dir, "neg", "negative.txt")), "ok exit=3, as expected") {
		t.Fatalf("an intended failure is ok: %s", checkOne(filepath.Join(dir, "neg", "negative.txt")))
	}
	if code, _, _ := proofRunExpect(filepath.Join(dir, "neg", "wrong.txt"), []string{os.Args[0], "-test.run=none"}, 1); code != 3 || !strings.HasPrefix(checkOne(filepath.Join(dir, "neg", "wrong.txt")), "FAILED exit=3 (expected 1)") {
		t.Fatalf("the wrong failure is FAILED: %s", checkOne(filepath.Join(dir, "neg", "wrong.txt")))
	}
	t.Setenv("OSENV_FAKE", "")
	if code, _, _ := proofRun(filepath.Join(dir, "out", "missing.txt"), []string{"no-such-program-osenv"}); code != 127 {
		t.Fatalf("a missing program is exit 127, got %d", code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"up"}`) }))
	defer srv.Close()
	if ok, _, err := proofHTTP(filepath.Join(dir, "out", "up.txt"), srv.URL, 200, `"up"`); !ok || err != nil {
		t.Fatalf("http check: %v", err)
	}
	if ok, _, _ := proofHTTP(filepath.Join(dir, "out", "down.txt"), srv.URL, 201, ""); ok {
		t.Fatal("a wrong status must FAIL")
	}
	os.WriteFile(filepath.Join(dir, "out", "ps.txt"), []byte{0xff, 0xfe, 'o', 0, 'k', 0}, 0o644)
	for name, img := range map[string]image.Image{"blank.png": image.NewRGBA(image.Rect(0, 0, 40, 40)), "real.png": func() image.Image {
		m := image.NewRGBA(image.Rect(0, 0, 40, 40))
		m.Set(20, 20, color.RGBA{255, 0, 0, 255})
		m.Set(0, 0, color.RGBA{0, 0, 255, 255})
		return m
	}()} {
		f, _ := os.Create(filepath.Join(dir, "out", name))
		png.Encode(f, img)
		f.Close()
	}
	var out bytes.Buffer
	if proofCheck(&out, []string{filepath.Join(dir, "out")}) != 1 {
		t.Fatal("check must fail when any proof file isn't ok")
	}
	for _, want := range []string{"FAILED", "run.txt", "missing.txt", "ok", "up.txt", "down.txt", "BROKEN", "ps.txt", "BLANK", "blank.png", "7 files, 5 not ok"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.HasPrefix(checkOne(filepath.Join(dir, "out", "real.png")), "ok 40x40") || !strings.HasPrefix(checkOne(filepath.Join(dir, "out", "up.txt")), "ok result: PASS") {
		t.Fatal("a real image and a passing http check are ok")
	}
	if name, _ := findBrowser(); name != "" { // the screenshot needs a real browser; skipped where there is none
		os.WriteFile(filepath.Join(dir, "p.html"), []byte("<h1>proof</h1><p style='height:1500px;background:#cde'>tall</p><footer>end</footer>"), 0o644)
		w, h, _, err := proofShot(filepath.Join(dir, "out", "p.png"), filepath.Join(dir, "p.html"), 768)
		if err != nil || w != 768 || h < 1500 || !strings.HasPrefix(checkOne(filepath.Join(dir, "out", "p.png")), "ok") {
			t.Fatalf("full-page shot: %dx%d %v", w, h, err)
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "out", ".osenv-shot-*")); len(m) != 0 {
			t.Fatalf("the browser profile must be removed: %v", m)
		}
	}
}

// The mutation check: bugs are planted one at a time in a copy of the project, a bug the tests catch is
// "caught", one they miss is named, and the real file is never touched.
func TestProofMutate(t *testing.T) {
	root := t.TempDir()
	src := "def add(a, b):\n    return a + b\n\n\n# a comment with a < b in it\ndef bigger(a, b):\n    return a > b\n\n\ndef note():\n    \"\"\"\n    True when a == b.\n    \"\"\"\n    return \"a < b and 3\"  # do not change: 7 < 8\n"
	os.WriteFile(filepath.Join(root, "lib.py"), []byte(src), 0o644)
	t.Setenv("OSENV_FAKE", "mutcheck")
	out := filepath.Join(root, "out", "mutate.txt")
	ok, report, err := proofMutate(root, out, filepath.Join(root, "lib.py"), []string{os.Args[0], "-test.run=none"}, 10, 0, time.Minute, io.Discard)
	if err != nil || ok {
		t.Fatalf("a missed bug must fail the check: %v %v", ok, err)
	}
	for _, want := range []string{`caught   line 2: " + " -> " - "`, `SURVIVED line 7: " > " -> " <= "`, "result: FAIL (1 of 2 planted bugs", "planted 2 bugs across"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "lib.py")); string(b) != src {
		t.Fatal("the real file must never be touched")
	}
	if m, _ := filepath.Glob(filepath.Join(root, ".osenv", "mutate-*")); len(m) != 0 {
		t.Fatalf("the copy must be removed: %v", m)
	}
	if !strings.HasPrefix(checkOne(out), "FAILED") {
		t.Fatalf("proof check reads the report: %s", checkOne(out))
	}
	if ok, report, _ := proofMutate(root, out, filepath.Join(root, "lib.py"), []string{os.Args[0], "-test.run=none"}, 10, 1, time.Minute, io.Discard); !ok || !strings.HasPrefix(report, "$ osenv proof mutate "+shellLine([]string{out, "lib.py", "--allow", "1", "--timeout", "1m0s", "--", os.Args[0], "-test.run=none"})+"\n") || !strings.Contains(report, "within the 1 allowed") || !strings.Contains(report, "SURVIVED") || !strings.HasPrefix(checkOne(out), "ok") {
		t.Fatalf("a survivor the job allows passes, and is still listed: %v %s", ok, report)
	}
	os.WriteFile(filepath.Join(root, "lib.py"), []byte("def add(a, b):\n    return a - b\n"), 0o644)
	if ok, report, _ := proofMutate(root, out, filepath.Join(root, "lib.py"), []string{os.Args[0], "-test.run=none"}, 10, 0, time.Minute, io.Discard); ok || !strings.Contains(report, "the tests fail before any bug is planted") {
		t.Fatalf("failing tests can't measure anything: %s", report)
	}
}

// Undo per run: a run that breaks files is put back byte for byte; a file changed again after the run, or
// touched by another hybrid during it, is left alone and named; a running task can't be undone.
func TestUndoRun(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	orig := map[string][]byte{"app.py": []byte("def main():\n    return 42\n"), "config.ini": []byte("[x]\na=1\n"), "shared.md": []byte("v1"), "notes.md": []byte("n1")}
	for f, b := range orig {
		os.WriteFile(filepath.Join(dir, f), b, 0o644)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	t.Setenv("OSENV_FAKE", "breakfile")
	s.st.Cfg.Muse.Cmd = os.Args[0]
	s.taskNew("bad", "# bad\nx")
	s.run("bad")
	// what the run's hooks recorded (the fake engine has no hooks): its own writes, in the run's window
	act := func(task, tool, what string) {
		s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": task, "role": "muse", "tool": tool, "what": what, "verdict": "allow"})
	}
	act("bad", "write_file", filepath.Join(dir, "app.py"))
	act("bad", "bash", "echo 'left behind' > scratch.txt && rm config.ini")
	act("bad", "bash", "mkdir -p tmpdir/deep && echo x > tmpdir/deep/x.txt")
	act("bad", "edit_file", filepath.Join(dir, "shared.md"))
	// another hybrid wrote shared.md while the run was going, and only read app.py (a read is not a touch)
	act("other", "write_file", filepath.Join(dir, "shared.md"))
	act("other", "bash", "md5sum app.py; python3 -c 'import app'")
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("n2"), 0o644) // untouched by the run: must stay n2
	if d, err := s.taskUndo("bad", 0, false, true); err != nil || !contains(d["would_restore"].([]string), "app.py") || !fileExists(filepath.Join(dir, "scratch.txt")) {
		t.Fatalf("a dry run says what it would do and touches nothing: %v %v", d, err)
	}
	r, err := s.taskUndo("bad", 0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string][]byte{"app.py": orig["app.py"], "config.ini": orig["config.ini"], "shared.md": []byte("the run's edit"), "notes.md": []byte("n2")} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); !bytes.Equal(b, want) {
			t.Fatalf("%s: got %q want %q (%v)", f, b, want, r)
		}
	}
	if fileExists(filepath.Join(dir, "tmpdir")) {
		t.Fatal("folders the run created, now empty, go too (Windows #20)")
	}
	if fileExists(filepath.Join(dir, "scratch.txt")) || !strings.Contains(fmt.Sprint(r["left_alone"]), "task other also wrote it") {
		t.Fatalf("the run's new file goes; the shared file is named: %v", r)
	}
	if r, _ := s.taskUndo("bad", 0, true, false); !contains(r["restored"].([]string), "shared.md") || !strings.Contains(fmt.Sprint(r["left_alone"]), "already as it was before the run") {
		t.Fatalf("force undoes the shared file too, and names what was undone already: %v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "shared.md")); string(b) != "v1" {
		t.Fatalf("shared.md after force: %q", b)
	}
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("the desk's own fix"), 0o644) // changed again after the run
	if r, _ := s.taskUndo("bad", 1, true, false); !strings.Contains(fmt.Sprint(r["left_alone"]), "changed again after the run") {
		t.Fatalf("a file changed after the run is left alone: %v", r)
	}
	s.mu.Lock()
	s.live["bad"] = nil
	s.mu.Unlock()
	if _, err := s.taskUndo("bad", 0, false, false); err == nil || !strings.Contains(err.Error(), "pause it first") {
		t.Fatalf("a running task can't be undone: %v", err)
	}
	s.mu.Lock()
	delete(s.live, "bad")
	s.mu.Unlock()
	if r := s.hook(HookIn{Task: "bad", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash",
		"tool_input": map[string]any{"command": "osenv do task.undo name=bad"}}}); !strings.Contains(fmt.Sprint(r), "desk's call") {
		t.Fatalf("a hybrid can't undo runs: %v", r)
	}
	s.retire("bad")
	s.undoGC()
	if b, _ := os.ReadDir(s.st.path("undo", "blobs")); len(b) != 0 {
		t.Fatalf("a retired task's kept contents go: %d left", len(b))
	}
}

// task.get lists the deliverables, not a browser profile's cache (Linux 0.3 run, I-1).
func TestTaskGetOut(t *testing.T) {
	s, _ := newServer(t.TempDir())
	s.taskNew("page", "# page\nx")
	os.MkdirAll(s.tdir("page", "out", ".profile", "cache"), 0o755)
	os.WriteFile(s.tdir("page", "out", ".profile", "cache", "part1"), []byte("x"), 0o644)
	for i := 0; i < 45; i++ {
		os.WriteFile(s.tdir("page", "out", fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644)
	}
	info, _ := s.taskInfo("page")
	out := info["out"].([]string)
	if len(out) != 41 || out[40] != "(+5 more files)" || strings.Contains(strings.Join(out, " "), ".profile") {
		t.Fatalf("out: %d entries, last %q", len(out), out[len(out)-1])
	}
}

// Viewing a view doesn't stack suffixes (Linux 0.3 run, I-3).
func TestViewOfView(t *testing.T) {
	s, _ := newServer(t.TempDir())
	s.taskNew("v", "# v\nx")
	p := s.tdir("v", "out", "plants.png")
	f, _ := os.Create(p)
	png.Encode(f, image.NewRGBA(image.Rect(0, 0, 1000, 500)))
	f.Close()
	out, err := s.viewImage(p, ViewIn{Path: p, Task: "v"})
	if err != nil || !strings.Contains(out, "plants-768px.jpg") {
		t.Fatalf("%v %s", err, out)
	}
	again, _ := s.viewImage(s.tdir("v", "views", "plants-768px.jpg"), ViewIn{Path: "plants-768px.jpg", Task: "v"})
	if strings.Contains(again, "768px-768px") || !strings.Contains(again, "plants-768px.jpg") {
		t.Fatalf("a view of a view: %s", again)
	}
}

// Windows 0.3 run: a server a hybrid started outlived the run, held run.log, and the PASS stalled and split the
// record. The engine writes through a pipe now, so a leftover process never holds run.log, and a retire never
// stops halfway.
func TestLeftoverProcessAndRetire(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	t.Setenv("OSENV_FAKE", "orphan")
	s.st.Cfg.Muse.Cmd = os.Args[0]
	s.taskNew("api", "# api\nx")
	start := time.Now()
	s.run("api")
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("the run must end when the engine ends, not when its leftovers do: %s", d)
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(s.tdir("api", "run.log"))
	if !strings.Contains(string(b), "ENGINE-SAID-HI") || !strings.Contains(string(b), "exit 0") {
		t.Fatalf("run.log keeps the engine's output:\n%s", b)
	}
	// retire now, while the leftover child still holds run.log open (on Windows this used to block the PASS)
	s.taskNew("api2", "# api2\nx")
	s.run("api2")
	rec2, err := s.retire("api2")
	if err != nil || s.tasks["api2"] != nil || !fileExists(filepath.Join(rec2, "run.log")) || fileExists(s.tdir("api2")) {
		t.Fatalf("a leftover process holding run.log must not stop the retire: %v %v", rec2, err)
	}
	if ev := s.board.Since(0, "api2", []string{"blocked"}, 3); len(ev) != 0 {
		t.Fatalf("nothing should be left behind: %+v", ev)
	}
	defer time.Sleep(3500 * time.Millisecond) // let the leftover children exit before the temp folder is removed
	// a folder that can't be deleted: the task still retires, keeps one record, and says what was left
	stuck := s.tdir("api", "stuck")
	os.MkdirAll(stuck, 0o755)
	os.WriteFile(filepath.Join(stuck, "held.txt"), []byte("x"), 0o644)
	os.Chmod(stuck, 0o500)
	defer os.Chmod(stuck, 0o755)
	rec, err := s.retire("api")
	if err != nil || s.tasks["api"] != nil || !fileExists(filepath.Join(rec, "JOB.md")) || !fileExists(filepath.Join(rec, "run.log")) {
		t.Fatalf("retire must finish: %v %v", rec, err)
	}
	if ev := s.board.Since(0, "api", []string{"blocked"}, 3); runtime.GOOS != "windows" && (len(ev) != 1 || !strings.Contains(ev[0].Text, "couldn't be fully deleted")) {
		t.Fatalf("what was left is named: %+v", ev)
	}
}

// Windows 0.3 run: Edge laid a 390 px shot out at ~500 px and handed back a slice. A probe page shows the width
// the browser really laid out: at 390 the 360-419 band (green) must be showing, whatever browser this machine has.
func TestProofShotNarrow(t *testing.T) {
	browsers := []string{""}
	for _, b := range []string{"google-chrome", "chromium", "msedge"} {
		if _, err := exec.LookPath(b); err == nil {
			browsers = append(browsers, b)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "probe.html"), []byte(`<!doctype html><html><head><style>body{margin:0}.b{display:none;height:60px}
@media (max-width:359px){.w300{display:block;background:#c00}}
@media (min-width:360px) and (max-width:419px){.w390{display:block;background:#060}}
@media (min-width:420px){.wbig{display:block;background:#333}}</style></head><body><div class="b w300"></div><div class="b w390"></div><div class="b wbig"></div><div style="height:700px;background:#eee"></div><p>end</p></body></html>`), 0o644)
	for _, b := range browsers {
		if name, _ := findBrowser(); name == "" && b == "" {
			t.Skip("no browser on this machine")
		}
		t.Setenv("OSENV_BROWSER", b)
		out := filepath.Join(dir, "shot-"+b+".png")
		w, h, used, err := proofShot(out, filepath.Join(dir, "probe.html"), 390)
		if err != nil || w != 390 || h < 700 {
			t.Fatalf("%s: %dx%d %v", used, w, h, err)
		}
		f, _ := os.Open(out)
		img, _, _ := image.Decode(f)
		f.Close()
		if r, g, bl, _ := img.At(10, 20).RGBA(); r>>8 != 0 || g>>8 != 102 || bl>>8 != 0 {
			t.Fatalf("%s laid the page out at the wrong width (band colour %d,%d,%d)", used, r>>8, g>>8, bl>>8)
		}
	}
}

// A run that outlived a server restart gets its undo record closed when it ends, so it can be undone.
func TestUndoOrphanRun(t *testing.T) {
	dir := t.TempDir()
	s, _ := newServer(dir)
	os.WriteFile(filepath.Join(dir, "game.py"), []byte("v1"), 0o644)
	s.taskNew("game", "# game\nx")
	s.undoBefore("game", "muse") // the old server started the run, then died
	os.WriteFile(filepath.Join(dir, "game.py"), []byte("v2 by the orphan run"), 0o644)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": "game", "role": "muse", "tool": "edit_file", "what": "game.py", "verdict": "allow"}) // its hooks still reach the new server
	s.mu.Lock()
	s.orphans["game"] = orphan{pid: 1 << 30, until: time.Now().Add(time.Hour)} // a pid that isn't alive
	s.mu.Unlock()
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	s.schedule()
	for i := 0; i < 50; i++ {
		if b, _ := os.ReadFile(s.undoPath("game", 1)); strings.Contains(string(b), `"end"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	_, live := s.live["game"]
	s.mu.Unlock()
	if live {
		t.Fatal("the next run waits for the next pass")
	}
	r, err := s.taskUndo("game", 1, false, false)
	if b, _ := os.ReadFile(filepath.Join(dir, "game.py")); err != nil || string(b) != "v1" {
		t.Fatalf("the orphan run must be undoable: %v %v %q", r, err, b)
	}
}

// Undo with another hybrid working in the same folder: only this run's own writes are put back. A file a
// script wrote (named by no act) is left alone while others were working, and undone when nobody else was.
func TestUndoAttribution(t *testing.T) {
	a := func(tool, what string) map[string]any {
		return map[string]any{"tool": tool, "what": what, "verdict": "allow"}
	}
	for _, c := range []struct {
		act  map[string]any
		want bool
	}{
		{a("write_file", "/p/tetris/board.py"), true},
		{a("edit_file", "/p/tetris/board.py"), true},
		{a("bash", "md5sum tetris/board.py tetris/pieces.py"), false},
		{a("bash", "python3 -m pytest tests/test_board.py -q > out.txt"), false},
		{a("bash", "python3 gen.py > tetris/board.py"), true},
		{a("bash", "cat x >> tetris/board.py"), true},
		{a("bash", "sed -i s/a/b/ tetris/board.py"), true},
		{a("bash", "sed -n 1,5p tetris/board.py"), false},
		{a("bash", "cp tetris/board.py /tmp/b.py"), false},
		{a("bash", "cp /tmp/b.py tetris/board.py"), true},
		{a("bash", "rm -f tetris/board.py"), true},
		{a("bash", "git diff 2>&1 | grep tetris/board.py"), false},
		{a("run_shell_command", "Set-Content tetris/board.py -Value x"), true},
		{map[string]any{"tool": "write_file", "what": "/p/tetris/board.py", "verdict": "deny"}, false},
	} {
		if got := actWrites(c.act, "tetris/board.py"); got != c.want {
			t.Errorf("%v: got %v want %v", c.act["what"], got, c.want)
		}
	}
	dir := t.TempDir()
	s, _ := newServer(dir)
	os.WriteFile(filepath.Join(dir, "gen.txt"), []byte("v1"), 0o644)
	s.taskNew("me", "# me\nx")
	n := s.undoBefore("me", "muse")
	os.WriteFile(filepath.Join(dir, "gen.txt"), []byte("v2 from a script"), 0o644)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": "me", "tool": "bash", "what": "python3 make.py", "verdict": "allow"})
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": "you", "tool": "read_file", "what": "x.md", "verdict": "log"})
	s.undoAfter("me", n)
	r, _ := s.taskUndo("me", n, false, true)
	if !strings.Contains(fmt.Sprint(r["left_alone"]), "while you also worked here, and no action of this run writes it") {
		t.Fatalf("with another hybrid working, a script's file is left alone: %v", r)
	}
	if r, _ := s.taskUndo("me", n, true, true); !contains(r["would_restore"].([]string), "gen.txt") {
		t.Fatalf("force undoes it: %v", r)
	}
	// Linux video bench: with nobody else working, every change in the window used to be this run's, so the undo of
	// a read-only review would have deleted the proof the desk wrote from its own terminal meanwhile.
	s.taskNew("solo", "# solo\nx")
	time.Sleep(2100 * time.Millisecond) // past the earlier acts' one-second margin
	m := s.undoBefore("solo", "qwen")
	os.MkdirAll(filepath.Join(dir, "desk-proof"), 0o755)
	os.WriteFile(filepath.Join(dir, "desk-proof", "check.txt"), []byte("the desk's own proof"), 0o644)
	os.WriteFile(filepath.Join(dir, "gen.txt"), []byte("v3 from a script"), 0o644)
	long := "python3 tools/render_all.py --quiet " + strings.Repeat("--flag ", 40) + "> own.txt" // the write is past the 240 characters "what" keeps
	os.WriteFile(filepath.Join(dir, "own.txt"), []byte("the run's own"), 0o644)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": "solo", "tool": "bash", "what": clip(long, 240), "judged": long, "verdict": "allow"})
	s.undoAfter("solo", m)
	r, _ = s.taskUndo("solo", m, false, true)
	if left := fmt.Sprint(r["left_alone"]); !strings.Contains(left, "desk-proof/check.txt") || !strings.Contains(left, "gen.txt") || !strings.Contains(left, "no action of this run writes it (the desk") ||
		contains(r["would_delete"].([]string), "desk-proof/check.txt") || !contains(r["would_delete"].([]string), "own.txt") {
		t.Fatalf("only what this run's own actions wrote is undone, even with nobody else working: %v", r)
	}
	if r, _ := s.taskUndo("solo", m, true, true); !contains(r["would_delete"].([]string), "desk-proof/check.txt") || !contains(r["would_restore"].([]string), "gen.txt") {
		t.Fatalf("force undoes the rest: %v", r)
	}
}

// A run stopped with the server (Ctrl-C) may never record its end; the next start closes it, so it can be undone.
func TestUndoClosedAtStart(t *testing.T) {
	dir := t.TempDir()
	s, _ := newServer(dir)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0o644)
	s.taskNew("x", "# x\nx")
	n := s.undoBefore("x", "muse")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2"), 0o644)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": now(), "event": "pre", "task": "x", "role": "muse", "tool": "write_file", "what": "a.txt", "verdict": "allow"}) // the run's own write
	s2, _ := newServer(dir)                                                                                                                                             // the restart
	if _, err := s2.taskUndo("x", n, false, false); err != nil {
		t.Fatalf("the run must be closed at start and undoable: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "v1" {
		t.Fatalf("got %q", b)
	}
}

// The mutation operators: Python's is/not, and nothing planted inside an inline comment (Linux rc1 N-3).
func TestMutantOps(t *testing.T) {
	ms, code := mutants("def f(x):\n    if x is None:\n        return 1\n    if not x.strip():\n        return 2\n    y = z  # never mind: a < b\n    return x not in SEEN\n", 10)
	var got []string
	for _, m := range ms {
		got = append(got, fmt.Sprintf("%d:%q->%q", m.line, m.from, m.to))
	}
	want := []string{`2:" is "->" is not "`, `3:"1"->"2"`, `4:" not "->" "`, `5:"2"->"3"`, `7:" not in "->" in "`}
	if !reflect.DeepEqual(got, want) || code != 7 {
		t.Fatalf("got %v (code lines %d), want %v", got, code, want)
	}
}

// Video bench (Linux): owner rules appear once in muse's brief; engines keep temp files in the task folder;
// a lesson edit leaves a board event.
func TestVideoBenchLinuxFixes(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o, _, _ := s.learn.Add(AddIn{Source: "owner", Text: "SPEC.md is the owner's; never edit it.", Detect: "The action edits SPEC.md."})
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := 0.9 // every lesson judged relevant, owner rules included
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	marker := filepath.Join(s.st.Root, "args.txt")
	t.Setenv("OSENV_FAKE", "args")
	t.Setenv("OSENV_FAKE_MARKER", marker)
	s.st.Cfg.Muse.Cmd = os.Args[0]
	s.taskNew("engine", "# engine\nrender")
	s.run("engine")
	b, _ := os.ReadFile(s.tdir("engine", "BRIEF-muse.md"))
	if n := strings.Count(string(b), o.Text); n != 1 {
		t.Fatalf("the owner rule must appear once in the brief, got %d:\n%s", n, b)
	}
	// Windows light run: run.start's lesson list left the owner rule out while the brief showed it
	if ev := s.board.Since(0, "engine", []string{"run.start"}, 1); len(ev) != 1 || strings.Count(fmt.Sprint(ev[0].Data["lessons"]), o.ID) != 1 {
		t.Fatalf("run.start lists the owner rule once: %+v", ev)
	}
	c, err := s.command("engine", "hybrid-engine", "muse", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var tmp string
	for _, e := range c.Env {
		if strings.HasPrefix(e, "TMPDIR=") {
			tmp = strings.TrimPrefix(e, "TMPDIR=")
		}
	}
	if tmp != s.tdir("engine", "tmp") || !fileExists(tmp) || !strings.Contains(strings.Join(c.Env, "\n"), "TEMP="+tmp) {
		t.Fatalf("engines keep temp files in the task folder: %q", tmp)
	}
	verbs["learn.edit"].run(s, json.RawMessage(`{"id":"`+o.ID+`","detect":"The action edits or deletes SPEC.md."}`))
	if ev := s.board.Since(0, "", []string{"learn"}, 5); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Text, "edited "+o.ID+" (detect)") {
		t.Fatalf("a lesson edit leaves a board event: %+v", ev)
	}
}

// Video bench: danger 0.5-0.75 is a note, 0.75 or more is still a hard deny (Linux #7); proof check reads video
// as video, never as text (Windows #7), and a corrupt clip is still BROKEN.
func TestWarnNoteAndMediaProof(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.st.Cfg.Kickback = true
	s.taskNew("app", "# app\nrun and close the app")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var danger float64
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "danger": danger}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	cmd := func() string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "app", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash",
			"tool_input": map[string]any{"command": "kill $(cat tmp/app.pid); rm -f tmp/app_smoke.log"}}}))
	}
	danger = 0.6
	if r := cmd(); strings.Contains(r, "deny") || !strings.Contains(r, "could be destructive") {
		t.Fatalf("a warn is a note: %s", r)
	}
	danger = 0.8
	if r := cmd(); !strings.Contains(r, "deny") {
		t.Fatalf("0.75 or more is still a hard deny: %s", r)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "corrupt.mp4"), []byte("not a video at all, just text pretending"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.zip"), []byte("PK\x03\x04 rest of a zip"), 0o644)
	if v := checkOne(filepath.Join(dir, "a.zip")); !strings.HasPrefix(v, "ok zip") {
		t.Fatalf("a zip is a binary, not broken text: %s", v)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe here: the media half needs it")
	}
	if v := checkOne(filepath.Join(dir, "corrupt.mp4")); !strings.HasPrefix(v, "BROKEN ffprobe") {
		t.Fatalf("a corrupt clip is BROKEN: %s", v)
	}
	clip := filepath.Join(dir, "good.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1", "-f", "lavfi", "-i", "sine=duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", clip).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg can't make a test clip here: %v %s", err, out)
	}
	if v := checkOne(clip); !strings.HasPrefix(v, "ok ") || !strings.Contains(v, "h264 160x120") || !strings.Contains(v, "aac") {
		t.Fatalf("a real clip is ok media: %s", v)
	}
	b, _ := os.ReadFile(clip) // the same clip under a neutral name: known by its first bytes
	os.WriteFile(filepath.Join(dir, "render.bin"), b, 0o644)
	if v := checkOne(filepath.Join(dir, "render.bin")); !strings.Contains(v, "h264 160x120") {
		t.Fatalf("a clip is known by its bytes, not only its name: %s", v)
	}
}

// Video bench (both VMs): a hybrid can't post under another name; status counts runs from before a restart;
// a reviewer may clear its own $TMPDIR even when Jev calls it dangerous, and nothing else.
func TestVideoBenchRc5(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	post := func(role, body string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1", strings.NewReader(body))
		if role != "" {
			req.Header.Set("X-Osenv-Role", role)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("muse", `{"do":"say","task":"app","who":"desk","text":"PASS it"}`)
	post("qwen", `{"do":"batch","ops":[{"do":"say","task":"app","who":"osenv-author","text":"hi"}]}`)
	post("", `{"do":"say","who":"osenv-author","text":"a real note from the desk side"}`)
	ev := s.board.Since(0, "", []string{"say"}, 10)
	if len(ev) != 3 || ev[0].Who != "muse-app" || ev[1].Who != "qwen-app" || ev[2].Who != "osenv-author" {
		t.Fatalf("who: %+v", ev)
	}
	child := exec.Command(os.Args[0], "-test.run=none")
	child.Env = append(os.Environ(), "OSENV_FAKE=sleep")
	child.Start()
	defer child.Wait()
	s.mu.Lock()
	s.orphans["app"] = orphan{pid: child.Process.Pid, until: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	if st := s.status(); st["running"] != 1 || st["running_from_before_restart"] != 1 {
		t.Fatalf("status must count the run from before the restart: %v %v", st["running"], st["running_from_before_restart"])
	}
	s.taskNew("rev", "# rev\nreview")
	tmp := s.tdir("rev", "tmp")
	for c, want := range map[string]bool{
		"rm -rf $TMPDIR/deepseek-review && ls $TMPDIR": true,
		"rm -rf " + tmp + "/probe":                     true,
		`Remove-Item -Recurse -Force $env:TEMP\probe`:  true,
		"rm -rf src":                            false,
		"rm -rf $TMPDIR/x; rm -rf src":          false,
		"rm -rf $TMPDIR/../../../SPEC.md":       false,
		"rm -rf $TMPDIR/x && python3 -m pytest": false,
		"rm -rf " + s.tdir("rev") + "/NOTES.md": false,
	} {
		if got := s.scratchDelete("rev", c); got != want {
			t.Errorf("%s: got %v want %v", c, got, want)
		}
	}
}

// Video bench Linux #15: a reviewer read a desk document the desk rewrote mid-review, and the lessons it filed baked
// in the old rule. Intake shows Jev the desk's orders since the review began; lessons they outdate aren't filed.
func TestOutdatedLessonNotFiled(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("engine", "# engine\nrender a project"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	newer := ""
	ask = func(st map[string]any, qs map[string]Q) (map[string]Ans, error) {
		if o, ok := st["desk_orders_since_the_review_began"].(string); ok {
			newer = o
		}
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"i0": 0.9, "learnable": 0.9, "outdated": 0.8}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	fb := `{"item":"CONTRACT's round(in*src_fps) gives the clip's first frame.","rule":"A clip's first frame is round(in*src_fps).","detect":"computing a clip's first frame without round(in*src_fps)"}` + "\n"
	began := time.Now().Add(-time.Minute)
	s.board.Post(Event{Kind: "orders", Task: "engine", Who: "desk", Text: "Desk decision (CONTRACT.md updated): the first frame is floor(in*src_fps + 1e-6)."})
	n := len(s.learn.items)
	os.WriteFile(s.tdir("engine", "feedback-deepseek.jsonl"), []byte(fb), 0o644)
	s.laneFilter("engine", "deepseek", began, true)
	ev := s.board.Since(0, "engine", []string{"learn"}, 5)
	if len(s.learn.items) != n || !strings.Contains(newer, "floor(") || len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Text, "outdate") {
		t.Fatalf("the desk's newer orders outdate it: not filed, and the board says why: %d items (was %d), orders %q, %+v", len(s.learn.items), n, newer, ev)
	}
	newer = ""
	os.WriteFile(s.tdir("engine", "feedback-deepseek.jsonl"), []byte(fb), 0o644)
	s.laneFilter("engine", "deepseek", time.Now().Add(time.Minute), true) // the orders came before this review: nothing newer
	if len(s.learn.items) != n+1 || newer != "" {
		t.Fatalf("no newer orders: filed as usual: %d items (was %d), orders %q", len(s.learn.items), n, newer)
	}
}

// Video bench Linux #17: a routed review that outlived a restart gets its findings merged when it ends, before muse
// runs again, and counts in the task record. One past the run timeout is stopped, never run beside a second engine.
func TestOrphanReviewWrappedUp(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskNew("tools", "# tools\nmake the checkers"); err != nil {
		t.Fatal(err)
	}
	// what the old server did before it died: started a routed deepseek review, which wrote its findings
	s.board.Post(Event{Kind: "run.start", Task: "tools", Who: "hybrid-tools", Text: "deepseek route R-code", Data: map[string]any{"engine": "deepseek", "routed": true}})
	os.WriteFile(s.tdir("tools", "feedback-deepseek.jsonl"), []byte(`{"item":"check_frames passes having compared 0 frames."}`+"\n"), 0o644)
	s.mu.Lock()
	s.orphans["tools"] = orphan{pid: 1 << 30, until: time.Now().Add(time.Hour)} // it has ended
	s.mu.Unlock()
	oldAsk := ask
	defer func() { ask = oldAsk }()
	gate, asked := make(chan struct{}), make(chan struct{}, 1)
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			if k == "i0" {
				asked <- struct{}{}
				<-gate // the lane check is under way
			}
			p := 0.9
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	s.schedule()
	<-asked
	s.schedule() // while the findings are being merged, muse must not start
	s.mu.Lock()
	_, live := s.live["tools"]
	s.mu.Unlock()
	close(gate)
	if live {
		t.Fatal("muse started before the orphaned review's findings were merged")
	}
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		_, waiting := s.orphans["tools"]
		s.mu.Unlock()
		if !waiting {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	tk := *s.tasks["tools"]
	_, left := s.orphans["tools"]
	s.mu.Unlock()
	ev := s.board.Since(0, "tools", []string{"orphan"}, 1)
	if left || !strings.Contains(s.notes("tools"), "compared 0 frames") || tk.Engines["deepseek"] != 1 || tk.RunsTotal != 1 || len(ev) != 1 || !strings.Contains(ev[0].Text, "deepseek run") {
		t.Fatalf("merged, counted, and announced: left %v, engines %v, runs %d, %+v\n%s", left, tk.Engines, tk.RunsTotal, ev, s.notes("tools"))
	}

	// past the run timeout and still alive: stopped, and not counted as a done review
	s2, _ := newServer(t.TempDir())
	s2.taskNew("app", "# app\nx")
	s2.board.Post(Event{Kind: "run.start", Task: "app", Who: "hybrid-app", Text: "muse", Data: map[string]any{"engine": "muse", "routed": false}})
	stuck := exec.Command(os.Args[0])
	stuck.Env = append(os.Environ(), "OSENV_FAKE=sleep")
	setGroup(stuck)
	if err := stuck.Start(); err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	s2.orphans["app"] = orphan{pid: stuck.Process.Pid, until: time.Now().Add(-time.Second)}
	s2.mu.Unlock()
	s2.schedule()
	stuck.Wait()
	for i := 0; i < 100; i++ {
		if ev := s2.board.Since(0, "app", []string{"orphan"}, 1); len(ev) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s2.mu.Lock()
	tk = *s2.tasks["app"]
	s2.mu.Unlock()
	if ev := s2.board.Since(0, "app", []string{"orphan"}, 1); stuck.ProcessState.ExitCode() == 0 || tk.Engines["muse"] != 0 || tk.RunsTotal != 1 || len(ev) != 1 || !strings.Contains(ev[0].Text, "stopped at the run timeout") {
		t.Fatalf("stopped at the timeout: exit %d, engines %v, runs %d, %+v", stuck.ProcessState.ExitCode(), tk.Engines, tk.RunsTotal, ev)
	}
}

// Video bench, Windows: a screenshot the job's gates name was kicked back as creep. Jev saw the job's first 1500
// characters (its gates start at 3273) and the command's first 240 (Win32 setup code, not the capture).
func TestJevSeesGatesAndWholeCommand(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := "# Job: app\n" + strings.Repeat("Build the window as SPEC 3 says. ", 100) + "\n## Gates\n- Screenshots of the real window, taken with the CopyFromScreen recipe.\n"
	if _, err := s.taskNew("app", job); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var st map[string]any
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		st = state
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	cmd := "Add-Type -AssemblyName System.Drawing; Add-Type @\"\n" + strings.Repeat("// window interop\n", 20) + "\"@\n$g.CopyFromScreen($r.Left, $r.Top, 0, 0, $bmp.Size); $bmp.Save('proof/app/01.png')"
	s.hook(HookIn{Task: "app", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "powershell", "tool_input": map[string]any{"command": cmd}}})
	task, _ := st["task"].(string)
	action, _ := st["action"].(string)
	if len(job) < 3000 || !strings.Contains(task, "CopyFromScreen recipe") || !strings.Contains(action, "$bmp.Save('proof/app/01.png')") {
		t.Fatalf("Jev must see the job's gates and the whole command:\ntask ...%q\naction ...%q", clip(task[max(0, len(task)-120):], 120), action[max(0, len(action)-80):])
	}
}

// Video bench, Linux #19: Jev read the project's own tmp/ as /tmp, so an owner rule against writing outside the
// project blocked correct work and let a real /tmp write through. Jev is told the working folder and where each path
// in the action resolves, following cd; a file tool's content never adds paths.
func TestPathFacts(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("app", "# app\nx")
	root := s.st.Root
	home, _ := os.UserHomeDir()
	rooted := func(p string) string { return filepath.Clean(filepath.VolumeName(root) + p) } // /tmp is C:\tmp on Windows
	in := func(p string) string { return filepath.Join(root, p) + " (inside the project folder)" }
	out := func(p string) string { return p + " (OUTSIDE the project folder)" }
	for _, c := range []struct {
		inp        map[string]any
		want, none []string
	}{
		{map[string]any{"command": "python3 exec/xdo.py shotroot tmp/screen_now2.png && /opt/osenv/osenv view --task app tmp/screen_now2.png 2>/dev/null"},
			[]string{"tmp/screen_now2.png -> " + in("tmp/screen_now2.png"), "exec/xdo.py -> " + in("exec/xdo.py")}, []string{"/dev/null", "/opt/osenv/osenv"}}, // the program it runs is no fact (Linux light rc8)
		{map[string]any{"command": `& "C:\kit\osenv.exe" do say task=app text="STEP DONE"; /home/u/kit/osenv do say task=app text=x > /tmp/said.txt`},
			[]string{"/tmp/said.txt -> " + out(rooted("/tmp/said.txt"))}, []string{"osenv.exe", "kit/osenv"}},
		{map[string]any{"command": "cp media/a.mp4 /tmp/a.mp4; ffmpeg -y -i media/a.mp4 ../out.mp4 ~/Videos/b.mp4 $HOME/c.mp4"},
			[]string{"/tmp/a.mp4 -> " + out(rooted("/tmp/a.mp4")), "../out.mp4 -> " + out(filepath.Join(filepath.Dir(root), "out.mp4")), "~/Videos/b.mp4 -> " + out(filepath.Join(home, "Videos", "b.mp4")), out(filepath.Join(home, "c.mp4"))}, nil},
		{map[string]any{"command": "cd /tmp && echo x > y.txt; cd " + root + "/tests && python3 run.py > ../tmp/r.txt"},
			[]string{"cd /tmp -> " + out(rooted("/tmp")) + ": the paths after it resolve from there", "../tmp/r.txt -> " + in("tmp/r.txt")}, nil},
		{map[string]any{"command": "echo x > $TMPDIR/scratch.txt; ffmpeg -i a.mp4 $OUT/b.mp4; cd $WORK && echo y > later/z.txt"},
			[]string{filepath.Join(s.tdir("app", "tmp"), "scratch.txt") + " (inside the project folder)"}, []string{"later/z.txt", "b.mp4"}}, // an unknown folder gets no fact, never a wrong one
		{map[string]any{"command": "curl -s http://127.0.0.1:8833/v1 -d x; ffmpeg -i a.mp4 -r 30000/1001 b.mp4"}, nil, []string{"http", "127.0.0.1", "8833", "30000"}},
		{map[string]any{"path": filepath.Join(root, "tmp", "s.py"), "content": "open('/etc/passwd')\n"}, []string{" -> " + in("tmp/s.py")}, []string{"passwd"}},
		{map[string]any{"command": `grep -rn "\.osenv\|tasks/panel" panel/ > tmp/hits.txt`}, []string{"tmp/hits.txt -> " + in("tmp/hits.txt")}, nil},
		{map[string]any{"command": "ls a/1 a/2 a/3 a/4 a/5 a/6 a/7 a/8 a/9 a/10 a/11 a/12 a/13 a/14"}, []string{"a/12 -> "}, []string{"a/13"}}, // at most 12
	} {
		if runtime.GOOS != "windows" { // a grep's regex is no path (Linux light rc8: `\.osenv\` was OUTSIDE the project)
			c.none = append(c.none, `\.osenv`)
		}
		got := s.pathFacts("app", c.inp)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v: missing %q in:\n%s", c.inp, w, got)
			}
		}
		for _, n := range c.none {
			if strings.Contains(got, n) {
				t.Errorf("%v: %q must not appear in:\n%s", c.inp, n, got)
			}
		}
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var st map[string]any
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		st = state
		return map[string]Ans{}, nil
	}
	s.hook(HookIn{Task: "app", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": "import -window root /tmp/screen.png"}}})
	if !strings.Contains(fmt.Sprint(st["working_directory"]), root) || !strings.Contains(fmt.Sprint(st["paths_in_action"]), out(rooted("/tmp/screen.png"))) {
		t.Fatalf("the hook must pass the working folder and the path facts: %v", st)
	}
	if !strings.Contains(fmt.Sprint(st["temp_folder"]), s.tdir("app", "tmp")+" (inside the project folder)") { // Windows light run: tempfile.mkdtemp() read as outside
		t.Fatalf("Jev is told where temp files go: %v", st["temp_folder"])
	}
	if a := s.acts("app", 1); len(a) == 0 || !strings.Contains(fmt.Sprint(a[0]["paths"]), out(rooted("/tmp/screen.png"))) { // Windows light run: the desk couldn't audit them
		t.Fatalf("acts keep the path facts Jev was given: %v", a)
	}
}

// Video bench, Windows #9: a hybrid killed "the newest python" and took down another hybrid's tests. A kill that
// picks processes by name, start time or recency is denied for every role; stopping its own process isn't.
func TestKillByNameDenied(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("app", "# app\nx")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	kills := 0.0
	ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "kills": kills}[k]
			out[k] = Ans{Noul: &p}
		}
		if _, ok := qs["kills"]; !ok {
			return nil, fmt.Errorf("the kill question must be asked")
		}
		return out, nil
	}
	run := func(role, cmd string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "app", Event: "pre", Role: role, Payload: map[string]any{"tool_name": "powershell", "tool_input": map[string]any{"command": cmd}}}))
	}
	for _, role := range []string{"muse", "deepseek"} {
		kills = 0.66 // the lowest measured kill by a search with the rc9 wording
		if r := run(role, "Get-Process python | Sort-Object StartTime | Select-Object -Last 1 | Stop-Process"); !strings.Contains(r, "deny") || !strings.Contains(r, "by the PID or window ID you saved when you started it") || !strings.Contains(r, "-PassThru") || !strings.Contains(r, `echo $$ > "$TMPDIR/<name>.pid"`) {
			t.Fatalf("%s: a kill by name or recency is denied, with the right way to stop a process: %s", role, r)
		}
		kills = 0.41 // the highest measured own-process stop with the rc9 wording (by pidfile, then a listing)
		if r := run(role, `kill $(cat "$TMPDIR/app.pid"); pgrep -af videoeditor.app`); strings.Contains(r, "deny") {
			t.Fatalf("%s: stopping its own process goes through: %s", role, r)
		}
	}
}

// Windows video bench #14: a key=@file value written by PowerShell 5.1 (Set-Content -Encoding utf8) starts with a BOM.
func TestAtFileDropsBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "say.txt")
	os.WriteFile(p, []byte("\ufeffon \"the newest python.exe\"\r\n"), 0o644)
	if v := typed("@" + p); v != `on "the newest python.exe"` {
		t.Fatalf("the BOM and the closing line break must go, the quotes stay: %q", v)
	}
}

// Video bench: jobs run to about 5000 characters, with the requirements at the end. The plain check missed a timeline
// without the file names the job asks for at character 1834, and the park gate missed "ask for a visual and a code
// review" at character 5046: both saw only the job's head.
func TestLongJobSeenWhole(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := "# app\n" + strings.Repeat("Build the window as SPEC 3 says. ", 150) + "\nEach block shows the file name.\nAsk for a visual review (qwen) and a code review (deepseek) before STEP DONE.\n"
	if _, err := s.taskNew("app", job); err != nil {
		t.Fatal(err)
	}
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"VERDICT: PASS"}}]}`)
	}))
	defer srv.Close()
	s.st.Cfg.Plan.URL = srv.URL
	t.Setenv("OSENV_PLAN_KEY", "test-key")
	for i := 1; i <= 5; i++ { // the job's gates name 5 screenshots: all 5 go (Windows: with 4 sent, one read as "not provided")
		fh, _ := os.Create(s.tdir("app", "out", fmt.Sprintf("shot%d.png", i)))
		img := image.NewRGBA(image.Rect(0, 0, 40, 90))
		img.Set(i, i, color.RGBA{uint8(i), 0, 0, 255}) // five different states: identical shots hold the park
		png.Encode(fh, img)
		fh.Close()
	}
	s.plainCheck("app")
	if len(job) < 5000 || !strings.Contains(sent, "Each block shows the file name.") {
		t.Fatalf("the plain check must see the whole job (%d chars)", len(job))
	}
	if n := strings.Count(sent, `"type":"image_url"`); n != 5 || !strings.Contains(sent, `"enable_thinking":false`) { // Windows #19: thinking used up the budget
		t.Fatalf("all 5 screenshots, and no thinking: %d images, %s", n, clip(sent, 300))
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var seen string
	ask = func(st map[string]any, qs map[string]Q) (map[string]Ans, error) {
		if _, ok := qs["visual"]; ok {
			seen, _ = st["job"].(string)
		}
		return map[string]Ans{}, nil
	}
	s.setNotes("app", underState(s.notes("app"), "- WAITING: desk review"))
	s.gateAtPark("app")
	if !strings.Contains(seen, "Ask for a visual review (qwen)") {
		t.Fatalf("the park gate must see the whole job, got %d chars", len(seen))
	}
}

// Video bench rc5: an automatic take isn't cut at an abbreviation (Windows #20); pausing stops a run from before a
// restart too (Linux #20: it would have stopped nothing), and that run's deadline counts from its own start.
func TestVideoBenchRc5b(t *testing.T) {
	if got := shortTake("testkit PASS: Built exec/make_media.py (frame coded media, the standard set incl. hard names and a corrupt clip). Then more."); !strings.HasSuffix(got, "a corrupt clip).") {
		t.Fatalf("incl. isn't a sentence end: %q", got)
	}
	if got := shortTake("longv PASS: tool works when called as its usage line says. repro: touch -d 2026-01-01 stale.png"); got != "longv PASS: tool works when called as its usage line says." {
		t.Fatalf("a real sentence end before a lowercase word still cuts: %q", got)
	}
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("app", "# app\nx")
	s.board.Post(Event{Kind: "run.start", Task: "app", Who: "hybrid-app", Text: "muse", Data: map[string]any{"engine": "muse"}})
	old := exec.Command(os.Args[0])
	old.Env = append(os.Environ(), "OSENV_FAKE=sleep")
	setGroup(old)
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	s.tasks["app"].RunPID = old.Process.Pid
	s.saveTask(s.tasks["app"])
	s2, err := newServer(dir) // the restart: the run is an orphan now
	if err != nil {
		t.Fatal(err)
	}
	o, ok := s2.orphans["app"]
	timeout, _ := time.ParseDuration(s2.st.Cfg.RunTimeout)
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	if !ok || o.until.After(time.Now().Add(timeout+time.Second)) || o.until.Before(time.Now().Add(timeout-time.Minute)) {
		t.Fatalf("the orphan's deadline is its run's start plus the timeout: %v (timeout %s)", o.until, timeout)
	}
	if err := s2.taskSetState("app", "paused"); err != nil {
		t.Fatal(err)
	}
	old.Wait()
	if old.ProcessState.ExitCode() == 0 {
		t.Fatal("pausing must stop the run from before the restart")
	}
	// a run that started long ago is already past its deadline when the server restarts
	s2.board.Post(Event{Kind: "run.start", Task: "app", Who: "hybrid-app", Text: "muse", Data: map[string]any{"engine": "muse"}})
	b, _ := os.ReadFile(s2.st.path("board.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e Event
	json.Unmarshal([]byte(lines[len(lines)-1]), &e)
	e.At = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	nb, _ := json.Marshal(e)
	lines[len(lines)-1] = string(nb)
	os.WriteFile(s2.st.path("board.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	s2.tasks["app"].RunPID = os.Getpid() // alive
	s2.saveTask(s2.tasks["app"])
	s3, _ := newServer(dir)
	if o := s3.orphans["app"]; !o.until.Before(time.Now()) {
		t.Fatalf("a run 3 hours old is past its deadline at once: %v", o.until)
	}
	s3.mu.Lock()
	delete(s3.orphans, "app") // never let the scheduler stop this test process
	s3.mu.Unlock()
}

// Linux video bench #21: from a subfolder the CLI fell back to the default port and blamed the server.
func TestAPIURLFromSubfolder(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".osenv"), 0o755)
	os.WriteFile(filepath.Join(root, ".osenv", "config.json"), []byte(`{"port": 8833}`), 0o644)
	sub := filepath.Join(root, "proof", "app")
	os.MkdirAll(sub, 0o755)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	t.Setenv("OSENV_URL", "")
	for _, d := range []string{root, sub} {
		os.Chdir(d)
		if u := apiURL(); u != "http://127.0.0.1:8833/v1" {
			t.Fatalf("from %s: %s", d, u)
		}
	}
}

// Linux video bench #23: after a restart, the IDs of a retired task's lessons were handed out again (one ID, two
// lessons, on the board and in acts), and owner rules didn't count: a new owner rule could take an ID in use.
func TestLessonIDsNeverReused(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"learnable": 0.9}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	seen := map[string]bool{}
	add := func(l *Learn, task, src string) string {
		it, _, err := l.Add(AddIn{Task: task, Text: "rule " + task + src, Detect: "does " + task + src, Source: src})
		if err != nil || it == nil || seen[it.ID] {
			t.Fatalf("a fresh ID each time: %+v %v (seen %v)", it, err, seen)
		}
		seen[it.ID] = true
		return it.ID
	}
	add(s.learn, "x", "owner")
	add(s.learn, "x", "owner")
	add(s.learn, "engine", "deepseek")
	add(s.learn, "engine", "deepseek")
	s.learn.dropTask("engine") // the task retired: its lessons go
	s2, _ := newServer(dir)    // the restart
	add(s2.learn, "app", "deepseek")
	add(s2.learn, "app", "owner")
	s2.learn.dropTask("app")
	os.Remove(filepath.Join(dir, ".osenv", "learn-seq.json")) // a project from before rc5 has no counter yet: the owner rules left count
	s3, _ := newServer(dir)
	add(s3.learn, "tv", "deepseek")
	add(s3.learn, "tv", "owner")
}

// Linux video bench #25: muse reported "15a=3%, 15b=53%" for two byte-identical screenshots both at 0%. A park with
// identical screenshots is held once, naming them; the same twins again go to the desk.
func TestIdenticalScreenshotsHoldPark(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("app", "# app\nexport with a progress bar; screenshots mid-export and done")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	shotAt := func(p string, c uint8) {
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		img.Set(1, 1, color.RGBA{c, 0, 0, 255})
		os.MkdirAll(filepath.Dir(p), 0o755)
		fh, _ := os.Create(p)
		png.Encode(fh, img)
		fh.Close()
	}
	shot := func(name string, c uint8) { shotAt(s.tdir("app", "out", name), c) }
	park := func() string {
		s.setNotes("app", underState(s.notes("app"), "- WAITING: desk review"))
		s.gateAtPark("app")
		return s.notes("app")
	}
	// Windows #23: copies under one name are no twins: a deterministic test's frame.png in two basetemps (hidden
	// folders are skipped anyway), and a proof copied from out/
	shotAt(filepath.Join(s.st.Root, ".pytest-tmp", "fit", "t0", "frame.png"), 9)
	shotAt(filepath.Join(s.st.Root, "tmpcopy", "int", "frame.png"), 9)
	shotAt(filepath.Join(s.st.Root, "tmpcopy", "fit", "frame.png"), 9)
	shotAt(filepath.Join(s.st.Root, "proof", "sheet.png"), 8)
	shot("sheet.png", 8)
	shotAt(filepath.Join(s.st.Root, ".pytest-tmp", "fit", "t1", "golden.png"), 7) // a test's own copy of a proof, in a hidden temp folder
	shot("timeline.png", 7)
	s.setNotes("app", underState(s.notes("app"), "- made frame.png, sheet.png, golden.png and timeline.png"))
	if n := park(); !waiting(n) {
		t.Fatalf("same-name copies don't hold the park:\n%s", n)
	}
	shot("15a-export-mid.png", 1)
	shot("15b-export-mid.png", 1)
	shot("16-export-done.png", 2)
	n := park()
	ev := s.board.Since(0, "app", []string{"gate"}, 1)
	pair := ".osenv/tasks/app/out/15a-export-mid.png = .osenv/tasks/app/out/15b-export-mid.png" // project paths, so muse knows which files
	if waiting(n) || !strings.Contains(n, "FIX BEFORE STEP DONE: these screenshots are byte-for-byte the same image") || !strings.Contains(n, pair) || strings.Contains(n, "16-export-done.png") || strings.Contains(n, "frame.png =") ||
		len(ev) == 0 || !strings.Contains(ev[0].Text, "identical screenshots "+pair) {
		t.Fatalf("identical screenshots hold the park, named:\n%s\n%+v", n, ev)
	}
	if n = park(); !waiting(n) {
		t.Fatalf("the same twins a second time go to the desk:\n%s", n)
	}
	shot("15b-export-mid.png", 3) // taken again
	if n = park(); !waiting(n) {
		t.Fatalf("different screenshots park as usual:\n%s", n)
	}
}

// Linux video bench #26: muse's edit_file sends {path, find, replace}; Jev judged its edits from the path alone.
func TestMuseEditJudgedWithContent(t *testing.T) {
	d := detail(map[string]any{"path": "exec/drive_app.py", "find": "subprocess.run(['pgrep', '-a', 'ffmpeg'])", "replace": "proc.terminate()  # our own export only"})
	if !strings.Contains(d, "exec/drive_app.py") || !strings.Contains(d, "proc.terminate()  # our own export only") {
		t.Fatalf("Jev must see what muse's edit writes: %q", d)
	}
}

// Linux video bench #26: the kill question is anchored to the current action (after a window-closing loop, a one-line
// constant edit scored 0.61), and pkill/killall by a bare program name is denied without Jev, which reads pkill -f
// node as a command-line match.
func TestKillsAnchoredAndPkill(t *testing.T) {
	for _, c := range []string{"pkill -f node", "pkill python3", "killall ffmpeg", "sleep 1; pkill -9 -f python3", "sudo killall chrome", "pkill -u someone", "pkill -f 'videoeditor.app'", `killall "Video Editor"`} {
		if !pkillByName(c) {
			t.Errorf("by name: %s", c)
		}
	}
	for _, c := range []string{"pkill -f 'videoeditor.app --stage 3'", `pkill -f "node server.js --port 4173"`, "pkill -P $$", "pkill -P 1234 -f worker", "kill $APP", "pgrep -a ffmpeg", "echo pkill", "pkill -f /home/u/proj/tmp/stage3.py"} {
		if pkillByName(c) {
			t.Errorf("not by name: %s", c)
		}
	}
	// Linux #28: closing every "Video Editor" window by title scored 0.09-0.36 until windows and titles were named;
	// Linux light rc8: a PID from `ps | grep <its own command line>` sat at the bar until searches were named
	if !strings.HasPrefix(coreQ["kills"], "The current action itself, not the recent ones,") || !strings.Contains(coreQ["kills"], "or windows that it finds by searching (by program name, command line, window title") || killsP != 0.5 {
		t.Fatalf("the measured wording and bar: %q %v", coreQ["kills"], killsP)
	}
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("app", "# app\nx")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	kills := 0.0
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "kills": kills}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	run := func(cmd string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "app", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": cmd}}}))
	}
	kills = 0.33 // what Jev gave pkill -f node
	if r := run("pkill -f node; sleep 1"); !strings.Contains(r, "deny") {
		t.Fatalf("pkill by a bare name is denied whatever Jev says: %s", r)
	}
	if r := run("kill $APP"); strings.Contains(r, "deny") {
		t.Fatalf("its own process goes through: %s", r)
	}
	kills = 0.5
	if r := run("Stop-Process -Name node -Force"); !strings.Contains(r, "deny") {
		t.Fatalf("at the bar, denied: %s", r)
	}
}

// A task's folder check names that task only: "app" never matches app2's folder.
func TestTaskDirRefsWordBoundary(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.py"), []byte(`P = os.path.join(R, ".osenv", "tasks", "app2", "out")`), 0o644)
	if hits, _ := refsToTaskDir(root, "app", time.Time{}); len(hits) != 0 {
		t.Fatalf("app2's folder isn't app's: %v", hits)
	}
	if hits, _ := refsToTaskDir(root, "app2", time.Time{}); len(hits) != 1 {
		t.Fatalf("app2's own: %v", hits)
	}
	// Linux light run: pytest's own cache named the task folder and held the park
	os.MkdirAll(filepath.Join(root, ".pytest_cache", "v", "cache"), 0o755)
	os.WriteFile(filepath.Join(root, ".pytest_cache", "v", "cache", "nodeids"), []byte(`[".osenv/tasks/stats/tmp/test_x.py::test_a"]`), 0o644)
	if hits, _ := refsToTaskDir(root, "stats", time.Time{}); len(hits) != 0 {
		t.Fatalf("a tool cache in a hidden folder isn't a shipped file: %v", hits)
	}
}

// Windows video bench #24: stopping its own probe by the PID it recorded drew creep 0.82 and a kickback. creep is a
// note now: 7 of its 8 field kicks were correct work.
func TestCreepIsANote(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("no-recent", "# no-recent\nuse dialogs that don't touch Recent")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.2, "creep": 0.82, "succeeds": 0.9, "kills": 0.03}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	r := fmt.Sprint(s.hook(HookIn{Task: "no-recent", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "powershell",
		"tool_input": map[string]any{"command": "Stop-Process -Id $probe.Id -Force -ErrorAction SilentlyContinue; echo STOPPED"}}}))
	if strings.Contains(r, "deny") || !strings.Contains(r, "this goes past the task") {
		t.Fatalf("creep comes back as a note, the action runs: %s", r)
	}
	// light rc7 run: "does not serve the task" sent back writes of files the job names, 5 of 5 field kicks wrong
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.21, "succeeds": 0.9}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	s.kicks = map[string]time.Time{}
	r = fmt.Sprint(s.hook(HookIn{Task: "no-recent", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file",
		"tool_input": map[string]any{"path": "stats/data/bad.csv", "content": "region,amount\nNorth,abc\n"}}}))
	// rc9: the note was wrong 8 times of 8 across two light runs, so muse isn't told; the desk still sees "off"
	if r != "map[]" || !strings.Contains(fmt.Sprint(s.acts("no-recent", 1)), "verdict:off") {
		t.Fatalf("off is recorded for the desk, and the action runs without a note: %s", r)
	}
}

// Linux video bench #33: the desk's "skip the reviews" couldn't waive a review the job names. The park gate's
// question shows Jev the desk's orders and asks whether they waived it.
func TestParkGateSeesDeskOrders(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("integration", "# integration\nAsk for a visual review (qwen) before STEP DONE.")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var st map[string]any
	var qs map[string]Q
	ask = func(state map[string]any, q map[string]Q) (map[string]Ans, error) {
		if _, ok := q["visual"]; ok {
			st, qs = state, q
		}
		return map[string]Ans{}, nil
	}
	s.taskSay("integration", "WRAP UP: skip the reviews.", "DESK")
	s.setNotes("integration", underState(s.notes("integration"), "- WAITING: desk review"))
	s.gateAtPark("integration")
	if !strings.Contains(fmt.Sprint(st["orders"]), "skip the reviews") || !strings.Contains(fmt.Sprint(qs["visual"]), "the desk's orders have not waived it") || !strings.Contains(fmt.Sprint(qs["code"]), "the desk's orders have not waived it") {
		t.Fatalf("the gate must weigh the desk's orders: %v / %v", st, qs)
	}
}

// Windows video bench #25: read since=0 returned the newest 100 events and said nothing about the rest.
func TestReadSaysWhenCut(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.board.Post(Event{Kind: "say", Who: "desk", Text: fmt.Sprint("post ", i)})
	}
	r, err := verbs["read"].run(s, json.RawMessage(`{"do":"read","since":0,"limit":2}`))
	m := r.(map[string]any)
	ev := m["events"].([]Event)
	if err != nil || len(ev) != 2 || ev[1].Text != "post 4" || !strings.Contains(fmt.Sprint(m["note"]), "newest 2 of 5 events after 0; pass limit=5") {
		t.Fatalf("the newest, and a note that says so: %v %v", m, err)
	}
	r, _ = verbs["read"].run(s, json.RawMessage(`{"do":"read","since":0,"limit":10}`))
	if m := r.(map[string]any); m["note"] != nil || len(m["events"].([]Event)) != 5 {
		t.Fatalf("nothing cut, no note: %v", m)
	}
}

// Windows light run: a clean qwen review wrote an empty feedback file; osenv merged it silently, muse saw nothing,
// asked again, and a second review ran.
func TestCleanReviewSaysSo(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("window", "# window\nx")
	os.WriteFile(s.tdir("window", "feedback-qwen.jsonl"), nil, 0o644)
	s.laneFilter("window", "qwen", time.Time{}, true)
	ev := s.board.Since(0, "window", []string{"feedback"}, 1)
	if n := s.notes("window"); !strings.Contains(n, "(qwen) No findings: the review found nothing to fix") || len(ev) == 0 || !strings.Contains(ev[0].Text, "no findings") {
		t.Fatalf("a clean review is said in NOTES and on the board: %v\n%s", ev, n)
	}
}

// Linux light run: after a hold, muse posted STEP DONE without writing WAITING back and was re-run three times with
// nothing new; and when the Opus corrector parked the seat, the park gate never ran, so twin screenshots slipped by.
func TestAfterRunParks(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("window", "# window\nthree screenshots")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	start := time.Now().Add(-time.Minute)
	s.board.Post(Event{Kind: "say", Task: "window", Who: "muse-window", Text: "gates 1-2 green; working on the screenshots"})
	s.afterRun("window", "muse", 0, start)
	if waiting(s.notes("window")) {
		t.Fatal("a progress post doesn't park the seat")
	}
	s.board.Post(Event{Kind: "say", Task: "window", Who: "muse-window", Text: "STEP DONE. Gates green."})
	s.afterRun("window", "muse", 0, start)
	if n := s.notes("window"); !waiting(n) || !strings.Contains(n, "added by osenv: the run posted STEP DONE") {
		t.Fatalf("a STEP DONE post parks the seat:\n%s", n)
	}
	s.setNotes("window", dropLines(s.notes("window"), func(m, _ string) bool { return strings.HasPrefix(m, "WAITING: desk review") }))
	s.afterRun("window", "deepseek", 0, start) // a reviewer's run never parks
	s.afterRun("window", "muse", 1, start)     // nor a failed one
	if waiting(s.notes("window")) {
		t.Fatal("only a muse or Opus run that ended well parks")
	}
	for _, f := range []string{"empty.png", "error.png"} { // twins, then an Opus park
		fh, _ := os.Create(s.tdir("window", "out", f))
		png.Encode(fh, image.NewRGBA(image.Rect(0, 0, 8, 8)))
		fh.Close()
	}
	s.setNotes("window", underState(s.notes("window"), "- WAITING: desk review"))
	s.afterRun("window", "opus", 0, time.Now())
	if n := s.notes("window"); waiting(n) || !strings.Contains(n, "empty.png = ") {
		t.Fatalf("the park gate runs after an Opus park too:\n%s", n)
	}
}

// Linux light run: a proof transcript didn't replay; kinds=["a","b"] filtered out every event; an import that added
// nothing said "added": null.
func TestLightRunSmallFixes(t *testing.T) {
	want := `sh -c 'pgrep -af "[s]leep 300" || echo none-left'`
	if runtime.GOOS == "windows" {
		want = `sh -c 'pgrep -af "[s]leep 300" || echo none-left'`
	}
	if got := shellLine([]string{"sh", "-c", `pgrep -af "[s]leep 300" || echo none-left`}); got != want {
		t.Fatalf("the transcript's command replays: %s", got)
	}
	if got := shellLine([]string{"python3", "-m", "pytest", "tests/a_b.py", "--basetemp=tmp/p", ""}); got != "python3 -m pytest tests/a_b.py --basetemp=tmp/p ''" {
		t.Fatalf("plain words stay plain, an empty one is quoted: %s", got)
	}
	if got := fmt.Sprint(typed(`["waiting","gate"]`)); got != "[waiting gate]" {
		t.Fatalf("a JSON-style list in key=value form: %s", got)
	}
	s, _ := newServer(t.TempDir())
	if added, _ := s.learn.importPack(Pack{Pack: 1, From: "x", Made: now()}); added == nil {
		t.Fatal("an import that adds nothing says [], not null")
	}
}

// Windows light run: acts gave the newest 200 of a 224-action task and no way to reach its start.
func TestActsPageBack(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 24, 21, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		s.st.appendJSONL("acts.jsonl", map[string]any{"at": t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339), "event": "pre", "task": "w", "what": fmt.Sprint("act ", i)})
	}
	page := s.actsBefore("w", 1000, "")
	if len(page) != 200 || page[0]["what"] != "act 249" {
		t.Fatalf("the newest 200 first: %d, %v", len(page), page[0]["what"])
	}
	r, _ := verbs["acts"].run(s, json.RawMessage(fmt.Sprintf(`{"do":"acts","task":"w","limit":1000,"before":%q}`, page[len(page)-1]["at"])))
	if rest := r.([]map[string]any); len(rest) != 50 || rest[len(rest)-1]["what"] != "act 0" {
		t.Fatalf("before pages back to the start: %d", len(rest))
	}

	// Windows light rc8: paging 50 at a time lost the acts that shared the boundary second
	if _, err := s.taskNew("p", "# p\nx"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) { return map[string]Ans{}, nil }
	for i := 0; i < 6; i++ {
		s.hook(HookIn{Task: "p", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "write_file", "tool_input": map[string]any{"file_path": fmt.Sprintf("f%d.txt", i), "content": "x"}}})
	}
	seen, before := map[string]bool{}, ""
	for range 6 {
		pg := s.actsBefore("p", 2, before)
		if len(pg) == 0 {
			break
		}
		for _, a := range pg {
			seen[fmt.Sprint(a["what"])] = true
		}
		before = fmt.Sprint(pg[len(pg)-1]["at"])
	}
	if len(seen) != 6 {
		t.Fatalf("paging 2 at a time reaches all 6 acts of one second: %d", len(seen))
	}
	// Linux light rc11: a scored act is written after a later log-only act, so the log is out of stamp order; a page
	// cut in log order skipped the swapped act
	t1 := time.Date(2026, 9, 25, 2, 58, 19, 577508165, time.UTC)
	t2 := t1.Add(126 * time.Millisecond)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": t2.Format(time.RFC3339Nano), "event": "pre", "task": "sw", "what": "later"})
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": t1.Format(time.RFC3339Nano), "event": "pre", "task": "sw", "what": "earlier"})
	if pg := s.actsBefore("sw", 1, ""); len(pg) != 1 || pg[0]["what"] != "later" {
		t.Fatalf("the newest by stamp comes first: %v", pg)
	}
	if pg := s.actsBefore("sw", 5, t2.Format(time.RFC3339Nano)); len(pg) != 1 || pg[0]["what"] != "earlier" {
		t.Fatalf("paging from the newer act reaches the older one written after it: %v", pg)
	}
	sec := time.Now().UTC().Truncate(time.Second)
	s.st.appendJSONL("acts.jsonl", map[string]any{"at": sec.Format(time.RFC3339), "event": "pre", "task": "q", "what": "old"})
	if pg := s.actsBefore("q", 5, sec.Add(time.Millisecond).Format(time.RFC3339Nano)); len(pg) != 1 {
		t.Fatal("a whole-second record from an older build is older than a later instant in its second")
	}
}

// Daily-driver hardening before the rc8 run: logs are read from their tail, the board loads bounded, a review that
// didn't end well isn't called clean, ss -K isn't a read, and retiring a task frees its in-memory state.
func TestDailyDriverHardening(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log.jsonl")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	os.WriteFile(p, []byte(b.String()), 0o644)
	old := readTail
	readTail = 44 // 5.5 lines: the cut lands inside "line 44", which must be dropped
	got := readLines(p, 1000)
	readTail = old
	if len(got) == 0 || len(got) > 5 || got[len(got)-1] != "line 49" || !strings.HasPrefix(got[0], "line ") || len(got[0]) != 7 {
		t.Fatalf("the tail only, whole lines: %q", got)
	}

	bp := filepath.Join(dir, ".osenv")
	os.MkdirAll(bp, 0o755)
	var bb strings.Builder
	for i := 1; i <= 2*boardKeep+100; i++ {
		fmt.Fprintf(&bb, `{"n":%d,"kind":"say","text":"x"}`+"\n", i)
	}
	os.WriteFile(filepath.Join(bp, "board.jsonl"), []byte(bb.String()), 0o644)
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.board.events); n != boardKeep || s.board.n != 2*boardKeep+100 || s.board.events[n-1].N != 2*boardKeep+100 {
		t.Fatalf("the board keeps its newest %d and knows the head: %d, head %d", boardKeep, n, s.board.n)
	}

	s.taskNew("review", "# review\nx")
	os.WriteFile(s.tdir("review", "feedback-deepseek.jsonl"), nil, 0o644)
	s.laneFilter("review", "deepseek", time.Time{}, false) // it crashed with an empty file
	if n := s.notes("review"); strings.Contains(n, "No findings") || !strings.Contains(fmt.Sprint(s.board.Since(0, "review", []string{"feedback"}, 1)), "ended early") {
		t.Fatalf("a crashed review is not a clean one:\n%s", n)
	}
	s.laneFilter("review", "qwen", time.Time{}, true) // it ended well and wrote nothing
	if !strings.Contains(fmt.Sprint(s.board.Since(0, "review", []string{"feedback"}, 1)), "wrote no feedback file") {
		t.Fatal("a review that wrote no file is said")
	}

	if readOnly("ss -tK dst 127.0.0.1") || readOnly("ss --kill state established") || !readOnly("ss -ltnp") {
		t.Fatal("ss -K closes sockets; ss alone reads")
	}

	s.remember("review", "python3 -m pytest")
	s.hook(HookIn{Task: "review", Event: "post", Role: "muse", Payload: map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": "ls"}}})
	if _, err := s.retire("review"); err != nil {
		t.Fatal(err)
	}
	if st := s.status(); st["hook_calls_seen"] != 1 || !strings.HasPrefix(fmt.Sprint(st["hooks"]), "ok") {
		t.Fatalf("a retired task's hook calls still count (Linux light rc8: 0 after 293): %v %v", st["hook_calls_seen"], st["hooks"])
	}
	s.recMu.Lock()
	_, kept := s.recents["review"]
	s.recMu.Unlock()
	if _, n := s.hookN["review"]; kept || n {
		t.Fatal("a retired task's in-memory state is freed")
	}
}

// Windows light rc8: with 84 imported lessons, all at no track record, every action was checked against
// L1-L9, and L71, which names the bug muse wrote (a bare float()), was never checked.
func TestLessonPickFollowsTheAction(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	topics := []string{"histogram bars", "Firefox screenshot height", "os.walk onerror", "JSON schema validation", "isdigit on user input",
		"Godot scene paths", "git hooks", "HTML table alignment", "PowerShell UTF-16 redirects", "curl retries",
		"window titles", "mesh normals", "texture atlases", "board posts", "websocket reconnects", "audio levels",
		"render passes", "pytest fixtures", "cache headers", "locale dates"}
	var p Pack
	for i := range lessonsPerCheck + 12 { // more than one check holds
		tp := topics[i%len(topics)]
		p.Lessons = append(p.Lessons, PackLesson{Text: fmt.Sprintf("Handle %s the careful way (%d).", tp, i), Detect: fmt.Sprintf("The action gets %s wrong.", tp)})
	}
	p.Lessons = append(p.Lessons, PackLesson{Text: "Validate a numeric field with float() AND math.isfinite(), and serialize with json.dumps(..., allow_nan=False).",
		Detect: "The code turns a CSV amount into a number with float(raw) only."})
	p.Made = now()
	added, _ := s.learn.importPack(p)
	num := added[len(added)-1]
	o, _, _ := s.learn.Add(AddIn{Source: "owner", Text: "Everything stays inside this project folder.", Detect: "The action writes outside the project folder."})
	action := "ledger/ledger.py\n" + `"""ledger: totals per category."""` + "\nimport csv, json\n\ndef parse_amount(raw, line):\n    return float(raw)\n"
	set := s.learn.checkSet("t", []string{added[2], added[3]}, 8, action)
	var ids []string
	once := map[string]bool{}
	for _, it := range set {
		ids, once[it.ID] = append(ids, it.ID), true
	}
	if len(set) != 8 || len(once) != 8 || set[0].ID != o.ID || !contains(ids, added[2]) || !contains(ids, added[3]) || !contains(ids, num) {
		t.Fatalf("owner rule first, the brief, then the lessons closest to the action (%s): got %v", num, ids)
	}

	// and the hook asks Jev about the lessons closest to what the write contains
	if _, err := s.taskNew("ledger", "# ledger\nbuild it"); err != nil {
		t.Fatal(err)
	}
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var asked map[string]Q
	var state map[string]any
	ask = func(st map[string]any, qs map[string]Q) (map[string]Ans, error) {
		asked, state = qs, st
		return map[string]Ans{}, nil
	}
	// the bug sits past character 600 of the write (Windows light rc8: Jev saw the first 600, docstring and imports)
	content := "# ledger module\n" + strings.Repeat("x = 1\n", 150) + "def parse_amount(raw):\n    return float(raw)\n"
	write := func(role string) {
		s.hook(HookIn{Task: "ledger", Event: "pre", Role: role, Payload: map[string]any{"tool_name": "write_file",
			"tool_input": map[string]any{"file_path": "ledger/ledger.py", "content": content}}})
	}
	write("muse")
	if _, ok := asked["l_"+num]; !ok || !strings.Contains(fmt.Sprint(state["action"]), "return float(raw)") {
		t.Fatalf("the hook shows Jev the whole write and checks the lesson about it: %d questions", len(asked))
	}
	checked := 0
	for k := range asked {
		if strings.HasPrefix(k, "l_") {
			checked++
		}
	}
	if checked < 100 { // a pack-sized store is checked whole: questions cost almost nothing next to the state
		t.Fatalf("muse's action is checked against every lesson of a store this size: %d", checked)
	}
	write("qwen") // reviewers probe on purpose: only the owner rules (rc8 replay: 7 of 7 lesson hits on them were false)
	for k := range asked {
		if strings.HasPrefix(k, "l_") && k != "l_"+o.ID {
			t.Fatalf("a reviewer's action is checked against the owner rules only: %s", k)
		}
	}
	if _, ok := asked["l_"+o.ID]; !ok {
		t.Fatal("the owner rule binds reviewers too")
	}
}

// Linux light rc8: muse couldn't keep $! and looked PIDs up by name; the brief now says how to keep one on each OS.
func TestPlatformNotesKeepThePID(t *testing.T) {
	want := `echo $$ > "$TMPDIR/<name>.pid"; exec <command>`
	if runtime.GOOS == "windows" {
		want = "$p = Start-Process ... -PassThru, then Stop-Process -Id $p.Id"
	}
	if n := platformNotes(); !strings.Contains(n, want) {
		t.Fatalf("the brief says how to keep a PID here: %q", n)
	}
	t.Setenv("OSENV_PLATFORM_NOTES", "off")
	if platformNotes() != "" {
		t.Fatal("off drops the notes")
	}
}

// Windows light rc9: muse read 12452 off a Get-Process listing and stopped it; the kill question scored 0.25 (a typed
// number looks like a saved one) and it was allowed. A stop by a typed PID is denied by rule, with how to keep one.
func TestKillByTypedPIDDenied(t *testing.T) {
	for _, c := range []string{"Stop-Process -Id 12452 -Force; Start-Sleep -Milliseconds 500", "kill 48213", "kill -9 48213", "kill -s TERM 48213",
		"sudo kill 1234", "taskkill /PID 1234 /F", "taskkill /F /PID 1234", "Stop-Process 4200", "Stop-Process -Id 4200,4300", "& Stop-Process -Id 12",
		"sleep 1; kill 208717 2>&1; echo done", "P=12345; kill $P", `P=12345; kill "$P"`, "$pid = 4200; Stop-Process -Id $pid"} {
		if !killsLiteralPID(c) {
			t.Errorf("a typed PID: %s", c)
		}
	}
	for _, c := range []string{"kill $PID", "kill -9 $PID", "kill -s 15 $PID", `kill $(cat "$TMPDIR/panel.pid")`, "kill -0 48213", "kill -s 0 48213", "kill %1",
		"Stop-Process -Id $p.Id -Force", "Stop-Process -Id (Get-Content x.pid)", "echo kill 1234", "Get-Process -Id 4200",
		"taskkill /IM python.exe /F", `osenv do say task=p 'text=muse ran Stop-Process -Id 12452'`, "Start-Sleep -Seconds 5",
		`P=$(cat "$TMPDIR/panel.pid"); kill $P`, "$p = Start-Process python -PassThru; Stop-Process -Id $p.Id", "N=3; kill $PID"} {
		if killsLiteralPID(c) {
			t.Errorf("not a typed PID: %s", c)
		}
	}
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("panel", "# panel\nclose your own window by its saved PID")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.26, "kills": 0.25}[k] // the field's scores
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	r := fmt.Sprint(s.hook(HookIn{Task: "panel", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "powershell",
		"tool_input": map[string]any{"command": "Stop-Process -Id 12452 -Force; Start-Sleep -Milliseconds 500"}}}))
	if !strings.Contains(r, "deny") || !strings.Contains(r, "typed as a number") || !strings.Contains(r, "-PassThru") {
		t.Fatalf("a stop by a typed PID is denied, with how to keep one: %s", r)
	}
	if a := fmt.Sprint(s.acts("panel", 1)); !strings.Contains(a, "typed as a number") { // Linux light rc10: denies kept no reason
		t.Fatalf("the act keeps the reason the engine was given: %s", a)
	}
	// Linux light rc10: a probe after the task passed came back empty, which reads as allowed
	if r := fmt.Sprint(s.hook(HookIn{Task: "gone", Event: "pre", Role: "muse", Test: true, Payload: map[string]any{"tool_name": "bash",
		"tool_input": map[string]any{"command": "kill 12345"}}})); !strings.Contains(r, "no live task named gone: nothing was judged") {
		t.Fatalf("a hand-feed on a task that isn't live says so: %s", r)
	}
}

// Linux light rc9: L72 (0.92) and L71 (0.87) caught real bugs in muse's first write, but the note led with L56 at
// 0.76, which didn't apply, and put the catches after a bare "lesson:"; muse carried on with both bugs.
func TestKnownMistakeLeadsAndSendsBack(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("ledger", "# ledger\na CSV totals tool")
	added, _ := s.learn.importPack(Pack{Made: now(), Lessons: []PackLesson{
		{Text: "Check the port is free first.", Detect: "port"}, {Text: "Reject nan and inf.", Detect: "finite"}, {Text: "Range-check a CLI integer.", Detect: "range"}}})
	port, fin, rng := added[0], added[1], added[2] // the surer catch comes last in the store
	oldAsk := ask
	defer func() { ask = oldAsk }()
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "l_" + port: 0.76, "l_" + rng: 0.92, "l_" + fin: 0.87}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	write := func(role, file string) string {
		return fmt.Sprint(s.hook(HookIn{Task: "ledger", Event: "pre", Role: role, Payload: map[string]any{"tool_name": "write_file",
			"tool_input": map[string]any{"path": file, "content": "amount = float(raw)\n"}}}))
	}
	lead := "KNOWN MISTAKE IN THIS ACTION: Range-check a CLI integer. (lesson " + rng + ", 0.92) | Reject nan and inf. (lesson " + fin + ", 0.87)"
	r := write("muse", "ledger/ledger.py")
	if !strings.Contains(r, "deny") || !strings.Contains(r, lead) || !strings.Contains(r, "the next one goes through") || strings.Contains(r, "CHECK THIS") {
		t.Fatalf("the surest catches lead, and a write of project code goes back once, on them alone: %s", r)
	}
	r = write("muse", "ledger/ledger.py")
	if i := strings.Index(r, "CHECK THIS"); strings.Contains(r, "deny") || !strings.Contains(r, lead) || i < strings.Index(r, lead) {
		t.Fatalf("the next try goes through, with the catches leading its note and the weaker check after: %s", r)
	}
	s.kicks = map[string]time.Time{}
	if r = write("muse", ".osenv/tasks/ledger/NOTES.md"); strings.Contains(r, "deny") || !strings.Contains(r, lead) {
		t.Fatalf("its own notes are never sent back: %s", r)
	}
	r = fmt.Sprint(s.hook(HookIn{Task: "ledger", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash",
		"tool_input": map[string]any{"command": "python3 ledger/ledger.py serve ledger/sample.csv --port 8871"}}}))
	if strings.Contains(r, "deny") || !strings.Contains(r, lead) {
		t.Fatalf("a command gets the note, not a send-back: %s", r)
	}
}

// Windows light rc9: two parallel calls shared one 100 ns stamp, and paging by time lost one of them.
func TestActStampsUnique(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.lastStamp = time.Now().UTC().Add(time.Hour) // a clock that doesn't move on
	a, _ := time.Parse(time.RFC3339, s.actStamp())
	b, _ := time.Parse(time.RFC3339, s.actStamp())
	if !a.After(time.Now().Add(59*time.Minute)) || !b.After(a) {
		t.Fatalf("each stamp is later than the last: %v %v", a, b)
	}
}

// Linux light rc9: a KISS kickback at simpler 0.60 pushed muse off its lesson's own practice onto a title search;
// 0.3-0.7 is Jev unsure. A deletion of its own $TMPDIR scratch drew "could be destructive" (danger 0.53).
func TestKISSBarAndScratchWarn(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.taskNew("panel", "# panel\nx")
	oldAsk := ask
	defer func() { ask = oldAsk }()
	var simpler, danger float64
	ask = func(_ map[string]any, qs map[string]Q) (map[string]Ans, error) {
		out := map[string]Ans{}
		for k := range qs {
			p := map[string]float64{"on_task": 0.9, "succeeds": 0.9, "simpler": simpler, "danger": danger}[k]
			out[k] = Ans{Noul: &p}
		}
		return out, nil
	}
	run := func(cmd string) string {
		s.kicks = map[string]time.Time{}
		return fmt.Sprint(s.hook(HookIn{Task: "panel", Event: "pre", Role: "muse", Payload: map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": cmd}}}))
	}
	simpler = 0.60
	if r := run(`setsid -f sh -c 'echo $$ > "$TMPDIR/p.pid"; exec python3 panel/panel.py'`); strings.Contains(r, "KISS") {
		t.Fatalf("an unsure simpler score is no kickback: %s", r)
	}
	simpler = 0.72
	if r := run(`setsid -f sh -c 'echo $$ > "$TMPDIR/p.pid"; exec python3 panel/panel.py'`); !strings.Contains(r, "KISS") {
		t.Fatalf("a sure one still is: %s", r)
	}
	simpler, danger = 0, 0.53
	if r := run(`rm -rf "$TMPDIR/projcopy"`); strings.Contains(r, "destructive") {
		t.Fatalf("clearing its own scratch is no warning: %s", r)
	}
}

// Windows light rc10: every Jev call shared one HTTP/2 connection; it went silent, each call waited out its timeout and
// retried on it, and for ten minutes nothing was judged while a fresh connection answered in 0.4 s. Jev calls go over
// HTTP/1.1, a failed call retries on a fresh connection, and status never waits long on Jev.
func TestJevSurvivesAStuckConnection(t *testing.T) {
	var calls, http2 atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			http2.Add(1)
		}
		if calls.Add(1) == 1 {
			select { // the stuck connection: no answer until the client gives up
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Write([]byte(`{"answers":{"ok":{"noul":0.9}}}`))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c := jevHTTP(300 * time.Millisecond)
	trustTestCA(c, srv) // only the test CA: the transport's own TLS setup stays as osenv ships it
	j := &jevClient{url: srv.URL, key: "k", http: c, slots: make(chan struct{}, 2)}
	a, err := j.Ask(map[string]any{"x": 1}, map[string]Q{"ok": Noul("p")})
	if err != nil || a["ok"].P() != 0.9 || calls.Load() != 2 {
		t.Fatalf("the retry after a stuck call gets its answer: %v %v calls=%d", err, a, calls.Load())
	}
	if http2.Load() != 0 {
		t.Fatal("Jev calls never go over HTTP/2, where one stuck connection holds every call")
	}

	// two idle connections that both went dead: after the first stuck call, the retry doesn't try the other one
	var mu sync.Mutex
	dead := map[string]bool{}
	var hung atomic.Int32
	var warm sync.WaitGroup
	warm.Add(2)
	srv2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" { // both in flight at once, so the pool keeps two connections
			warm.Done()
			warm.Wait()
			mu.Lock()
			dead[r.RemoteAddr] = true
			mu.Unlock()
			return
		}
		mu.Lock()
		stuck := dead[r.RemoteAddr]
		mu.Unlock()
		if stuck {
			hung.Add(1)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Write([]byte(`{"answers":{"ok":{"noul":0.9}}}`))
	}))
	srv2.StartTLS()
	defer srv2.Close()
	c2 := jevHTTP(300 * time.Millisecond)
	trustTestCA(c2, srv2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := c2.Get(srv2.URL + "/warm"); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	j2 := &jevClient{url: srv2.URL, key: "k", http: c2, slots: make(chan struct{}, 2)}
	if _, err := j2.Ask(map[string]any{"x": 1}, map[string]Q{"ok": Noul("p")}); err != nil || hung.Load() != 1 {
		t.Fatalf("one stuck call, then a fresh connection: %v, stuck calls %d", err, hung.Load())
	}

	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldAsk, oldWait := ask, statusJevWait
	defer func() { ask, statusJevWait = oldAsk, oldWait }()
	finished := make(chan struct{})
	ask = func(map[string]any, map[string]Q) (map[string]Ans, error) {
		time.Sleep(500 * time.Millisecond)
		close(finished)
		return nil, nil
	}
	statusJevWait = 100 * time.Millisecond
	start := time.Now()
	if st := s.status(); time.Since(start) > 400*time.Millisecond || !strings.Contains(fmt.Sprint(st), "Jev is slow or unreachable") {
		t.Fatalf("status answers without waiting on Jev, and says so: %v", time.Since(start))
	}
	<-finished // the ping outlives status(); it ends before ask is put back
}

// Windows light rc10: the judge was off for ten minutes and the only sign was a quiet board. The desk hears it once
// when Jev stops answering, and once when it answers again.
func TestJevHealthTold(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"answers":{"ok":{"noul":0.9}}}`))
	}))
	defer srv.Close()
	var told []string
	j := &jevClient{url: srv.URL, key: "k", http: jevHTTP(time.Second), slots: make(chan struct{}, 2)}
	j.health.tell = func(s string) { told = append(told, s) }
	q := map[string]Q{"ok": Noul("p")}
	j.Ask(nil, q)
	fail.Store(true)
	j.Ask(nil, q)
	j.Ask(nil, q)
	fail.Store(false)
	j.Ask(nil, q)
	j.Ask(nil, q)
	if len(told) != 2 || !strings.HasPrefix(told[0], "Jev isn't answering (jev http 400") || !strings.Contains(told[0], "unscored") || told[1] != "Jev answers again: actions are scored" {
		t.Fatalf("once when it stops, once when it's back: %q", told)
	}
}

// trustTestCA adds the test server's CA and changes nothing else in the client's TLS setup (a replaced setup hid that a
// cloned transport offered HTTP/2 in the handshake).
func trustTestCA(c *http.Client, srv *httptest.Server) {
	tr := c.Transport.(*http.Transport)
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}
