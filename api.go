package main

// api.go: the whole surface is one route. POST /v1 {"do": "<verb>", ...}. "help" lists every
// verb; "batch" runs several in one call. Loopback only: whoever can reach it is the desk.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Server struct {
	st    *Store
	board *Board
	learn *Learn
	takes *Takes
	bin   string // this binary, which the engines' hooks call back into
	cli   string // bin as an engine's shell must call it (cmdPath)
	url   string

	mu        sync.Mutex
	tasks     map[string]*Task
	live      map[string]*exec.Cmd
	kicks     map[string]time.Time
	hookN     map[string]int // hook calls seen per task: a long run with none means the hooks are broken
	hookCalls int            // every engine hook call since serve started (hookN forgets a task when it retires)
	// orphans: engine runs still alive from before a restart (they run in their own process group, so they
	// outlive the server). Soak test: without this, a restart put a second engine on the same hybrid.
	orphans map[string]orphan
	held    map[string]string // the files the last task-folder hold named, per task: the same hold twice parks for the desk

	recMu   sync.Mutex
	recents map[string][]string

	stampMu   sync.Mutex
	lastStamp time.Time // acts get unique, increasing stamps (Windows light rc9: two parallel calls shared one 100 ns stamp)
}

type orphan struct {
	pid    int
	until  time.Time
	ending bool // it ended, and its wrap-up (undo record, review merge) is running
}

type verb struct {
	usage string
	run   func(s *Server, r json.RawMessage) (any, error)
}

func arg[T any](r json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(r, &v)
	return v, err
}

