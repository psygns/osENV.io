package main

// task.go: one task = one hybrid seat (hybrid-<name>). It lives in .osenv/tasks/<name>/ and
// is retired when the desk passes it: the record moves to .osenv/done/, the working folder
// is deleted, and nothing but the shared lessons carries into the next hybrid.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Task struct {
	Name      string `json:"name"`
	Seat      string `json:"seat"`
	State     string `json:"state"` // active | waiting | paused | blocked
	Created   string `json:"created"`
	RunsTotal int    `json:"runs_total"`
	// loop bookkeeping, read and written by pick.go
	LastEngine string         `json:"last_engine"`
	Reason     string         `json:"reason"`
	Runs       int            `json:"runs_since_opus"`
	Crashes    int            `json:"remedy_crashes"`
	LastExit   int            `json:"last_exit"`
	ServedFail int            `json:"served_fail"` // board n of the newest desk FAIL an Opus run has served
	OpusDay    string         `json:"opus_day"`
	OpusToday  int            `json:"opus_today"`
	Surfaced   []string       `json:"surfaced"`        // lessons Jev judged relevant for the current run
	Tools      []string       `json:"tools,omitempty"` // exec/ tools Jev listed in the current run's brief
	Tags       []string       `json:"tags,omitempty"`  // the tag string the desk spooled it with: project ID, then buckets
	Takes      []string       `json:"takes,omitempty"` // the takes it starts with (IDs), picked by Jev at spool
	QuickFails int            `json:"quick_fails"`
	LastEnd    string         `json:"last_end"`
	Engines    map[string]int `json:"engines,omitempty"`
	RunPID     int            `json:"run_pid,omitempty"` // the live engine; a restart waits for it instead of starting a second one // clean runs per engine: reviews done, for the gate at park
}

func (s *Server) tdir(name string, parts ...string) string {
	return filepath.Join(append([]string{s.st.Dir, "tasks", name}, parts...)...)
}

func (s *Server) loadTasks() {
	ents, _ := os.ReadDir(s.st.path("tasks"))
	for _, e := range ents {
		var t Task
		if readJSON(s.tdir(e.Name(), "task.json"), &t) == nil {
			if t.State == "" {
				t.State = "active"
			}
			s.tasks[t.Name] = &t
		}
	}
}

func (s *Server) saveTask(t *Task) { writeJSON(s.tdir(t.Name, "task.json"), t) }

func (s *Server) notes(name string) string {
	b, _ := os.ReadFile(s.tdir(name, "NOTES.md"))
	return string(b)
}

func (s *Server) setNotes(name, text string) {
	os.WriteFile(s.tdir(name, "NOTES.md"), []byte(text), 0o644)
}

func (s *Server) job(name string) string {
	b, _ := os.ReadFile(s.tdir(name, "JOB.md"))
	return string(b)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, "# ")); l != "" {
			return l
		}
	}
	return ""
}

func (s *Server) taskNew(name, job string) (*Task, error) { return s.taskSpool(name, job, nil, nil) }

