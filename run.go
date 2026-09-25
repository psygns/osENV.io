package main

// run.go: the supervisor. It launches one engine run at a time per hybrid (up to `parallel`
// hybrids at once), parks a seat that wrote "WAITING: desk review", and after a routed
// review keeps only the reviewer's in-lane feedback. The engines are the CLIs you already
// have (muse, qwen for deepseek/qwen, claude for Opus); each one's hooks call back into
// this binary, so Jev sees every tool call.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed prompts/*.md
var prompts embed.FS

func (s *Server) supervise() {
	tick := time.NewTicker(3 * time.Second)
	sweep := time.NewTicker(10 * time.Minute)
	for {
		select {
		case <-tick.C:
			s.schedule()
		case <-sweep.C:
			for _, id := range s.learn.sweep() {
				s.board.Post(Event{Kind: "learn", Who: "jev", Text: "retired " + id + " as noise: surfaced 40+ times, never caught"})
			}
		}
	}
}

func (s *Server) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts []*Task
	for _, t := range s.tasks {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Created < ts[j].Created })
	for _, t := range ts {
		if _, live := s.live[t.Name]; live || t.State != "active" {
			continue
		}
		if o, ok := s.orphans[t.Name]; ok {
			if o.ending || procAlive(o.pid) && time.Now().Before(o.until) {
				continue // a run from before the restart is still going, or being wrapped up: never two engines on one hybrid
			}
			killed := procAlive(o.pid)
			if p, err := os.FindProcess(o.pid); err == nil && killed { // past the run timeout, as its own server would have
				killGroup(&exec.Cmd{Process: p})
			}
			o.ending = true
			s.orphans[t.Name] = o
			t.RunPID = 0
			s.saveTask(t)
			go s.orphanEnded(t.Name, killed)
			continue // its next run starts on a pass after the wrap-up
		}
		if t.LastEnd != "" {
			if e, err := time.Parse(time.RFC3339, t.LastEnd); err == nil && time.Since(e) < 12*time.Second {
				continue
			}
		}
		if waiting(s.notes(t.Name)) {
			t.State = "waiting"
			s.saveTask(t)
			s.board.Post(Event{Kind: "waiting", Task: t.Name, Who: t.Seat, Text: "STEP DONE, waiting for the desk (task.get " + t.Name + ")"})
			continue
		}
		if len(s.live) >= max(1, s.st.Cfg.Parallel) {
			return
		}
		s.live[t.Name] = nil // claimed; the command lands here once it starts
		go s.run(t.Name)
	}
}