var verbs = map[string]verb{
	"status": {`{"do":"status"} - engines, keys, Jev, tasks and the board head`, func(s *Server, _ json.RawMessage) (any, error) {
		return s.status(), nil
	}},
	"task.new": {`{"do":"task.new","name":"hero-body","job":"<goal, gates, the proof to show>","tags":"<optional: project ID>,<bucket>,...","takes":5} - starts a fresh hybrid (hybrid-<name>); with tags it starts with up to takes (default 5) memories Jev picks from the project's takes in those buckets`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				Name, Job, Tags string
				Takes           *int
			}](r)
			if err != nil {
				return nil, err
			}
			var tags, picked []string
			if a.Tags != "" { // the desk asked for memories: Jev picks them before the hybrid's first run
				if tags, err = parseTags(a.Tags); err != nil {
					return nil, err
				}
				for _, b := range tags {
					if strings.ContainsAny(b[:1], "+-") {
						return nil, fmt.Errorf("a spool's tags have no +/-: <project ID>,<bucket>,...")
					}
				}
				x := takesDefault
				if a.Takes != nil {
					x = *a.Takes
				}
				picked = s.recall(tags, a.Job, x)
			}
			t, err := s.taskSpool(a.Name, a.Job, tags, picked)
			if err != nil {
				return nil, err
			}
			return map[string]any{"task": t, "takes": s.takesFull(picked)}, nil
		}},
	"task.undo": {`{"do":"task.undo","name":"x","run":0,"force":false,"dry":false} - put back what one run changed (run 0: the latest finished run); a file changed again since, or touched by another hybrid during that run, is left alone and named (force=true undoes the second kind too). dry=true only says what it would do. Pause a running task first. Desk only`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				Name       string
				Run        int
				Force, Dry bool
			}](r)
			if err != nil {
				return nil, err
			}
			return s.taskUndo(a.Name, a.Run, a.Force, a.Dry)
		}},
	"take.add": {`{"do":"take.add","text":"<what an agent did and what came of it>","tags":"<project ID>,<bucket>,..."} - a new take; or {"do":"take.add","id":"T12","tags":"<its project ID>,+bucket,-bucket"} - move it between buckets. Desk only`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ ID, Text, Tags string }](r)
			if err != nil {
				return nil, err
			}
			tags, err := parseTags(a.Tags)
			if err != nil {
				return nil, err
			}
			if a.ID != "" {
				return s.takes.move(a.ID, tags)
			}
			return s.takes.add(a.Text, tags, "desk", "")
		}},
	"take.remove": {`{"do":"take.remove","id":"T12"} - delete a take (prune); it never reaches a hybrid again. Desk only`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ ID string }](r)
			if err != nil {
				return nil, err
			}
			return map[string]any{"removed": a.ID}, s.takes.remove(a.ID)
		}},
	"take.list": {`{"do":"take.list","project":"","bucket":""} - every take in full (optionally one project and/or bucket), to audit and prune`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ Project, Bucket string }](r)
			if err != nil {
				return nil, err
			}
			return s.takes.list(strings.ToLower(a.Project), strings.ToLower(a.Bucket)), nil
		}},
	"task.list": {`{"do":"task.list"} - every live hybrid and its state`, func(s *Server, _ json.RawMessage) (any, error) {
		return s.taskList(), nil
	}},
	"task.get": {`{"do":"task.get","name":"x"} - its STATE, open asks, outputs and recent events`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct{ Name string }](r)
		if err != nil {
			return nil, err
		}
		return s.taskInfo(a.Name)
	}},
	"task.say": {`{"do":"task.say","name":"x","text":"<new orders>"} - orders on top of its STATE; wakes a waiting seat`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ Name, Text string }](r)
			if err != nil {
				return nil, err
			}
			if err := s.taskSay(a.Name, a.Text, "DESK"); err != nil {
				return nil, err
			}
			s.board.Post(Event{Kind: "orders", Task: a.Name, Who: "desk", Text: a.Text})
			return "ok", nil
		}},
	"task.verdict": {`{"do":"task.verdict","name":"x","pass":true|false,"text":"<why>"} - PASS retires the hybrid; FAIL sends its next run to the Opus corrector`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				Name, Text string
				Pass       bool
			}](r)
			if err != nil {
				return nil, err
			}
			return s.taskVerdict(a.Name, a.Pass, a.Text)
		}},
	"task.pause": {`{"do":"task.pause","name":"x"} - stops its run and holds it`, setState("paused")},
	"task.cancel": {`{"do":"task.cancel","name":"x","text":"<why>"} - abandon a task: stops its run, keeps the record (marked cancelled), retires the hybrid`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ Name, Text string }](r)
			if err != nil {
				return nil, err
			}
			return s.taskCancel(a.Name, a.Text)
		}},
	"task.resume": {`{"do":"task.resume","name":"x"} - back to work (also clears blocked)`, setState("active")},
	"say": {`{"do":"say","task":"x","text":"..."} - a seat's post to the board (5 lines max)`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct{ Task, Text, Who string }](r)
		if err != nil {
			return nil, err
		}
		if a.Who == "" {
			a.Who = "hybrid-" + a.Task
		}
		return s.board.Post(Event{Kind: "say", Task: a.Task, Who: a.Who, Text: clip(a.Text, 2000)}), nil
	}},
	"read": {`{"do":"read","since":0,"task":"","kinds":["say"]} - board events after n (optional filters)`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct {
			Since int
			Task  string
			Kinds []string
			Limit int
		}](r)
		if err != nil {
			return nil, err
		}
		if a.Limit == 0 {
			a.Limit = 100
		}
		all := s.board.Since(a.Since, a.Task, a.Kinds, 0)
		res := map[string]any{"head": s.board.Head()}
		if len(all) > a.Limit { // the newest ones (a hybrid wants the desk's latest word), and it says so: on Windows the desk's stats lost a whole task to a silent cut
			res["note"] = fmt.Sprintf("showing the newest %d of %d events after %d; pass limit=%d for all of them", a.Limit, len(all), a.Since, len(all))
			all = all[len(all)-a.Limit:]
		}
		res["events"] = all
		return res, nil
	}},
	"wait": {`{"do":"wait","since":<n>,"timeout":900} - blocks until something needs the desk (waiting, blocked, say, verdict, feedback, learn)`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				Since, Timeout int
				Task           string
				Kinds          []string
			}](r)
			if err != nil {
				return nil, err
			}
			if len(a.Kinds) == 0 {
				a.Kinds = []string{"waiting", "blocked", "say", "feedback", "learn"}
			}
			if a.Timeout <= 0 || a.Timeout > 3600 {
				a.Timeout = 900
			}
			ev := s.board.Wait(a.Since, a.Task, a.Kinds, time.Duration(a.Timeout)*time.Second)
			return map[string]any{"head": s.board.Head(), "events": ev}, nil
		}},
	"learn.add": {`{"do":"learn.add","task":"x","text":"<the right way>","detect":"<the mistake as an action about to happen>","next":"<optional: the kind of next step, for a route>","source":"opus","evidence":"<n / file>"} - Jev decides lesson vs route, general vs this-task-only, and merges repeats`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[AddIn](r)
			if err != nil {
				return nil, err
			}
			it, how, err := s.learn.Add(a)
			if it == nil && err == nil {
				s.board.Post(Event{Kind: "learn", Task: a.Task, Who: a.Source, Text: how + ": " + clip(a.Text, 200)})
			} else if err == nil {
				s.board.Post(Event{Kind: "learn", Task: a.Task, Who: a.Source, Text: fmt.Sprintf("%s %s (%s, %s): %s", how, it.ID, it.Kind, it.Scope, clip(it.Text, 160))})
			}
			return map[string]any{"item": it, "how": how}, err
		}},
	"tool.list": {`{"do":"tool.list"} - the tool library: every script in exec/ with a "usage:" line near its top`,
		func(s *Server, r json.RawMessage) (any, error) { return s.tools(), nil }},
	"learn.export": {`{"do":"learn.export","file":".osenv/lesson-pack.json","min_catches":1} - write the lessons that proved themselves (caught a real mistake) to a pack another project can load; owner rules stay home`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				File       string
				MinCatches *int `json:"min_catches"`
			}](r)
			if err != nil {
				return nil, err
			}
			if a.File == "" {
				a.File = s.st.path("lesson-pack.json")
			} else if !filepath.IsAbs(a.File) {
				a.File = filepath.Join(s.st.Root, a.File)
			}
			n := 1
			if a.MinCatches != nil {
				n = max(0, *a.MinCatches)
			}
			p := s.learn.exportPack(s.projectID(), n)
			if len(p.Lessons) == 0 {
				return nil, fmt.Errorf("no active global lesson has caught %d mistake(s) yet; nothing to export", n)
			}
			writeJSON(a.File, p)
			return map[string]any{"file": a.File, "lessons": len(p.Lessons), "pack": p}, nil
		}},
	"learn.import": {`{"do":"learn.import","file":"<a lesson pack from learn.export>"} - load another project's proven lessons as global lessons (a lesson already here is skipped). Desk only`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ File string }](r)
			if err != nil {
				return nil, err
			}
			if !filepath.IsAbs(a.File) {
				a.File = filepath.Join(s.st.Root, a.File)
			}
			b, err := os.ReadFile(a.File)
			if err != nil {
				return nil, err
			}
			var p Pack
			if err := json.Unmarshal(b, &p); err != nil || p.Pack != 1 {
				return nil, fmt.Errorf("%s is not an osenv lesson pack (learn.export writes one)", a.File)
			}
			added, skipped := s.learn.importPack(p)
			msg := fmt.Sprintf("imported %d lessons from %s's pack (%s)", len(added), p.From, strings.Join(added, ", "))
			if len(added) == 0 {
				msg = fmt.Sprintf("imported no lessons from %s's pack: all %d are already here", p.From, len(skipped))
			}
			s.board.Post(Event{Kind: "learn", Who: "desk", Text: msg})
			return map[string]any{"added": added, "skipped_as_already_here": skipped}, nil
		}},
	"learn.list": {`{"do":"learn.list","task":"","kind":"lesson|route"} - the learned loop (task = what that hybrid can see)`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ Task, Kind string }](r)
			if err != nil {
				return nil, err
			}
			s.learn.mu.Lock()
			defer s.learn.mu.Unlock()
			return s.learn.list(a.Kind, a.Task), nil
		}},
	"learn.relevant": {`{"do":"learn.relevant","task":"x","job":"<default: the task's JOB.md>","orders":"<what the agent is about to do>"} - the lessons Jev would remind muse of at run start, with no side effects on a live seat`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct{ Task, Job, Orders string }](r)
			if err != nil {
				return nil, err
			}
			if a.Job == "" {
				a.Job = s.job(a.Task)
			}
			return s.learn.Relevant(a.Task, clip(a.Job, 2000), a.Orders, false)
		}},
	"learn.edit": {`{"do":"learn.edit","id":"R1","kind":"lesson","scope":"global","severity":"nudge","to":"","text":"...","detect":"...","escapes":0,"catches":0} - fix how Jev filed a lesson or route; only the fields you give change. escapes and catches correct counts a bug or a false alarm inflated`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[struct {
				ID, Kind, Scope, Severity, To, Text, Detect string
				Escapes                                     *int // Linux run: a duplicate-finding bug pushed L10 to kick; severity alone re-escalates
				Catches                                     *int // VM 0.2.3 #10: a false catch inflated L3's record, which orders the checked set
			}](r)
			if err != nil {
				return nil, err
			}
			var replay []map[string]any
			if a.Detect != "" {
				replay = s.replayCatches(a.ID, a.Detect)
			}
			s.learn.mu.Lock()
			defer s.learn.mu.Unlock()
			it := s.learn.get(a.ID)
			if it == nil {
				return nil, fmt.Errorf("no item %s", a.ID)
			}
			e := *it
			for _, f := range []struct {
				v string
				p *string
			}{{a.Kind, &e.Kind}, {a.Scope, &e.Scope}, {a.Severity, &e.Severity}, {a.To, &e.To}, {a.Text, &e.Text}, {a.Detect, &e.Detect}} {
				if f.v != "" {
					*f.p = f.v
				}
			}
			if e.Kind == "lesson" {
				e.To = ""
			}
			if a.Escapes != nil {
				if *a.Escapes < 0 {
					return nil, fmt.Errorf("escapes is 0 or more")
				}
				e.Escapes = *a.Escapes
			}
			if a.Catches != nil {
				if *a.Catches < 0 {
					return nil, fmt.Errorf("catches is 0 or more")
				}
				e.Catches = *a.Catches
			}
			switch {
			case e.Kind != "lesson" && e.Kind != "route":
				return nil, fmt.Errorf("kind is lesson or route")
			case e.Kind == "route" && e.To != "deepseek" && e.To != "qwen" && e.To != "opus":
				return nil, fmt.Errorf("a route needs to=deepseek, qwen or opus")
			case e.Scope != "global" && !strings.HasPrefix(e.Scope, "task:"):
				return nil, fmt.Errorf("scope is global or task:<name>")
			case e.Severity != "nudge" && e.Severity != "kick":
				return nil, fmt.Errorf("severity is nudge or kick")
			}
			e.Why = strings.TrimPrefix(e.Why+"; edited by the desk "+now()[:16], "; ")
			*it = e
			s.learn.save()
			var changed []string
			for k, v := range map[string]bool{"kind": a.Kind != "", "scope": a.Scope != "", "severity": a.Severity != "", "to": a.To != "",
				"text": a.Text != "", "detect": a.Detect != "", "escapes": a.Escapes != nil, "catches": a.Catches != nil} {
				if v {
					changed = append(changed, k)
				}
			}
			sort.Strings(changed)
			s.board.Post(Event{Kind: "learn", Who: "desk", Text: fmt.Sprintf("edited %s (%s): %s", it.ID, strings.Join(changed, ", "), clip(it.Text, 160))}) // video bench #4: an audit trail
			if len(replay) > 0 {
				return map[string]any{"item": it, "replay": replay, "note": "each action was caught by this lesson before; misses_now means the new detect lets it through, which is right only if that catch was a false alarm"}, nil
			}
			return it, nil
		}},
	"learn.retire": {`{"do":"learn.retire","id":"L3"} - retire a lesson or route by hand`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct{ ID string }](r)
		if err != nil {
			return nil, err
		}
		s.learn.mu.Lock()
		defer s.learn.mu.Unlock()
		it := s.learn.get(a.ID)
		if it == nil {
			return nil, fmt.Errorf("no item %s", a.ID)
		}
		it.Status = "retired"
		s.learn.save()
		return it, nil
	}},
	"learn.graph": {`{"do":"learn.graph"} - the learned loop as nodes and edges (kgraph shape)`, func(s *Server, _ json.RawMessage) (any, error) {
		return s.learn.graph(), nil
	}},
	"acts": {`{"do":"acts","task":"x","limit":40,"before":"<at>"} - the seat's scored tool calls, newest first, up to 200; to page back, pass the oldest "at" you got as before (within the log's newest 16 MB)`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct {
			Task, Before string
			Limit        int
		}](r)
		if err != nil {
			return nil, err
		}
		return s.actsBefore(a.Task, a.Limit, a.Before), nil
	}},
	"view": {`{"do":"view","task":"x","path":"<file>","q":"<optional question>","crop":"<optional x,y,w,h>"} - a parsed view: small image, log errors+tail, code outline, JSON shape, or the parts Jev reasons answer q`,
		func(s *Server, r json.RawMessage) (any, error) {
			a, err := arg[ViewIn](r)
			if err != nil {
				return nil, err
			}
			return s.view(a)
		}},
	"hook": {`{"do":"hook",...} - internal: the engines' hooks (via "osenv hook pre|post")`, func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[HookIn](r)
		if err != nil {
			return nil, err
		}
		return s.hook(a), nil
	}},
}