// taskSpool: a new hybrid with the desk's tag string and the takes it starts with.
func (s *Server) taskSpool(name, job string, tags, takes []string) (*Task, error) {
	name = slug(name)
	if name == "" || strings.TrimSpace(job) == "" {
		return nil, fmt.Errorf("task.new needs name and job (the job text: goal, gates, what proof to show)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[name]; ok {
		return nil, fmt.Errorf("task %q exists; finish it or pick another name", name)
	}
	if err := os.MkdirAll(s.tdir(name, "out"), 0o755); err != nil {
		return nil, err
	}
	t := &Task{Name: name, Seat: "hybrid-" + name, State: "active", Created: now(), Tags: tags, Takes: takes}
	os.WriteFile(s.tdir(name, "JOB.md"), []byte(job), 0o644)
	s.setNotes(name, fmt.Sprintf("# %s NOTES\n\n## STATE\n- Job: %s (full text in .osenv/tasks/%s/JOB.md)\n\n## LOG\n", t.Seat, firstLine(job), name))
	s.saveTask(t)
	s.tasks[name] = t
	s.board.Post(Event{Kind: "task.new", Task: name, Who: "desk", Text: firstLine(job)})
	return t, nil
}

func (s *Server) task(name string) (*Task, error) {
	t, ok := s.tasks[name]
	if !ok {
		return nil, fmt.Errorf("no task %q (task.list shows the live ones)", name)
	}
	return t, nil
}

func (s *Server) taskInfo(name string) (map[string]any, error) {
	s.mu.Lock()
	t, err := s.task(name)
	var tc Task
	if err == nil {
		tc = *t
	}
	_, live := s.live[name]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	n := s.notes(name)
	var outs []string
	more := 0
	filepath.WalkDir(s.tdir(name, "out"), func(p string, d os.DirEntry, _ error) error {
		if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir // a browser profile or cache (Linux 0.3 run: task.get was mostly Firefox cache files)
		}
		if d != nil && !d.IsDir() {
			if len(outs) < 40 {
				r, _ := filepath.Rel(s.st.Root, p)
				outs = append(outs, r)
			} else {
				more++
			}
		}
		return nil
	})
	if more > 0 {
		outs = append(outs, fmt.Sprintf("(+%d more files)", more))
	}
	return map[string]any{"task": tc, "running": live, "state_block": stateBlock(n), "asks": asks(n),
		"waiting": tc.State == "waiting", "out": outs, "notes_path": s.tdir(name, "NOTES.md"),
		"events": s.board.Since(0, name, nil, 12), "takes": s.takesFull(tc.Takes)}, nil
}

func (s *Server) taskList() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, t := range s.tasks {
		_, live := s.live[t.Name]
		out = append(out, map[string]any{"name": t.Name, "seat": t.Seat, "state": t.State, "running": live,
			"last_engine": t.LastEngine, "runs": t.RunsTotal, "created": t.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["created"].(string) < out[j]["created"].(string) })
	return out
}

// taskSay puts the desk's orders on top of STATE and wakes a parked seat.
func (s *Server) taskSay(name, text, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.task(name)
	if err != nil {
		return err
	}
	n := dropLines(s.notes(name), func(m, _ string) bool { return strings.HasPrefix(m, "WAITING: desk review") })
	s.setNotes(name, underState(n, fmt.Sprintf("- %s %s: %s", prefix, now()[:16], text)))
	if t.State == "waiting" || t.State == "blocked" {
		t.State, t.QuickFails = "active", 0
		s.saveTask(t)
	}
	return nil
}

// taskVerdict: PASS retires the hybrid; FAIL sends its next run to the Opus corrector.
func (s *Server) taskVerdict(name string, pass bool, text string) (map[string]any, error) {
	s.mu.Lock()
	_, err := s.task(name)
	_, live := s.live[name]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !pass {
		s.takes.add(name+" FAIL: "+shortTake(text), s.takeTags(name), "osenv", name) // recorded automatically
		e := s.board.Post(Event{Kind: "verdict", Task: name, Who: "desk", Text: text, Data: map[string]any{"pass": false}})
		return map[string]any{"verdict": "fail", "event": e.N, "next": "the next run goes to the Opus corrector, then back to muse"},
			s.taskSay(name, text, "DESK FAIL")
	}
	if live && !s.waitIdle(name, 90*time.Second) { // a seat that just wrote WAITING is still shutting down
		return nil, fmt.Errorf("%s is still mid-run after 90 s; pass it when the run ends (task.get shows running), or stop the run now with task.pause name=%s, check what it changed with task.undo name=%s dry=true, then pass", name, name, name)
	}
	s.takes.add(name+" PASS: "+shortTake(text), s.takeTags(name), "osenv", name)
	os.WriteFile(s.tdir(name, "VERDICT.md"), []byte("PASS "+now()+" by the desk: "+text+"\n"), 0o644) // the record keeps its verdict (Windows 0.3 run)
	s.mu.Lock()
	if t, ok := s.tasks[name]; ok {
		t.State = "passed"
		s.saveTask(t)
	}
	s.mu.Unlock()
	s.board.Post(Event{Kind: "verdict", Task: name, Who: "desk", Text: text, Data: map[string]any{"pass": true}})
	rec, err := s.retire(name)
	return map[string]any{"verdict": "pass", "record": rec}, err
}