func (s *Server) run(name string) {
	s.mu.Lock()
	t, ok := s.tasks[name]
	if !ok {
		delete(s.live, name)
		s.mu.Unlock()
		return
	}
	tc := *t
	s.mu.Unlock()

	notes := s.notes(name)
	engine, reason, routed, notes2 := s.pick(&tc, notes) // Jev calls happen outside the lock
	if notes2 != notes {
		s.setNotes(name, notes2)
	}
	eff, lessonText, toolText := "", "", ""
	var surfaced, tools []string
	if engine == "muse" {
		for _, it := range s.learn.ownerRules() { // always in the brief, so always in run.start's list (Windows light run: n55 left O71 out)
			lessonText += fmt.Sprintf("- OWNER RULE, never break it: %s (%s)\n", it.Text, it.ID)
			surfaced = append(surfaced, it.ID)
		}
		eff = effort(notes2)
		if rel, err := s.learn.Relevant(name, clip(s.job(name), 2000), orders(notes2), true); err == nil {
			for _, it := range rel {
				if it.Source == "owner" {
					continue // already listed as an OWNER RULE above (video bench #1: every owner rule appeared twice)
				}
				surfaced = append(surfaced, it.ID)
				lessonText += fmt.Sprintf("- %s (lesson %s)\n", it.Text, it.ID)
			}
		}
		for _, t := range s.fitTools(name) {
			tools = append(tools, t.Path)
			toolText += fmt.Sprintf("- %s: %s\n", t.Path, t.Usage)
		}
	}

	s.mu.Lock()
	t, ok = s.tasks[name]
	if !ok || t.State != "active" { // retired or paused while we were deciding
		delete(s.live, name)
		s.mu.Unlock()
		return
	}
	t.LastEngine, t.Reason, t.Runs, t.Crashes, t.ServedFail = engine, reason, tc.Runs, tc.Crashes, tc.ServedFail
	t.OpusDay, t.OpusToday, t.Surfaced, t.Tools = tc.OpusDay, tc.OpusToday, surfaced, tools
	s.saveTask(t)
	seat := t.Seat
	s.mu.Unlock()

	if engine == "qwen" && routed {
		t0 := time.Now()
		verdict, txt := s.plainCheck(name)
		took := time.Since(t0).Round(time.Second).String() // every plain check leaves its own event, with its time (Windows light run: a PASS left none)
		switch verdict {
		case "fail": // a plain fault goes straight back to muse; the handback clears the visual ask
			s.mu.Lock()
			s.setNotes(name, sectionAfterState(s.notes(name), "## FEEDBACK (deepseek plain check "+now()[:16]+"; qwen was not called)\n"+txt+"\n"))
			if t, ok := s.tasks[name]; ok {
				t.LastExit, t.LastEnd, t.Reason = 0, now(), t.Reason+" (plain check: qwen not called)"
				s.saveTask(t)
			}
			delete(s.live, name)
			s.mu.Unlock()
			s.board.Post(Event{Kind: "feedback", Task: name, Who: "deepseek", Text: "plain check FAILED in " + took + " before the visual review, so qwen was not called; back to muse: " + clip(txt, 300)})
			return
		case "pass":
			if s.plainOnly(name) { // nothing to judge but plain checks, and they passed: qwen isn't needed
				s.mu.Lock()
				s.setNotes(name, sectionAfterState(s.notes(name), "## FEEDBACK (deepseek plain check "+now()[:16]+": PASSED; the job needs only plain checks, so this was the visual review)\n"+txt+"\n"))
				if t, ok := s.tasks[name]; ok {
					t.LastExit, t.LastEnd, t.Reason = 0, now(), t.Reason+" (plain check: qwen not called)"
					if t.Engines == nil {
						t.Engines = map[string]int{}
					}
					t.Engines["plain"]++ // counts as the visual review at park
					s.saveTask(t)
				}
				delete(s.live, name)
				s.mu.Unlock()
				s.board.Post(Event{Kind: "feedback", Task: name, Who: "deepseek", Text: "plain check PASSED in " + took + " and the job needs nothing more, so qwen was not called; back to muse"})
				return
			}
			reason += " | a cheap plain check already PASSED (nothing cut off, missing or clipped), so judge how it looks"
			s.board.Post(Event{Kind: "feedback", Task: name, Who: "deepseek", Text: "plain check PASSED in " + took + "; qwen judges the look next"})
		default:
			if txt != "" { // it ran and couldn't decide: say so, never skip it silently (Linux 0.3 run)
				s.board.Post(Event{Kind: "feedback", Task: name, Who: "deepseek", Text: "plain check gave no verdict in " + took + " (" + clip(txt, 160) + "); qwen reviews as usual"})
			}
		}
	}

	s.board.Post(Event{Kind: "run.start", Task: name, Who: seat, Text: engine + " " + reason,
		Data: map[string]any{"engine": engine, "effort": eff, "routed": routed, "lessons": surfaced}})
	cmd, err := s.command(name, seat, engine, eff, reason, lessonText, toolText)
	s.mu.Lock()
	hooks0 := s.hookN[name]
	s.mu.Unlock()
	start := time.Now()
	code := 127
	if err == nil {
		// The engine writes straight into run.log, so it survives a server crash (a pipe would die with the
		// server). On Windows run.log is opened so it can be moved while a process holds it: a server a hybrid
		// left running held it, and the PASS couldn't retire the task (Windows 0.3 run).
		logf, _ := openLog(s.tdir(name, "run.log"))
		fmt.Fprintf(logf, "--- %s %s %s\n", now(), engine, reason)
		cmd.Stdout, cmd.Stderr = logf, logf
		setGroup(cmd)
		undoN := s.undoBefore(name, engine) // what the project looked like, so task.undo can put this run back
		defer s.undoAfter(name, undoN)
		if err = cmd.Start(); err == nil {
			s.mu.Lock()
			s.live[name] = cmd
			if t, ok := s.tasks[name]; ok {
				t.RunPID = cmd.Process.Pid
				s.saveTask(t)
			}
			s.mu.Unlock()
			timeout, _ := time.ParseDuration(s.st.Cfg.RunTimeout)
			if timeout <= 0 {
				timeout = 45 * time.Minute
			}
			timer := time.AfterFunc(timeout, func() { killGroup(cmd) })
			err = cmd.Wait()
			timer.Stop()
			code = 0
			if err != nil {
				code = 1
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				}
			}
		}
		fmt.Fprintf(logf, "--- %s exit %d\n", now(), code)
		logf.Close()
	}
	if err != nil && code == 127 {
		s.board.Post(Event{Kind: "blocked", Task: name, Who: seat, Text: engine + " could not start: " + err.Error()})
	}
	if routed && (engine == "deepseek" || engine == "qwen") {
		s.laneFilter(name, engine, start, code == 0)
	}
	s.afterRun(name, engine, code, start) // before the seat is released, so the scheduler can't park it first

	s.mu.Lock()
	if t, ok := s.tasks[name]; ok {
		t.LastExit, t.RunsTotal, t.LastEnd, t.RunPID = code, t.RunsTotal+1, now(), 0
		if code != 0 && time.Since(start) < 30*time.Second {
			t.QuickFails++
		} else {
			t.QuickFails = 0
		}
		if t.QuickFails >= 3 && t.State == "active" {
			t.State = "blocked"
			s.board.Post(Event{Kind: "blocked", Task: name, Who: seat, Text: "3 runs in a row died within 30 s (see .osenv/tasks/" + name + "/run.log); task.resume after fixing"})
		}
		if code == 0 {
			if t.Engines == nil {
				t.Engines = map[string]int{}
			}
			t.Engines[engine]++
		}
		// The tripwire: a real run makes tool calls, and every one passes the hook. None in a
		// minute or more means Jev saw nothing (VM test, 0.1: a whole night ran unscored).
		if err == nil && time.Since(start) >= hookGrace && s.hookN[name] == hooks0 && t.State == "active" {
			t.State = "blocked"
			s.board.Post(Event{Kind: "blocked", Task: name, Who: "osenv", Text: fmt.Sprintf("%s ran %s and Jev saw none of its actions: its hooks never reached osenv, so nothing was scored. Check the hook command in %s, then task.resume.",
				engine, time.Since(start).Round(time.Second), hookFile(engine))})
		}
		s.saveTask(t)
	}
	delete(s.live, name)
	s.mu.Unlock()
	s.board.Post(Event{Kind: "run.end", Task: name, Who: seat, Text: fmt.Sprintf("%s exit %d after %s", engine, code, time.Since(start).Round(time.Second)),
		Data: map[string]any{"engine": engine, "exit": code}})
}

// ---- engines ---------------------------------------------------------------------------------

func (s *Server) hooks(name, role string) map[string]any {
	cmd := func(ev string) []map[string]any {
		return []map[string]any{{"matcher": "*", "hooks": []map[string]any{{"type": "command",
			"command": fmt.Sprintf(`%s hook %s --task %s --role %s --url %s`, s.cli, ev, name, role, s.url)}}}}
	}
	return map[string]any{"PreToolUse": cmd("pre"), "PostToolUse": cmd("post")}
}