// help lists verbs, so it is registered after the table exists (a literal entry would be an init cycle).
func init() {
	verbs["help"] = verb{`{"do":"help"} - this list`, func(s *Server, _ json.RawMessage) (any, error) { return help(), nil }}
}

func setState(st string) func(*Server, json.RawMessage) (any, error) {
	return func(s *Server, r json.RawMessage) (any, error) {
		a, err := arg[struct{ Name string }](r)
		if err != nil {
			return nil, err
		}
		return "ok", s.taskSetState(a.Name, st)
	}
}

func help() map[string]any {
	var names []string
	for k := range verbs {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		out = append(out, verbs[k].usage)
	}
	return map[string]any{"route": "POST /v1", "verbs": out,
		"batch": `{"do":"batch","ops":[{"do":"task.list"},{"do":"read","since":0}]}`}
}

// forceWho: in a request from inside an engine run, every say is signed <role>-<task>, whatever who it asked for.
func forceWho(raw json.RawMessage, role string) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	switch m["do"] {
	case "batch":
		if ops, ok := m["ops"].([]any); ok {
			for i, op := range ops {
				b, _ := json.Marshal(op)
				var o any
				json.Unmarshal(forceWho(b, role), &o)
				ops[i] = o
			}
		}
	case "say":
		t, _ := m["task"].(string)
		m["who"] = role + "-" + t
	default:
		return raw
	}
	b, _ := json.Marshal(m)
	return b
}