// retire keeps the record (job, final notes, task.json, out/) and deletes the working seat.
func (s *Server) retire(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.st.path("done", name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(rec, 0o755); err != nil {
		return "", err
	}
	for from, to := range map[string]string{"JOB.md": "JOB.md", "NOTES.md": "final-NOTES.md", "task.json": "task.json", "out": "out", "CANCELLED.md": "CANCELLED.md", "VERDICT.md": "VERDICT.md", "run.log": "run.log"} {
		if src := s.tdir(name, from); fileExists(src) && os.Rename(src, filepath.Join(rec, to)) != nil {
			copyTree(src, filepath.Join(rec, to)) // held open by something (Windows): keep a copy, never lose the record
		}
	}
	// A retire never stops halfway (Windows 0.3 run: a failed delete left the task live, and a second PASS split
	// the record in two and recorded a second take). Whatever can't be deleted is named instead.
	if err := os.RemoveAll(s.tdir(name)); err != nil {
		s.board.Post(Event{Kind: "blocked", Task: name, Who: "osenv", Text: "retired, but its folder couldn't be fully deleted (a process it started may still hold a file): " + clip(err.Error(), 200) + ". Stop that process, then delete .osenv/tasks/" + name + " by hand."})
	}
	delete(s.tasks, name)
	delete(s.orphans, name)
	delete(s.held, name) // a long-running server doesn't keep every retired task's state
	delete(s.hookN, name)
	s.recMu.Lock()
	delete(s.recents, name)
	s.recMu.Unlock()
	s.learn.dropTask(name)
	go s.undoGC() // its snapshots went with its folder
	s.board.Post(Event{Kind: "retired", Task: name, Who: "desk", Text: rec})
	return rec, nil
}

func (s *Server) taskSetState(name, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.task(name)
	if err != nil {
		return err
	}
	if state == "paused" {
		s.stopRun(name)
	}
	t.State = state
	if state == "active" {
		t.QuickFails = 0
	}
	s.saveTask(t)
	s.board.Post(Event{Kind: state, Task: name, Who: "desk"})
	return nil
}

// taskCancel abandons a task without a verdict: its run stops, its record is kept and marked
// cancelled, and its hybrid retires like any other (VM test issue #7: a restarted task used to
// sit on the board as "paused" for good).
func (s *Server) taskCancel(name, why string) (map[string]any, error) {
	s.mu.Lock()
	_, err := s.task(name)
	if err == nil && s.stopRun(name) {
		s.mu.Unlock()
		s.waitIdle(name, 10*time.Second) // give the run a moment to end before its folder moves
	} else {
		s.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	os.WriteFile(s.tdir(name, "CANCELLED.md"), []byte("Cancelled "+now()+" by the desk: "+why+"\n"), 0o644)
	s.takes.add(name+" cancelled: "+shortTake(why), s.takeTags(name), "osenv", name)
	s.mu.Lock()
	if t, ok := s.tasks[name]; ok {
		t.State = "cancelled"
		s.saveTask(t)
	}
	s.mu.Unlock()
	s.board.Post(Event{Kind: "cancelled", Task: name, Who: "desk", Text: why})
	rec, err := s.retire(name)
	return map[string]any{"cancelled": name, "record": rec}, err
}

// stopRun stops the task's run, whether this server started it or it's one from before a restart (Linux video bench
// #20: pausing an orphaned run would have stopped nothing). The caller holds s.mu. True when something was stopped.
func (s *Server) stopRun(name string) bool {
	if c, ok := s.live[name]; ok && c != nil && c.Process != nil {
		killGroup(c)
		return true
	}
	if o, ok := s.orphans[name]; ok && procAlive(o.pid) {
		if p, err := os.FindProcess(o.pid); err == nil {
			killGroup(&exec.Cmd{Process: p})
			return true
		}
	}
	return false
}

// waitIdle waits up to d for the task's run to end; true when it has.
func (s *Server) waitIdle(name string, d time.Duration) bool {
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		s.mu.Lock()
		_, live := s.live[name]
		s.mu.Unlock()
		if !live || time.Now().After(end) {
			return !live
		}
	}
}

// copyTree copies a file or folder (a retire's fallback when a move fails).
func copyTree(src, dst string) {
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			os.MkdirAll(t, 0o755)
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			os.MkdirAll(filepath.Dir(t), 0o755)
			os.WriteFile(t, b, 0o644)
		}
		return nil
	})
}
