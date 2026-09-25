package main

// store.go: the project's .osenv/ folder, the config, and the NOTES.md markers.
// Everything is plain files (JSON and JSONL) so a human or an agent can read it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Help       map[string]string `json:"_help,omitempty"` // what each setting means, written into the file itself
	Project    string            `json:"project"`         // shown to every model as the task label
	Port       int               `json:"port"`            // loopback only
	Parallel   int               `json:"parallel"`        // hybrids running at once
	Cadence    int               `json:"cadence"`         // every Nth muse run gets an Opus review
	OpusCap    int               `json:"opus_per_day"`    // 0 = no cap
	RunTimeout string            `json:"run_timeout"`     // one engine run, e.g. "45m"
	Turns      int               `json:"max_turns"`       // per engine run
	Kickback   bool              `json:"kickback"`        // Jev sends a doubtful action back once
	SafeRuns   []string          `json:"safe_runs"`       // commands containing these are tests: never kicked back
	Muse       struct {
		Cmd   string `json:"cmd"`
		Model string `json:"model"`
		Auth  string `json:"auth"` // muse's auth.json, linked into each seat
	} `json:"muse"`
	Plan struct { // one OpenAI-compatible endpoint serves deepseek and qwen
		URL      string `json:"url"`
		KeyFile  string `json:"key_file"`
		Deepseek string `json:"deepseek"`
		Qwen     string `json:"qwen"`
		Cmd      string `json:"cmd"`
	} `json:"plan"`
	Claude struct {
		Cmd    string `json:"cmd"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	} `json:"claude"`
	Jev struct {
		URL     string `json:"url"`
		Model   string `json:"model"`
		KeyFile string `json:"key_file"`
	} `json:"jev"`
}

func defaultConfig() Config {
	var c Config
	c.Help = map[string]string{
		"project":      "a short name for the project, shown to every model",
		"port":         "the loopback port osenv serves on (127.0.0.1 only)",
		"parallel":     "how many hybrids may run at once",
		"cadence":      "every Nth muse run gets an Opus review (0 turns the review off)",
		"opus_per_day": "the most Opus corrector runs per task per day. 0 means NO CAP, not zero runs; Opus still reviews, fixes desk FAILs and escalations",
		"run_timeout":  "the longest one engine run may take, e.g. 45m",
		"max_turns":    "the turn limit handed to each engine run",
		"kickback":     "true: Jev sends a doubtful action back once before letting it through",
		"safe_runs":    "command fragments that mark a test run; those are never kicked back",
		"muse":         "the default worker: cmd, model, and auth (its login file; a relative path is inside .osenv/)",
		"plan":         "one OpenAI-compatible endpoint that serves deepseek and qwen: url, key_file (relative = inside .osenv/), the two model names, and cmd (the qwen CLI)",
		"claude":       "the Opus corrector: cmd, model, effort",
		"jev":          "the judge (TypeSafe System One): url, model, key_file (relative = inside .osenv/)",
	}
	c.Project, c.Port, c.Parallel, c.Cadence = "project", 8811, 3, 6
	c.RunTimeout, c.Turns, c.Kickback = "45m", 120, true
	c.SafeRuns = []string{"--test=", "go test", "pytest", "npm test", "cargo test"}
	c.Muse.Cmd, c.Muse.Model, c.Muse.Auth = "muse", "muse-spark-1.3-contributor", "~/.config/muse/auth.json"
	c.Plan.URL = "https://token-plan.maas.qwencloudapi.com/compatible-mode/v1"
	c.Plan.KeyFile, c.Plan.Deepseek, c.Plan.Qwen, c.Plan.Cmd = "~/.config/osenv/plan.key", "deepseek-v4.1-flash", "qwen3.8-max", "qwen"
	c.Claude.Cmd, c.Claude.Model, c.Claude.Effort = "claude", "claude-opus-5-5", "max"
	c.Jev.URL, c.Jev.Model, c.Jev.KeyFile = "https://api.typesafe.ai/v1/systemone", "jev-latest", "~/.config/osenv/jev.key"
	return c
}

// Store is one project's .osenv/ folder.
type Store struct {
	Root string // the project root; every engine runs with this as its cwd
	Dir  string // Root/.osenv
	Cfg  Config
	mu   sync.Mutex // guards JSONL appends
}

func openStore(root string) (*Store, error) {
	root, _ = filepath.Abs(root)
	s := &Store{Root: root, Dir: filepath.Join(root, ".osenv"), Cfg: defaultConfig()}
	for _, d := range []string{"tasks", "done"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	p := filepath.Join(s.Dir, "config.json")
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &s.Cfg); err != nil {
			return nil, err
		}
	} else if err := writeJSON(p, s.Cfg); err != nil {
		return nil, err
	}
	// a key file named without ~ or a full path lives in .osenv/ (so a project can carry its own keys)
	for _, kp := range []*string{&s.Cfg.Jev.KeyFile, &s.Cfg.Plan.KeyFile, &s.Cfg.Muse.Auth} {
		if *kp != "" && !strings.HasPrefix(*kp, "~") && !filepath.IsAbs(*kp) {
			*kp = filepath.Join(s.Dir, *kp)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "PROJECT.md")); os.IsNotExist(err) {
		os.WriteFile(filepath.Join(s.Dir, "PROJECT.md"), []byte(projectTemplate), 0o644)
	}
	return s, nil
}

const projectTemplate = `# What this project is (every hybrid reads this; edit it)

One paragraph: what the project is, what "good" looks like, the owner's standing calls,
and anything no agent may touch.
`

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.Dir}, parts...)...)
}

func (s *Store) appendJSONL(name string, v any) {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// writeJSON replaces a file atomically, so a crash never leaves half a config or task.
func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func home(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// readKey takes the env var first, then the file. A file may hold the bare key or NAME=key lines.
func readKey(env, file string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	b, err := os.ReadFile(home(file))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if i := strings.Index(l, "="); i >= 0 {
			return strings.TrimSpace(l[i+1:])
		}
		return l
	}
	return ""
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// ---- NOTES.md markers ------------------------------------------------------------------
// muse writes its own notes; these read them the way it actually writes them
// (bulleted or bare, anywhere in the file). Lessons paid for on 09-23:
// bullets hid the asks, answered asks re-fired, and quoted phrases parked seats early.

func mark(line string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*• "))
}

// asks are the open ESCALATE lines: a line that starts with ESCALATE:, not answered or resolved.
func asks(notes string) []string {
	var out []string
	for _, l := range strings.Split(notes, "\n") {
		m := mark(l)
		u := strings.ToUpper(l)
		if strings.HasPrefix(m, "ESCALATE:") && !strings.Contains(u, "ANSWERED") && !strings.Contains(u, "RESOLVED") {
			out = append(out, m)
		}
	}
	return out
}

// waiting: the seat parked itself for the desk, and it has no open ask (an ask runs first).
func waiting(notes string) bool {
	for _, l := range strings.Split(notes, "\n") {
		if strings.HasPrefix(mark(l), "WAITING: desk review") {
			return len(asks(notes)) == 0
		}
	}
	return false
}

// stateBlock is the section under the STATE heading: the seat's standing orders.
func stateBlock(notes string) string {
	lines := strings.Split(notes, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") && strings.HasPrefix(strings.TrimSpace(strings.TrimLeft(l, "#")), "STATE") {
			var body []string
			for _, b := range lines[i+1:] {
				if strings.HasPrefix(b, "#") {
					break
				}
				body = append(body, b)
			}
			return strings.TrimSpace(strings.Join(body, "\n"))
		}
	}
	return ""
}

// orders is what Jev judges: the open asks first, then STATE. A long FEEDBACK section
// above STATE once pushed an ask past the cut and it never routed.
func orders(notes string) string {
	s := strings.Join(asks(notes), "\n") + "\n# STATE\n" + stateBlock(notes)
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}

// underState inserts a line right below the STATE heading (the desk's orders land on top).
func underState(notes, line string) string {
	lines := strings.Split(notes, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") && strings.HasPrefix(strings.TrimSpace(strings.TrimLeft(l, "#")), "STATE") {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, line)
			return strings.Join(append(out, lines[i+1:]...), "\n")
		}
	}
	return "## STATE\n" + line + "\n" + notes
}

// dropLines removes every line whose marker satisfies drop.
func dropLines(notes string, drop func(m, raw string) bool) string {
	var out []string
	for _, l := range strings.Split(notes, "\n") {
		if !drop(mark(l), l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// sectionAfterState places a block after the STATE section, never above it.
func sectionAfterState(notes, block string) string {
	lines := strings.Split(notes, "\n")
	in := false
	for i, l := range lines {
		if strings.HasPrefix(l, "#") {
			if in {
				out := append([]string{}, lines[:i]...)
				out = append(out, block)
				return strings.Join(append(out, lines[i:]...), "\n")
			}
			in = strings.HasPrefix(strings.TrimSpace(strings.TrimLeft(l, "#")), "STATE")
		}
	}
	return strings.TrimRight(notes, "\n") + "\n\n" + block + "\n"
}