// deskVerb: the first desk-only verb in a request, batches included (a hybrid wrapped take.list in a batch).
func deskVerb(raw json.RawMessage) string {
	var h struct {
		Do     string
		Source string
		Ops    []json.RawMessage
	}
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	if h.Do == "batch" {
		for _, op := range h.Ops {
			if v := deskVerb(op); v != "" {
				return v
			}
		}
		return ""
	}
	switch {
	case deskVerbRe.MatchString(h.Do), strings.HasPrefix(h.Do, "take."):
		return h.Do
	case h.Do == "learn.add" && strings.EqualFold(h.Source, "owner"):
		return "learn.add source=owner"
	}
	return ""
}

func (s *Server) call(raw json.RawMessage) (any, error) {
	var h struct {
		Do  string
		Ops []json.RawMessage
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("body must be JSON with a \"do\" field: %v", err)
	}
	if h.Do == "batch" {
		var out []any
		for _, op := range h.Ops {
			v, err := s.call(op)
			if err != nil {
				v = map[string]any{"error": err.Error()}
			}
			out = append(out, v)
		}
		return out, nil
	}
	v, ok := verbs[h.Do]
	if !ok {
		return nil, fmt.Errorf("unknown verb %q; {\"do\":\"help\"} lists them", h.Do)
	}
	return v.run(s, raw)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v1" && r.URL.Path != "/v1/" {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"error": "one route: POST /v1 {\"do\":\"help\"}"})
		return
	}
	if r.Method == "GET" {
		json.NewEncoder(w).Encode(help())
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	role := r.Header.Get("X-Osenv-Role") // set by `osenv do` inside an engine run; the desk's calls carry none
	var v any
	var err error
	if role != "" && role != "desk" {
		raw = forceWho(raw, role) // a hybrid's post carries its own name (the Windows desk showed who= could be anyone)
	}
	if verb := deskVerb(raw); role != "" && role != "desk" && verb != "" {
		err = fmt.Errorf("%s is the desk's call; a hybrid posts STEP DONE and WAITING: desk review instead (takes meant for it are in its brief under MEMORIES)", verb)
	} else {
		v, err = s.call(raw)
	}
	out := map[string]any{"ok": err == nil, "result": v}
	if err != nil {
		w.WriteHeader(400)
		out = map[string]any{"ok": false, "error": err.Error()}
	}
	if role == "" || role == "desk" { // the API speaks to the desk inside every response it already gets
		if m := s.takes.message(); m != "" {
			out["take_buckets"] = m
		}
	}
	json.NewEncoder(w).Encode(out)
}

