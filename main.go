// osenv: the jev-hybrid as one binary. Point your Claude at SKILL.md; it becomes the desk
// and steers hybrids through one route, POST /v1 {"do": ...}.
//
//	osenv serve              run the board, the supervisor and the Jev judge (in the project root)
//	osenv do '<json>'        one API call, e.g. osenv do '{"do":"help"}'
//	osenv view <file> [--q "question"] [--crop x,y,w,h] [--task x]   a parsed view instead of the raw file
//	osenv hook pre|post      internal: the engines' hooks call this
//	osenv sim loop|learn     simulate the loop (no tokens) or Jev's learned-context reasoning (real Jev)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const version = "0.3.0"

const usage = "usage: osenv serve [--root DIR] | do <verb> key=value... | do '<json>' | view <file> [--q ...] | proof run|http|shot|mutate|check ... | hook pre|post | sim loop|learn | version"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Println(usage + "\n`osenv do help` lists every API verb; `osenv proof --help` the proof commands.")
	case "serve":
		serve(os.Args[2:])
	case "do":
		do(os.Args[2:])
	case "hook":
		hookClient(os.Args[2:])
	case "view":
		viewClient(os.Args[2:])
	case "proof":
		proofClient(os.Args[2:])
	case "sim":
		sim(os.Args[2:])
	case "version":
		fmt.Println("osenv", version)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func newServer(root string) (*Server, error) {
	st, err := openStore(root)
	if err != nil {
		return nil, err
	}
	jev.configure(st.Cfg)
	bin, _ := os.Executable()
	bin, _ = filepath.EvalSymlinks(bin)
	s := &Server{st: st, board: openBoard(st), learn: openLearn(st), takes: openTakes(st), bin: bin, cli: cmdPath(bin),
		url:   fmt.Sprintf("http://127.0.0.1:%d/v1", st.Cfg.Port),
		tasks: map[string]*Task{}, live: map[string]*exec.Cmd{}, kicks: map[string]time.Time{}, hookN: map[string]int{},
		recents: map[string][]string{}, orphans: map[string]orphan{}, held: map[string]string{}}
	s.loadTasks()
	timeout, _ := time.ParseDuration(st.Cfg.RunTimeout)
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	for _, t := range s.tasks {
		if t.RunPID == 0 {
			continue
		}
		if procAlive(t.RunPID) {
			until := time.Now().Add(timeout)
			if ev := s.board.Since(0, t.Name, []string{"run.start"}, 1); len(ev) == 1 { // the run's own deadline, as its server set it
				if at, err := time.Parse(time.RFC3339, ev[0].At); err == nil && at.Add(timeout).Before(until) {
					until = at.Add(timeout) // Linux video bench #20: 68 minutes on a 60-minute timeout
				}
			}
			s.orphans[t.Name] = orphan{pid: t.RunPID, until: until}
			s.board.Post(Event{Kind: "orphan", Task: t.Name, Who: "osenv", Text: fmt.Sprintf("osenv restarted while this task's engine (pid %d) was still running; its next run waits until that one ends", t.RunPID)})
		} else {
			t.RunPID = 0
			s.saveTask(t)
		}
	}
	for _, t := range s.tasks { // a run the last server stopped or lost without recording its end: close it now
		if _, waitFor := s.orphans[t.Name]; !waitFor {
			s.undoCloseOpen(t.Name)
		}
	}
	return s, nil
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", ".", "the project root (every engine runs here)")
	fs.Parse(args)
	s, err := newServer(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv:", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.st.Cfg.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv: port", s.st.Cfg.Port, "is taken (is osenv already serving? set port in .osenv/config.json):", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		s.mu.Lock()
		for _, c := range s.live {
			killGroup(c)
		}
		s.mu.Unlock()
		time.Sleep(time.Second)
		os.Exit(0)
	}()
	jev.health.tell = func(text string) { s.board.Post(Event{Kind: "say", Who: "osenv", Text: text}) }
	go s.supervise()
	fmt.Printf("osenv %s serving %s on %s (%d live tasks)\n", version, s.st.Root, s.url, len(s.tasks))
	http.Serve(ln, s)
}