func (s *Server) command(name, seat, engine, eff, reason, lessons, tools string) (*exec.Cmd, error) {
	cfg := s.st.Cfg
	var takeIDs []string
	s.mu.Lock()
	if t, ok := s.tasks[name]; ok {
		takeIDs = append(takeIDs, t.Takes...)
	}
	s.mu.Unlock()
	fill := func(tpl string) string {
		project, _ := os.ReadFile(s.st.path("PROJECT.md"))
		kicks, _ := os.ReadFile(s.tdir(name, "kicks-"+engine+".txt"))
		if lessons == "" {
			lessons = "(none that Jev judged relevant to your next steps)\n"
		}
		if tools == "" {
			tools = "(none in exec/ that fit this job yet)\n"
		}
		return strings.NewReplacer(
			"{{SEAT}}", seat, "{{TASK}}", name, "{{RUN}}", s.cli, "{{ROOT}}", s.st.Root, "{{DIR}}", ".osenv/tasks/"+name,
			"{{URL}}", s.url, "{{PROJECT}}", strings.TrimSpace(string(project)), "{{JOB}}", strings.TrimSpace(s.job(name)),
			"{{LEARNED}}", strings.TrimRight(lessons, "\n"), "{{TOOLS}}", strings.TrimRight(tools, "\n"), "{{MEMORIES}}", s.memories(takeIDs), "{{REASON}}", reason, "{{MODEL}}", engine,
			"{{LANE}}", lanes[engine], "{{KICKS}}", tail(string(kicks), 3), "{{CADENCE}}", fmt.Sprint(cfg.Cadence), "{{PLATFORM}}", platformNotes(),
		).Replace(tpl)
	}
	env := append(os.Environ(), "OSENV_TASK="+name, "OSENV_URL="+s.url, "OSENV_ROLE="+engine)
	// Engines and everything they start keep temp files in the task folder, never the machine's /tmp (video bench #3:
	// the muse CLI made /tmp/muse-* dirs itself). The folder goes when the task retires.
	tmp := s.tdir(name, "tmp")
	os.MkdirAll(tmp, 0o755)
	env = append(env, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	// The brief goes in a file, never on the command line: on Linux a hybrid's own `pkill -f api/server.py`
	// matched the job text in muse's argv and killed muse mid-run (0.3 proof bench, exit 143). Windows needed
	// the file anyway (command-line length). Every engine reads it as its first step.
	argPrompt := func(full string) string {
		p := s.tdir(name, "BRIEF-"+engine+".md")
		os.WriteFile(p, []byte(full), 0o644)
		return "Your full instructions for this run are in " + p + ". Read that whole file first, then follow it exactly."
	}
	var c *exec.Cmd
	switch engine {
	case "muse":
		xdg := s.tdir(name, "xdg")
		os.MkdirAll(filepath.Join(xdg, "muse"), 0o755)
		writeJSON(filepath.Join(xdg, "muse", "settings.json"), map[string]any{"schema_version": 1, "hooks": s.hooks(name, "muse")})
		if auth := home(cfg.Muse.Auth); fileExists(auth) {
			dst := filepath.Join(xdg, "muse", "auth.json")
			os.Remove(dst)
			if os.Symlink(auth, dst) != nil { // Windows needs admin or developer mode for symlinks: copy instead
				if b, err := os.ReadFile(auth); err == nil {
					os.WriteFile(dst, b, 0o600)
				}
			}
		}
		c = exec.Command(cfg.Muse.Cmd, "exec", "--model", cfg.Muse.Model, "--reasoning-effort", eff, "--yolo",
			"--approval-judge", "off", argPrompt(fill(mustRead("prompts/brief.md"))))
		env = append(env, "XDG_CONFIG_HOME="+xdg)
	case "deepseek", "qwen":
		model := cfg.Plan.Deepseek
		if engine == "qwen" {
			model = cfg.Plan.Qwen
		}
		key := readKey("OSENV_PLAN_KEY", cfg.Plan.KeyFile)
		if key == "" {
			return nil, fmt.Errorf("no plan key for %s: set OSENV_PLAN_KEY or plan.key_file", engine)
		}
		qhome := s.tdir(name, "qhome")
		os.MkdirAll(filepath.Join(qhome, ".qwen"), 0o755)
		writeJSON(filepath.Join(qhome, ".qwen", "settings.json"), map[string]any{"hooks": s.hooks(name, engine),
			"modelProviders": map[string]any{"openai": []map[string]any{{"id": model, "envKey": "OPENAI_API_KEY",
				"baseUrl": cfg.Plan.URL, "capabilities": map[string]any{"vision": true},
				"generationConfig": map[string]any{"modalities": map[string]any{"image": true}}}}}})
		sheetNote, turns := "", cfg.Turns
		if engine == "qwen" {
			if p, names := s.contactSheet(name); p != "" {
				turns = min(turns, qwenSheetTurns)
				sheetNote = fmt.Sprintf("CONTACT SHEET: %s holds every screenshot of the work in ONE image, tiled left to right, top to bottom: %s. A page taller than its tile shows its top and its bottom, with a red band where the middle was cut. Read that one image first and judge from it. Open a crop (%s view <file> --crop x,y,w,h) only for a detail you can't judge on the sheet. This review has at most %d turns: write your feedback file early.\n",
					p, strings.Join(names, ", "), s.cli, turns)
			}
		}
		prompt := strings.Replace(fill(mustRead("prompts/routed.md")), "{{SHEET}}", sheetNote, 1) + "\n\n" + fill(mustRead("prompts/brief.md"))
		c = exec.Command(cfg.Plan.Cmd, "--auth-type", "openai", "--openai-base-url", cfg.Plan.URL, "-m", model,
			"--approval-mode", "yolo", "--max-session-turns", fmt.Sprint(turns), argPrompt(prompt))
		env = append(env, "HOME="+qhome, "USERPROFILE="+qhome, "OPENAI_API_KEY="+key, "QWEN_CODE_SUPPRESS_YOLO_WARNING=1") // USERPROFILE: Windows
	case "opus":
		set := s.tdir(name, "claude-settings.json")
		writeJSON(set, map[string]any{"hooks": s.hooks(name, "opus")})
		c = exec.Command(cfg.Claude.Cmd, "-p", argPrompt(fill(mustRead("prompts/corrector.md"))), "--model", cfg.Claude.Model,
			"--effort", cfg.Claude.Effort, "--settings", set, "--max-turns", fmt.Sprint(cfg.Turns),
			"--permission-mode", "bypassPermissions") // headless like muse --yolo: Jev's hook is the gate
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
	c.Dir, c.Env = s.st.Root, env
	return c, nil
}

var lanes = map[string]string{
	// Windows 0.2.3 #18: a chart's number formatting was dropped as out of lane (0.48-0.54); with "reads on screen"
	// and "labels and number formatting" it scores 0.82, while code items stay at 0.05-0.08.
	"qwen":     "computer use, visual design, how something looks or reads on screen (renders, UI, art, color, composition, labels and number formatting) or creative direction",
	"deepseek": "code and how it behaves: scripts, logic, APIs and protocols, tests, build, gates, terminal commands or file handling",
}

// laneFilter keeps only the routed reviewer's in-lane feedback and hands it to muse under STATE.
// Out-of-lane items go back to that reviewer as a kick it sees next time it is called in.
func (s *Server) laneFilter(name, model string, began time.Time, ended bool) {
	p := s.tdir(name, "feedback-"+model+".jsonl")
	f, err := os.Open(p)
	if err != nil {
		if ended {
			s.board.Post(Event{Kind: "feedback", Task: name, Who: model, Text: "the review wrote no feedback file"})
		}
		return
	}
	var items []string
	type lessonIn struct{ rule, detect string }
	lessons := map[int]lessonIn{} // reviewer findings muse could repeat: they go through Jev's intake too
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r struct{ Item, Rule, Detect string }
		if json.Unmarshal(sc.Bytes(), &r) == nil && strings.TrimSpace(r.Item) != "" {
			if strings.TrimSpace(r.Detect) != "" {
				lessons[len(items)] = lessonIn{strings.TrimSpace(r.Rule), strings.TrimSpace(r.Detect)}
			}
			items = append(items, strings.TrimSpace(r.Item))
		}
	}
	f.Close()
	os.Rename(p, p+"."+time.Now().UTC().Format("20060102T150405Z")+".merged")
	if len(items) == 0 && !ended { // a crash can leave an empty file: that's no clean review
		s.board.Post(Event{Kind: "feedback", Task: name, Who: model, Text: "the review ended early and left no findings; it may not have finished"})
		return
	}
	if len(items) == 0 { // a clean review: say so, or muse sees nothing and asks again (Windows light run: a second qwen review)
		s.mu.Lock()
		s.setNotes(name, sectionAfterState(s.notes(name), "## FEEDBACK ("+model+", "+now()[:16]+")\n- ("+model+") No findings: the review found nothing to fix. Don't ask for this review again unless the work changes.\n"))
		s.mu.Unlock()
		s.board.Post(Event{Kind: "feedback", Task: name, Who: model, Text: "no findings: a clean review"})
		return
	}
	qs := map[string]Q{}
	for i, it := range items {
		if len(it) > 800 {
			it = it[:800]
		}
		qs[fmt.Sprintf("i%d", i)] = Noul("This feedback item is about " + lanes[model] + ", and nothing else: \"" + it + "\"")
	}
	a, err := ask(map[string]any{"reviewer": model}, qs)
	if err != nil {
		time.Sleep(5 * time.Second)
		a, err = ask(map[string]any{"reviewer": model}, qs)
	}
	var keep, kicks, dropped []string
	seen := map[string]bool{} // lessons this review already touched: a duplicate finding isn't an escape
	for i, it := range items {
		switch p := a[fmt.Sprintf("i%d", i)].P(); {
		case err != nil:
			keep = append(keep, "- ("+model+", unchecked: Jev was down) "+it)
		// Measured on the real Jev: in-lane "keep as-is / no change needed" notes 0.53-0.75, out-of-lane
		// items 0.02-0.10 (Linux I-4: at 0.6 the confirmations were dropped and muse "fixed" correct code).
		case p >= 0.4:
			keep = append(keep, "- ("+model+") "+it)
			if l, ok := lessons[i]; ok && p >= 0.6 { // clearly in lane and repeatable: Jev decides general rule vs this-task noise
				text := l.rule
				if text == "" {
					text = it
				}
				if li, how, err := s.learn.Add(AddIn{Task: name, Text: text, Detect: l.detect, Source: model, Evidence: "feedback-" + model, Seen: seen, Newer: s.ordersSince(name, began)}); li == nil && err == nil {
					s.board.Post(Event{Kind: "learn", Task: name, Who: model, Text: how + ": " + clip(text, 200)})
				} else if err == nil {
					seen[li.ID] = true
					s.board.Post(Event{Kind: "learn", Task: name, Who: model, Text: fmt.Sprintf("%s %s (%s, %s): %s", how, li.ID, li.Kind, li.Scope, clip(li.Text, 160))})
				}
			}
		default:
			kicks = append(kicks, fmt.Sprintf("JEV KICKBACK: %q is outside your lane (%s). Dropped; muse never saw it.", clip(it, 120), lanes[model]))
			dropped = append(dropped, clip(it, 200))
		}
	}
	if len(keep) > 0 {
		s.mu.Lock()
		s.setNotes(name, sectionAfterState(s.notes(name), "## FEEDBACK ("+model+", lane-checked by Jev "+now()[:16]+")\n"+strings.Join(keep, "\n")+"\n"))
		s.mu.Unlock()
	}
	if len(kicks) > 0 {
		kf, _ := os.OpenFile(s.tdir(name, "kicks-"+model+".txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		kf.WriteString(strings.Join(kicks, "\n") + "\n")
		kf.Close()
	}
	txt := fmt.Sprintf("%d kept, %d dropped as out of lane", len(keep), len(kicks))
	if len(dropped) > 0 { // Windows 0.2.3 #18: the desk sees what muse never will, so a dropped fix isn't lost silently
		txt += ". Dropped (the desk may order any of these): " + strings.Join(dropped, " | ")
	}
	s.board.Post(Event{Kind: "feedback", Task: name, Who: model, Text: txt})
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func tail(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// jobClip: how much of a job Jev and the plain check see: all of it, gates included. Jobs run to about 5000 characters,
// and the requirements live at the end. Cut shorter (video bench): a screenshot the gates name scored creep 0.80 (0.38-0.40
// whole), the park gate missed "ask for a visual and a code review" at character 5046 (0.36, 0.95 whole), and the
// plain check passed a timeline missing the file names the job asks for at character 1834.
// ponytail: a job past 6000 characters loses its tail; send its Gates section on its own if jobs grow that long.
const jobClip = 6000

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// platformNotes: facts about the machine muse is on that it would otherwise learn the hard way.
// OSENV_PLATFORM_NOTES=off (set before serve) drops them, to prove the learned loop catches the mistake on its own.
func platformNotes() string {
	if os.Getenv("OSENV_PLATFORM_NOTES") == "off" {
		return ""
	}
	if runtime.GOOS != "windows" {
		// Linux light rc8: the shell tool ends a & job when the call returns, so muse started servers and windows with
		// setsid -f (no $!) and then looked their PIDs up by name, which the kill check denies.
		return "LINUX: your shell tool ends a background job (&) when the call returns, and setsid -f gives no $!. To start something you stop later, save its PID from inside: setsid -f sh -c 'echo $$ > \"$TMPDIR/<name>.pid\"; exec <command>', then stop it with kill $(cat \"$TMPDIR/<name>.pid\"). Never look a process up by name, command line or window title to stop it.\n\n"
	}
	return "WINDOWS (PowerShell 5.1): save command output with cmd /c \"<command> > <file> 2>&1\" or | Out-File -Encoding utf8, never with PowerShell's own > (it writes UTF-16LE, which other tools read as binary). curl here is not real curl: post to the board with the do lines below, and put text with a $ in single quotes (text='costs $135'): in double quotes PowerShell swallows $135. Use python or py, not python3. A Python server here can bind a port another process already holds: check the port is free first (Get-NetTCPConnection -LocalPort <n>), and stop every server you start before you park, by the PID you kept: $p = Start-Process ... -PassThru, then Stop-Process -Id $p.Id.\n\n"
}

var hookGrace = time.Minute // a run this long with no hook call trips the wire

func hookFile(engine string) string {
	switch engine {
	case "muse":
		return "the seat's xdg/muse/settings.json"
	case "opus":
		return "the seat's claude-settings.json"
	}
	return "the seat's qhome/.qwen/settings.json"
}

// gateAtPark: muse parked STEP DONE. If the job requires a review that never happened, Jev
// holds the park and routes that review first (VM test: a visual gate was skipped, the desk
// had to order it). The WAITING line goes, so muse acts on the review before parking again.
func (s *Server) gateAtPark(name string) {
	if !waiting(s.notes(name)) {
		return
	}
	// Byte-identical screenshots mean a state one of them should show wasn't captured (Linux video bench: muse reported
	// "15a=3%, 15b=53%" for two identical shots both at 0%, and 15b was 16's twin). A hash finds it exactly; a model
	// told about it ignored it.
	if same := sameImages(s.st.Root, s.taskImages(name, 40)); len(same) > 0 {
		k := "identical: " + strings.Join(same, ", ")
		s.mu.Lock()
		if s.held[name] == k { // intended twins: the desk decides
			delete(s.held, name)
			s.mu.Unlock()
			s.board.Post(Event{Kind: "gate", Task: name, Who: "osenv", Text: "the same screenshots are still identical (" + strings.Join(same, ", ") + "); not held again, the desk decides"})
		} else {
			s.held[name] = k
			n := dropLines(s.notes(name), func(m, _ string) bool { return strings.HasPrefix(m, "WAITING: desk review") })
			s.setNotes(name, underState(n, "- FIX BEFORE STEP DONE: these screenshots are byte-for-byte the same image, so a state one of them should show wasn't captured: "+strings.Join(same, ", ")+". Take them again and open each one before you keep it, then park again. (added by osenv)"))
			s.mu.Unlock()
			s.board.Post(Event{Kind: "gate", Task: name, Who: "osenv", Text: "STEP DONE held: identical screenshots " + strings.Join(same, ", ")})
			return
		}
	}
	// A shipped file that points into the task folder breaks the moment the task passes: that folder is
	// deleted on retire (Linux run, task 6: a test read .osenv/tasks/refactor/baseline/ and failed after PASS).
	// A text search finds it exactly, so no Jev call.
	born, _ := time.Parse(time.RFC3339, t0Created(s, name))
	if files, lines := refsToTaskDir(s.st.Root, name, born); len(files) > 0 {
		// A file no action of this task wrote isn't muse's to fix (Windows light rc8: muse rewrote the desk's own
		// test file into one of its own): the desk decides at once.
		own, mine := s.acts(name, 200), false
		for _, f := range files {
			mine = mine || writerOf(own, f) != nil
		}
		s.mu.Lock()
		if !mine {
			delete(s.held, name)
			s.mu.Unlock()
			s.board.Post(Event{Kind: "gate", Task: name, Who: "osenv", Text: strings.Join(files, ", ") + " point into the task folder, which is deleted on PASS, and no action of this task wrote them; not held, the desk decides: " + strings.Join(lines, " | ")})
			goto reviews
		}
		if k := strings.Join(files, ", "); s.held[name] == k { // VM 0.2.3 #14: muse can't fix it (the desk's own job file named the path): don't loop, the desk decides
			delete(s.held, name)
			s.mu.Unlock()
			s.board.Post(Event{Kind: "gate", Task: name, Who: "osenv", Text: "the same files still point into the task folder (" + k + "); not held again, the desk decides"})
			goto reviews
		}
		s.held[name] = strings.Join(files, ", ")
		n := dropLines(s.notes(name), func(m, _ string) bool { return strings.HasPrefix(m, "WAITING: desk review") })
		s.setNotes(name, underState(n, "- FIX BEFORE STEP DONE: "+strings.Join(files, ", ")+" use .osenv/tasks/"+name+"/, which is deleted when this task passes. Move what they need into the project (a comment that names the folder goes stale too), re-run the gates, then park again. Where: "+strings.Join(lines, " | ")+" (added by osenv)"))
		s.mu.Unlock()
		s.board.Post(Event{Kind: "gate", Task: name, Who: "osenv", Text: "STEP DONE held: " + strings.Join(files, ", ") + " depend on the task folder, which is deleted on PASS: " + strings.Join(lines, " | ")})
		return
	}
reviews:
	s.mu.Lock()
	t, ok := s.tasks[name]
	var done map[string]int
	if ok {
		done = map[string]int{"visual review (qwen, or a passed plain check when that was all the job needed)": t.Engines["qwen"] + t.Engines["plain"],
			"code review (deepseek)": t.Engines["deepseek"]}
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	// The desk's orders can waive a review the job names (Linux video bench: "skip the reviews" for a wrap-up, and
	// the gate routed qwen anyway). Measured on the real Jev with the orders shown: waived 0.16-0.18; no order or an
	// unrelated one 0.92-0.95. The old wording, which asked about the job text alone, scored the waiver 0.69-0.70.
	a, err := ask(map[string]any{"job": clip(s.job(name), jobClip), "reviews_done_so_far": done, "orders": orders(s.notes(name))}, map[string]Q{
		"visual": Noul("The job requires a visual review (of how something looks) before the work is handed in, the desk's orders have not waived it, and the record shows none has been done yet."),
		"code":   Noul("The job requires a code review before the work is handed in, the desk's orders have not waived it, and the record shows none has been done yet."),
	})
	if err != nil {
		return // Jev down: park as usual, the desk still checks
	}
	var add []string
	if a["visual"].P() >= 0.7 {
		add = append(add, "- ESCALATE: visual review of the deliverables (the job requires it before STEP DONE; added by osenv)")
	}
	if a["code"].P() >= 0.7 {
		add = append(add, "- ESCALATE: code review of the changes (the job requires it before STEP DONE; added by osenv)")
	}
	if len(add) == 0 {
		return
	}
	s.mu.Lock()
	n := dropLines(s.notes(name), func(m, _ string) bool { return strings.HasPrefix(m, "WAITING: desk review") })
	s.setNotes(name, underState(n, strings.Join(add, "\n")))
	s.mu.Unlock()
	s.board.Post(Event{Kind: "gate", Task: name, Who: "jev", Text: "STEP DONE held: the job requires a review that hasn't happened; routing it first"})
}

// plainCheck: before a qwen review, one cheap deepseek call looks at the task's newest screenshots for
// plain faults only. Linux run: qwen's 20-minute review failed on a screenshot cut off mid-page; deepseek
// flagged that same capture in one call (and passed the full one). It fails open: no screenshots, no key
// or a dead endpoint means qwen runs as before.
func (s *Server) plainCheck(name string) (verdict, text string) {
	cfg := s.st.Cfg
	key := readKey("OSENV_PLAN_KEY", cfg.Plan.KeyFile)
	imgs := s.taskImages(name, 8) // a job's gates can name 5 screenshots: with 4 sent, one read as "not provided" (video bench)
	if key == "" || len(imgs) == 0 {
		return "", ""
	}
	content := []map[string]any{{"type": "text", "text": plainPrompt + "\n\nTHE JOB:\n" + clip(s.job(name), jobClip)}}
	for _, p := range imgs {
		if u := jpegDataURL(p, 640, 3000); u != "" {
			content = append(content, map[string]any{"type": "text", "text": "Screenshot " + filepath.Base(p) + ":"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
	}
	// deepseek-v4.1-flash reasons before it answers: with 4000 tokens, tall screenshots used them all on
	// reasoning and the answer came back empty (Linux 0.3 run; measured 2,923 reasoning tokens on a retry).
	// No thinking: measured live on 5 screenshots, thinking ran 2 minutes and used up 12000 tokens without an answer
	// (Windows video bench #19); without it, 5 seconds and a verdict. The check fails open, and qwen reviews anyway.
	body, _ := json.Marshal(map[string]any{"model": cfg.Plan.Deepseek, "max_tokens": 12000, "enable_thinking": false,
		"messages": []map[string]any{{"role": "user", "content": content}}})
	req, err := http.NewRequest("POST", strings.TrimRight(cfg.Plan.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 4 * time.Minute}).Do(req)
	if err != nil {
		return "", "no verdict: " + err.Error()
	}
	defer resp.Body.Close()
	var r struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct{ Content string }
		} `json:"choices"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || len(r.Choices) == 0 {
		return "", fmt.Sprintf("no verdict: HTTP %d with no answer", resp.StatusCode)
	}
	text = strings.TrimSpace(r.Choices[0].Message.Content)
	if text == "" {
		return "", "no verdict: an empty answer (finish: " + r.Choices[0].FinishReason + ")"
	}
	switch up := strings.ToUpper(text); {
	case strings.Contains(up, "VERDICT: FAIL"):
		return "fail", text
	case strings.Contains(up, "VERDICT: PASS"):
		return "pass", text
	}
	return "", "no verdict line in the answer: " + clip(text, 200)
}

// plainOnly: a visual review needs nothing beyond the plain checks that just passed, so qwen, the
// priciest model, can stay out. A job that names qwen always gets qwen. Otherwise Jev must be sure (0.7).
// Measured on the real Jev: 3 plain jobs scored 0.75-0.88 and 3 taste jobs 0.27-0.38 on this wording;
// a stacked question and a "calls for taste" wording both left plain jobs unsure.
func (s *Server) plainOnly(name string) bool {
	job := s.job(name)
	if strings.Contains(strings.ToLower(job), "qwen") {
		return false
	}
	a, err := ask(map[string]any{"job": clip(job, 2000)}, map[string]Q{"plain": Noul(plainOnlyQ)})
	return err == nil && a["plain"].P() >= 0.7
}

const plainOnlyQ = "Checking this job's result needs no opinion on how good anything looks, only whether the required things are present, complete and readable."

const plainPrompt = `You check screenshots for PLAIN faults only, never taste or style:
1) Is any screenshot cut off: a page that stops mid-content, or a missing bottom or footer?
2) Is everything the job asks to be shown there and complete: charts, tables, labels, titles, text? Judge presence and completeness only, never values: don't check whether a number or total is right (the tests do that).
3) Is anything clipped, overlapping or unreadable?
4) Is there an error screen, an empty area where content should be, or placeholder content?
Answer each with PASS or FAIL and one line of evidence naming the screenshot. End with exactly one line: VERDICT: PASS or VERDICT: FAIL.`

// taskImages: this task's newest screenshots: those in its out/ folder, and project images its job, notes or
// actions name (VM 0.2.3 #7: the screenshot was a deliverable in pages/, so the plain check never ran). Named,
// whatever their age (0.3 sheet bench: a review of screenshots made before the task found none), and never
// just new: a parallel task's screenshots are never judged here.
func (s *Server) taskImages(name string, n int) []string {
	named := s.job(name) + "\n" + s.notes(name)
	for _, a := range s.acts(name, 200) {
		w, _ := a["what"].(string)
		named += "\n" + w
	}
	// Screenshots made during the task come first. Older named ones count only when the task made none (a review
	// of existing screenshots): a job naming an old image to protect it put a stale shot on qwen's sheet (Linux 0.3 run).
	born, _ := time.Parse(time.RFC3339, t0Created(s, name))
	fresh := newestImages(s.tdir(name, "out"), n, time.Time{})
	var old []string
	for _, p := range newestImages(s.st.Root, 1<<20, time.Time{}) { // ponytail: walks every image; index them if big asset trees stall
		if strings.Contains(named, filepath.Base(p)) {
			if mtime(p).Before(born.Add(-time.Second)) {
				old = append(old, p)
			} else {
				fresh = append(fresh, p)
			}
		}
	}
	all := fresh
	if len(all) == 0 {
		all = old
	}
	sort.SliceStable(all, func(i, j int) bool { return mtime(all[i]).After(mtime(all[j])) })
	return all[:min(n, len(all))]
}

func mtime(p string) time.Time {
	if info, err := os.Stat(p); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// newestImages: the newest png/jpg files under dir written after since, newest first (skips .osenv, .git, deps).
func newestImages(dir string, n int, since time.Time) []string {
	skip := map[string]bool{".osenv": true, ".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true}
	var fs []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".")) { // hidden: caches and test temp (.pytest-tmp)
				return filepath.SkipDir
			}
			return nil
		}
		if e := strings.ToLower(filepath.Ext(p)); (e == ".png" || e == ".jpg" || e == ".jpeg") && !mtime(p).Before(since) {
			fs = append(fs, p)
		}
		return nil
	})
	sort.Slice(fs, func(i, j int) bool { return mtime(fs[i]).After(mtime(fs[j])) })
	return fs[:min(n, len(fs))]
}

// jpegDataURL shrinks an image to fit maxW x maxH (a tall page keeps readable text) as an inline JPEG.
func jpegDataURL(p string, maxW, maxH int) string {
	fh, err := os.Open(p)
	if err != nil {
		return ""
	}
	img, _, err := image.Decode(fh)
	fh.Close()
	if err != nil {
		return ""
	}
	b := img.Bounds()
	scale := math.Min(1, math.Min(float64(maxW)/float64(b.Dx()), float64(maxH)/float64(b.Dy())))
	out := boxResize(img, b, max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale)))
	var buf bytes.Buffer
	if jpeg.Encode(&buf, out, &jpeg.Options{Quality: 85}) != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// refsToTaskDir: project files (outside .osenv and other tool folders) that mention this task's folder.
// t0Created: the task's creation time (files older than it aren't the hybrid's doing).
func t0Created(s *Server, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[name]; ok {
		return t.Created
	}
	return ""
}

// ponytail: reads up to 20k files per park; fine for small and medium projects, index by mtime if big repos stall.
func refsToTaskDir(root, name string, since time.Time) (hits, lines []string) {
	// /, \ and escaped \\, and a path built from parts: os.path.join(REPO, ".osenv", "tasks", "testkit", "out") or
	// Path(".osenv") / "tasks" / name. Windows video bench: a joined path slipped past, and every later pytest run
	// recreated the retired task's folder.
	sep := `["')\s]*[,/\\+]+[\s"'(]*`
	needle := regexp.MustCompile(`\.osenv` + sep + `tasks` + sep + regexp.QuoteMeta(name) + `\b`)
	skip := map[string]bool{".osenv": true, ".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true}
	seen := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(hits) >= 10 || seen > 20000 {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if p != root && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".")) { // hidden: tool caches (Linux light run: .pytest_cache/v/cache/nodeids held a park)
				return filepath.SkipDir
			}
			return nil
		}
		seen++
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 || info.ModTime().Before(since) {
			return nil // only files touched during this task: a doc the desk wrote earlier never holds the park
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if loc := needle.FindIndex(b); loc != nil {
			i := loc[0]
			rel, _ := filepath.Rel(root, p)
			hits = append(hits, filepath.ToSlash(rel))
			// the first line that names it, so the fix takes seconds (Windows: muse spent a 4.5-minute run finding docstring mentions)
			start := bytes.LastIndexByte(b[:i], '\n') + 1
			end := bytes.IndexByte(b[i:], '\n')
			if end < 0 {
				end = len(b) - i
			}
			lines = append(lines, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), bytes.Count(b[:i], []byte("\n"))+1, clip(strings.TrimSpace(string(b[start:i+end])), 160)))
		}
		return nil
	})
	return hits, lines
}

// ordersSince: the desk's orders and verdicts for this task posted after t, newest last.
func (s *Server) ordersSince(name string, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	var out []string
	for _, e := range s.board.Since(0, name, []string{"orders", "verdict"}, 200) {
		if at, err := time.Parse(time.RFC3339, e.At); err == nil && !at.Before(t.Add(-time.Second)) && e.Who == "desk" {
			out = append(out, e.Text)
		}
	}
	return strings.Join(out, "\n")
}

// orphanEnded does what the server that started a run would have done at its end, had it not died first: close the
// run's undo record, merge a routed review's findings into NOTES before muse runs again, and count the run. Video
// bench: an orphaned deepseek review's findings sat unmerged, and muse's next run went without them.
func (s *Server) orphanEnded(name string, killed bool) {
	s.undoCloseOpen(name)
	engine, routed, began := "", false, time.Time{}
	if ev := s.board.Since(0, name, []string{"run.start"}, 1); len(ev) == 1 { // the old server's last start is this run
		engine, _ = ev[0].Data["engine"].(string)
		routed, _ = ev[0].Data["routed"].(bool)
		began, _ = time.Parse(time.RFC3339, ev[0].At)
	}
	if routed && (engine == "deepseek" || engine == "qwen") {
		s.laneFilter(name, engine, began, !killed)
	}
	s.mu.Lock()
	if t, ok := s.tasks[name]; ok && engine != "" {
		t.RunsTotal, t.LastEnd = t.RunsTotal+1, now()
		if !killed { // its exit code died with the old server; a run that ended on its own counts as done
			if t.Engines == nil {
				t.Engines = map[string]int{}
			}
			t.Engines[engine]++
		}
		s.saveTask(t)
	}
	delete(s.orphans, name)
	s.mu.Unlock()
	what := "the run from before the restart has ended"
	if engine != "" {
		what = "the " + engine + " run from before the restart has ended"
	}
	if killed {
		what = strings.Replace(what, "has ended", "was stopped at the run timeout", 1)
	}
	s.board.Post(Event{Kind: "orphan", Task: name, Who: "osenv", Text: what + "; runs resume"})
}

// sameImages: byte-identical images under different names ("15a-mid.png" and "15b-mid.png" should show two states),
// as project paths "a = b". Copies under one name are left alone: a proof copied from out/, or a deterministic test's
// frame.png in two basetemps (Windows video bench: the first hold fired on exactly those).
func sameImages(root string, paths []string) []string {
	by := map[[32]byte][]string{}
	var order [][32]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		h := sha256.Sum256(b)
		if by[h] == nil {
			order = append(order, h)
		}
		rel, _ := filepath.Rel(root, p)
		by[h] = append(by[h], filepath.ToSlash(rel))
	}
	var out []string
	for _, h := range order {
		names := map[string]bool{}
		for _, p := range by[h] {
			names[path.Base(p)] = true
		}
		if len(names) > 1 {
			sort.Strings(by[h])
			out = append(out, strings.Join(by[h], " = "))
		}
	}
	return out
}

// afterRun: a run that ended well may have parked the seat. A run that posted STEP DONE but left no WAITING line is
// parked as if it had written one (Linux light run: after a hold, muse posted STEP DONE three times without writing
// WAITING back and was re-run with nothing new, then an Opus run). The park gate runs after Opus too, since the
// corrector can park the seat (Linux light run: a twin-screenshot probe slipped past an Opus park).
func (s *Server) afterRun(name, engine string, code int, start time.Time) {
	if code != 0 || (engine != "muse" && engine != "opus") {
		return
	}
	if !waiting(s.notes(name)) {
		for _, e := range s.board.Since(0, name, []string{"say"}, 50) {
			at, err := time.Parse(time.RFC3339, e.At)
			if err == nil && !at.Before(start.Add(-time.Second)) && e.Who != "desk" && strings.HasPrefix(strings.TrimSpace(e.Text), "STEP DONE") {
				s.mu.Lock()
				s.setNotes(name, underState(s.notes(name), "- WAITING: desk review (added by osenv: the run posted STEP DONE but didn't park)"))
				s.mu.Unlock()
				break
			}
		}
	}
	s.gateAtPark(name)
}