var statusJevWait = 8 * time.Second

func (s *Server) status() map[string]any {
	cfg := s.st.Cfg
	tool := func(c string) string {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
		return "MISSING"
	}
	// status answers even when Jev doesn't (Windows light rc10: it hung behind a stuck connection)
	jevState := "no answer within 8 s: Jev is slow or unreachable, so actions are running unscored"
	done := make(chan error, 1)
	go func() {
		_, err := ask(map[string]any{"ping": "status check"}, map[string]Q{"ok": Noul("This is a status check.")})
		done <- err
	}()
	select {
	case jerr := <-done:
		jevState = "ok"
		if jerr != nil {
			jevState = jerr.Error()
		}
	case <-time.After(statusJevWait):
	}
	// the project's own toolchain (VM test: a fresh Windows had only the Store's python stub)
	tools := map[string]string{}
	py := "python3" // Debian and friends ship python3 only (Linux VM test)
	if runtime.GOOS == "windows" {
		py = "python"
	}
	for _, c := range []string{py, "git", "node", "go"} {
		p := tool(c)
		if strings.Contains(strings.ToLower(p), `\windowsapps\`) {
			p = "MISSING (only the Microsoft Store stub)"
		}
		tools[c] = p
	}
	if p := tools[py]; !strings.HasPrefix(p, "MISSING") { // bench run: a job asked for pytest on a machine without it
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if exec.CommandContext(ctx, p, "-m", "pytest", "--version").Run() == nil {
			tools["pytest"] = "ok"
		} else {
			tools["pytest"] = "MISSING (" + py + " -m pytest fails)"
		}
		cancel()
	}
	s.mu.Lock()
	live, orphans := len(s.live), 0
	for _, o := range s.orphans { // runs from before a restart are still running (video bench: status said 0 while 3 ran)
		if procAlive(o.pid) {
			orphans++
		}
	}
	n, seen := len(s.tasks), s.hookCalls // Linux light rc8: summing hookN said 0 after 293 calls, once both tasks retired
	s.mu.Unlock()
	hooks := fmt.Sprintf("ok: %d hook calls seen since serve started", seen)
	switch {
	case strings.Contains(s.cli, " ") && runtime.GOOS == "windows":
		hooks = "BROKEN: the path to osenv.exe has spaces and no short name; move it to a folder without spaces"
	case seen == 0: // VM test #23: "ok" before any hook had fired only said the command looked right
		hooks = "configured, no hook call seen yet (a run's first action proves it): " + s.cli + " hook ..."
	}
	return map[string]any{"root": s.st.Root, "url": s.url, "board_head": s.board.Head(), "tasks": n, "running": live + orphans, "running_from_before_restart": orphans,
		"hooks": hooks, "hook_calls_seen": seen,
		"tools": tools, "tools_note": "installed something new? restart osenv serve: it keeps the PATH it started with",
		"engines": map[string]string{"muse": tool(cfg.Muse.Cmd), "deepseek/qwen": tool(cfg.Plan.Cmd), "opus": tool(cfg.Claude.Cmd)},
		"keys":    map[string]bool{"jev": readKey("OSENV_JEV_KEY", cfg.Jev.KeyFile) != "", "plan": readKey("OSENV_PLAN_KEY", cfg.Plan.KeyFile) != ""},
		"version": version, "jev": jevState, "lessons": len(s.learn.list("lesson", "")), "exec_tools": len(s.tools()), "routes": len(s.learn.list("route", "")),
		"opus_cap": opusCap(cfg.OpusCap), "cadence": fmt.Sprintf("an Opus review every %d muse runs", cfg.Cadence)}
}

func (s *Server) acts(task string, limit int) []map[string]any { return s.actsBefore(task, limit, "") }

// actsBefore: the newest acts older than before (an "at" time; "" = now), so a desk can page back through a long task
// (Windows light run: a 224-action task showed its newest 200 and no way to reach the start).
func (s *Server) actsBefore(task string, limit int, before string) []map[string]any {
	if limit <= 0 {
		limit = 40
	}
	limit = min(limit, 200) // VM issue #28: over 200 used to fall back to 40 without a word
	bt, berr := time.Parse(time.RFC3339, before)
	older := func(at string) bool { // as times: records from before rc9 have whole seconds, newer ones nanoseconds
		if t, err := time.Parse(time.RFC3339, at); err == nil && berr == nil {
			return t.Before(bt)
		}
		return at < before
	}
	// Newest by stamp, not by line: an act is stamped when it arrives and written when Jev has answered, so the log
	// can hold two acts out of stamp order, and a page cut in log order skipped one (Linux light rc11).
	type act struct {
		m  map[string]any
		at time.Time
	}
	var all []act
	for _, l := range readLines(s.st.path("acts.jsonl"), 1<<20) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && (task == "" || m["task"] == task) && m["event"] == "pre" && (before == "" || older(fmt.Sprint(m["at"]))) {
			at, _ := time.Parse(time.RFC3339, fmt.Sprint(m["at"]))
			all = append(all, act{m, at})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	out := []map[string]any{}
	for _, a := range all[:min(limit, len(all))] {
		out = append(out, a.m)
	}
	return out
}

// replayCatches: a detect edit is re-scored on the actions the lesson caught before (Windows 0.2.3 #13: an edit
// made L3 stop catching a bare > and nothing said so). Up to 8 recent catches, one Jev call each.
func (s *Server) replayCatches(id, detect string) []map[string]any {
	var acts []map[string]any
	for _, a := range s.acts("", 200) {
		if l, _ := a["learned"].([]any); a["test"] == nil && contains(anyStrings(l), id) && len(acts) < 8 {
			acts = append(acts, a)
		}
	}
	var out []map[string]any
	for _, a := range acts {
		r, err := ask(map[string]any{"action": a["what"], "tool": a["tool"], "platform": platformName()}, map[string]Q{"d": Noul(detect)})
		if err != nil {
			return nil
		}
		p := float64(int(r["d"].P()*100)) / 100
		out = append(out, map[string]any{"action": a["what"], "task": a["task"], "score": p, "misses_now": p < detectP})
	}
	return out
}

func anyStrings(l []any) []string {
	var out []string
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func opusCap(n int) string {
	if n <= 0 {
		return "no cap (opus_per_day 0 means unlimited)"
	}
	return fmt.Sprintf("%d Opus runs per task per day", n)
}