func apiURL() string {
	if u := os.Getenv("OSENV_URL"); u != "" {
		return u
	}
	port := defaultConfig().Port
	for d, _ := os.Getwd(); d != ""; { // the project's config, from any folder inside it (Linux video bench #21)
		var c Config
		if readJSON(filepath.Join(d, ".osenv", "config.json"), &c) == nil && c.Port != 0 {
			port = c.Port
			break
		}
		if up := filepath.Dir(d); up != d {
			d = up
		} else {
			break
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

func post(url string, body []byte, timeout time.Duration) ([]byte, error) {
	cl := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if role := os.Getenv("OSENV_ROLE"); role != "" { // an engine run: its responses carry no desk message
		req.Header.Set("X-Osenv-Role", role)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// sayWho: a board post from inside a run names the engine that wrote it (Linux run: deepseek's and qwen's
// posts all read as the seat's).
func sayWho(body []byte, role string) []byte {
	var m map[string]any
	if role == "" || role == "muse" || json.Unmarshal(body, &m) != nil || m["do"] != "say" || m["who"] != nil {
		return body
	}
	t, _ := m["task"].(string)
	m["who"] = role + "-" + t
	b, _ := json.Marshal(m)
	return b
}

// do: one API call from a shell. `osenv do '{"do":"help"}'`, or JSON on stdin with `osenv do -`.
func do(args []string) {
	var body []byte
	switch {
	case len(args) == 0 || args[0] == "-":
		body, _ = io.ReadAll(os.Stdin)
	case strings.HasPrefix(strings.TrimSpace(args[0]), "{"):
		body = []byte(strings.Join(args, " "))
	default: // verb key=value ...: no JSON to quote (PowerShell mangles embedded quotes)
		m := map[string]any{"do": args[0]}
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok { // Windows video bench #13: PowerShell 5.1 splits a value with double quotes inside it into pieces
				fmt.Fprintln(os.Stderr, "osenv do: expected key=value, got", kv, "(a value with double quotes inside breaks into pieces in PowerShell 5.1: write it to a file and pass key=@file)")
				os.Exit(2)
			}
			m[k] = typed(v)
		}
		body, _ = json.Marshal(m)
	}
	body = sayWho(body, os.Getenv("OSENV_ROLE"))
	out, err := post(apiURL(), body, 3700*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv: is `osenv serve` running here?", err)
		os.Exit(1)
	}
	var v any
	if json.Unmarshal(out, &v) == nil {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Println(string(out))
}

// hookClient forwards an engine's hook payload to the server and prints its decision.
// Anything wrong (server down, timeout, bad input) means: say nothing, let the action run.
func hookClient(args []string) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	task := fs.String("task", os.Getenv("OSENV_TASK"), "")
	role := fs.String("role", os.Getenv("OSENV_ROLE"), "")
	url := fs.String("url", "", "")
	test := fs.Bool("test", false, "a hand-fed call: Jev scores it and you get the answer, but no lesson record, seat history or kickback window changes")
	ev := "pre"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ev, args = args[0], args[1:]
	}
	fs.Parse(args)
	if *url != "" {
		os.Setenv("OSENV_URL", *url)
	}
	payload, _ := io.ReadAll(bufio.NewReader(os.Stdin))
	var p map[string]any
	if json.Unmarshal(plainText(payload), &p) != nil {
		fmt.Fprintf(os.Stderr, "osenv hook: stdin is not a JSON hook payload (%d bytes); nothing was scored\n", len(payload))
		return // fail open: the zero-hooks tripwire on the server catches a hook that never gets through
	}
	body, _ := json.Marshal(map[string]any{"do": "hook", "task": *task, "role": *role, "event": ev, "payload": p, "test": *test})
	out, err := post(apiURL(), body, 55*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv hook: can't reach osenv at", apiURL(), "- nothing was scored:", err)
		return
	}
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &r) == nil && len(r.Result) > 0 && string(r.Result) != "null" {
		fmt.Println(string(r.Result))
	}
}

// readLines returns the last n lines of a file.
// readTail: how much of a log's end readLines reads. As a daily driver the acts log grows by megabytes a day (1.4 MB
// in a 4-hour bench), and every park, undo and audit read all of it; 16 MB is days of heavy use.
var readTail int64 = 16 << 20

// readLines: the last n lines of a file, from at most its last readTail bytes.
func readLines(p string, n int) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(st.Size()-readTail, 0)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil
	}
	if off > 0 { // the first line is cut: drop it
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	l := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return l
}

// viewClient prints a parsed view (plain text, ready for an agent to read).
func viewClient(args []string) {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	q := fs.String("q", "", "what you need from the file: Jev picks the parts that answer it")
	crop := fs.String("crop", "", "images: x,y,w,h in pixels or fractions of the original")
	task := fs.String("task", os.Getenv("OSENV_TASK"), "the task (views are saved in its folder)")
	pos := parseAnyOrder(fs, args)
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: osenv view <file> [--q question] [--crop x,y,w,h] [--task name]")
		os.Exit(2)
	}
	p := pos[0]
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	body, _ := json.Marshal(map[string]any{"do": "view", "task": *task, "path": p, "q": *q, "crop": *crop})
	out, err := post(apiURL(), body, 120*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv: is `osenv serve` running?", err)
		os.Exit(1)
	}
	var r struct {
		OK          bool   `json:"ok"`
		Result      string `json:"result"`
		Error       string `json:"error"`
		TakeBuckets string `json:"take_buckets"`
	}
	if json.Unmarshal(out, &r) != nil || !r.OK {
		fmt.Fprintln(os.Stderr, "osenv view:", r.Error)
		os.Exit(1)
	}
	fmt.Print(r.Result)
	if !strings.HasSuffix(r.Result, "\n") { // Linux run: the next prompt landed on the last line
		fmt.Println()
	}
	if r.TakeBuckets != "" {
		fmt.Println("\ntake_buckets: " + r.TakeBuckets)
	}
}

// typed turns a key=value value into JSON: @file reads the file, true/false/numbers stay typed,
// a,b,c inside [ ] becomes a list.
func typed(v string) any {
	switch {
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(v[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "osenv do:", err)
			os.Exit(2)
		}
		// PowerShell 5.1's Set-Content -Encoding utf8 writes a BOM (Windows video bench #14) and a closing CRLF, which an
		// owner rule kept in its text and detect (Windows light rc8)
		return strings.TrimRight(strings.TrimPrefix(string(b), "\ufeff"), "\r\n")
	case v == "true" || v == "false":
		return v == "true"
	case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
		var out []string
		for _, x := range strings.Split(strings.Trim(v, "[]"), ",") {
			if x = strings.Trim(strings.TrimSpace(x), `"'`); x != "" { // kinds=["waiting","gate"] too (Linux light run: the quotes filtered out every event)
				out = append(out, x)
			}
		}
		return out
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return v
}

// plainText drops a UTF-8 byte-order mark and decodes UTF-16 (what PowerShell 5.1's > writes),
// so a payload piped in by hand on Windows still parses.
func plainText(b []byte) []byte {
	switch {
	case len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff):
		u := make([]uint16, (len(b)-2)/2)
		for i := range u {
			if b[0] == 0xff {
				u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
			} else {
				u[i] = uint16(b[2+2*i])<<8 | uint16(b[3+2*i])
			}
		}
		return []byte(string(utf16.Decode(u)))
	}
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
}

// parseAnyOrder parses flags before or after the positional arguments and returns the positionals.
// Go's flag package stops at the first plain argument, so "view <file> --q ..." used to fail even though
// every doc and hint wrote it that way (Linux run: it cost qwen about 4 minutes and caused a kickback).
func parseAnyOrder(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return pos
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}
