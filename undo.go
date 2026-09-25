package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Undo per run: before and after every engine run osenv records the project's files (path -> content hash;
// the content itself is stored once, shared across runs, under .osenv/undo/blobs). task.undo puts back what
// that run changed, and only that: a file changed again after the run, or touched by another hybrid while the
// run was going, is left alone and named (force=true overrides the second). Only files whose size or time
// changed are hashed again, so a big project costs a stat walk. A task's snapshots go when it retires.

const undoMaxFile = 8 << 20 // bigger files are recorded but not kept: undo names them instead

type snapEntry struct {
	Sha  string `json:"sha,omitempty"` // empty: too big to keep
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"`
}

type undoRun struct {
	N      int                  `json:"n"`
	Engine string               `json:"engine"`
	Start  string               `json:"start"`
	End    string               `json:"end,omitempty"`
	Before map[string]snapEntry `json:"before"`
	After  map[string]snapEntry `json:"after,omitempty"`
}

var snapMu sync.Mutex
var snapCache = map[string]snapEntry{} // root|path -> last known entry: unchanged size and time skip the hash

// snap records every project file (outside .osenv, .git and dependency folders).
func (s *Server) snap() map[string]snapEntry {
	root := s.st.Root
	blobs := s.st.path("undo", "blobs")
	os.MkdirAll(blobs, 0o755)
	skip := map[string]bool{".osenv": true, ".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true, ".pytest_cache": true}
	out := map[string]snapEntry{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		e := snapEntry{Size: info.Size(), Mod: info.ModTime().UnixNano()}
		snapMu.Lock()
		c, ok := snapCache[root+"|"+rel]
		snapMu.Unlock()
		if ok && c.Size == e.Size && c.Mod == e.Mod && (c.Sha == "" || fileExists(filepath.Join(blobs, c.Sha))) {
			out[rel] = c
			return nil
		}
		if e.Size <= undoMaxFile {
			if b, err := os.ReadFile(p); err == nil {
				h := sha256.Sum256(b)
				e.Sha = hex.EncodeToString(h[:])
				if bp := filepath.Join(blobs, e.Sha); !fileExists(bp) {
					tmp := bp + ".tmp" + fmt.Sprint(time.Now().UnixNano())
					if os.WriteFile(tmp, b, 0o644) == nil {
						os.Rename(tmp, bp)
					}
				}
			}
		}
		snapMu.Lock()
		snapCache[root+"|"+rel] = e
		snapMu.Unlock()
		out[rel] = e
		return nil
	})
	return out
}

func (s *Server) undoPath(task string, n int) string {
	return s.tdir(task, "undo", fmt.Sprintf("run-%03d.json", n))
}

// undoBefore records the project before a run and returns its number.
func (s *Server) undoBefore(task, engine string) int {
	ents, _ := filepath.Glob(s.tdir(task, "undo", "run-*.json"))
	n := len(ents) + 1
	os.MkdirAll(s.tdir(task, "undo"), 0o755)
	writeJSON(s.undoPath(task, n), undoRun{N: n, Engine: engine, Start: now(), Before: s.snap()})
	return n
}

// undoAfter records the project after the run.
func (s *Server) undoAfter(task string, n int) {
	var u undoRun
	if b, err := os.ReadFile(s.undoPath(task, n)); err != nil || json.Unmarshal(b, &u) != nil {
		return
	}
	u.End, u.After = now(), s.snap()
	writeJSON(s.undoPath(task, n), u)
}

type undoSkip struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// taskUndo puts back what run n (0: the latest finished run) changed. The task must not be running.
// dry: say what it would do, touch nothing.
func (s *Server) taskUndo(task string, n int, force, dry bool) (map[string]any, error) {
	s.mu.Lock()
	_, err := s.task(task)
	_, live := s.live[task]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if live && !dry {
		return nil, fmt.Errorf("%s is running: task.pause it first, then undo", task)
	}
	if n <= 0 {
		ents, _ := filepath.Glob(s.tdir(task, "undo", "run-*.json"))
		sort.Strings(ents)
		for i := len(ents) - 1; i >= 0 && n <= 0; i-- {
			var u undoRun
			if b, err := os.ReadFile(ents[i]); err == nil && json.Unmarshal(b, &u) == nil && u.End != "" {
				n = u.N
			}
		}
		if n <= 0 {
			return nil, fmt.Errorf("%s has no finished run to undo", task)
		}
	}
	var u undoRun
	if b, err := os.ReadFile(s.undoPath(task, n)); err != nil || json.Unmarshal(b, &u) != nil || u.End == "" {
		return nil, fmt.Errorf("%s has no finished run %d", task, n)
	}
	// Who wrote what during the run: every act in its window (the whole log, not the last few hundred: a busy
	// project had pushed the window out of reach). Only writes count; a read that names a file isn't a touch
	// (Tetris bench: `md5sum tetris/board.py` by another hybrid blocked undoing this run's own board.py).
	start, _ := time.Parse(time.RFC3339, u.Start)
	end, _ := time.Parse(time.RFC3339, u.End)
	var own, others []map[string]any
	busy := map[string]bool{} // other hybrids working during the run
	for _, l := range readLines(s.st.path("acts.jsonl"), 1<<30) {
		var a map[string]any
		if json.Unmarshal([]byte(l), &a) != nil || a["event"] != "pre" || a["test"] != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, fmt.Sprint(a["at"]))
		if err != nil || at.Before(start.Add(-time.Second)) || at.After(end.Add(time.Second)) {
			continue
		}
		if t, _ := a["task"].(string); t == task {
			own = append(own, a)
		} else {
			others, busy[t] = append(others, a), true
		}
	}
	var liveNames []string
	for t := range busy {
		liveNames = append(liveNames, t)
	}
	sort.Strings(liveNames)
	cur := s.snap()
	var restored, deleted []string
	var skipped []undoSkip
	paths := map[string]bool{}
	for p := range u.Before {
		paths[p] = true
	}
	for p := range u.After {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		b, hadB := u.Before[p]
		a, hadA := u.After[p]
		if hadB == hadA && (!hadB || b.Sha == a.Sha && b.Size == a.Size && (b.Sha != "" || b.Mod == a.Mod)) {
			continue // the run didn't change it
		}
		c, hasC := cur[p]
		if hasC == hadB && (!hasC || c.Sha == b.Sha && c.Size == b.Size && b.Sha != "") {
			skipped = append(skipped, undoSkip{p, "already as it was before the run (undone earlier?)"})
			continue
		}
		if hasC != hadA || (hasC && (c.Sha != a.Sha || c.Size != a.Size)) {
			skipped = append(skipped, undoSkip{p, "changed again after the run; left as it is"})
			continue
		}
		if a := writerOf(others, p); a != nil && !force {
			skipped = append(skipped, undoSkip{p, fmt.Sprintf("task %v also wrote it during the run (%s); left as it is (force=true to undo it anyway)", a["task"], clip(fmt.Sprint(a["what"]), 120))})
			continue
		}
		// A change counts as this run's only when one of this run's own actions wrote the file. The desk, the owner
		// and other tools write files osenv never sees (Linux video bench: a read-only qwen review's undo would have
		// deleted the 68 proof files the desk wrote from its own terminal during the review). A script the run ran
		// names none of its files either: force=true undoes those.
		if writerOf(own, p) == nil && !force {
			why := "changed during the run, but no action of this run writes it (the desk, another tool or a script it ran may have)"
			if len(liveNames) > 0 {
				why = "changed during the run while " + strings.Join(liveNames, ", ") + " also worked here, and no action of this run writes it"
			}
			skipped = append(skipped, undoSkip{p, why + "; left as it is (force=true to undo it anyway)"})
			continue
		}
		full := filepath.Join(s.st.Root, filepath.FromSlash(p))
		if dry {
			if !hadB {
				deleted = append(deleted, p)
			} else if b.Sha == "" {
				skipped = append(skipped, undoSkip{p, fmt.Sprintf("too big to keep (%d bytes, over %d); restore it by hand", b.Size, undoMaxFile)})
			} else {
				restored = append(restored, p)
			}
			continue
		}
		switch {
		case !hadB: // the run created it, and any folders that now stand empty
			if os.Remove(full) == nil {
				deleted = append(deleted, p)
				for d := filepath.Dir(full); d != s.st.Root && strings.HasPrefix(d, s.st.Root) && !hadDir(u.Before, s.st.Root, d); d = filepath.Dir(d) {
					if os.Remove(d) != nil { // not empty: stop
						break
					}
				}
			}
		case b.Sha == "":
			skipped = append(skipped, undoSkip{p, fmt.Sprintf("too big to keep (%d bytes, over %d); restore it by hand", b.Size, undoMaxFile)})
		default:
			data, err := os.ReadFile(s.st.path("undo", "blobs", b.Sha))
			if err != nil {
				skipped = append(skipped, undoSkip{p, "its saved copy is missing"})
				continue
			}
			os.MkdirAll(filepath.Dir(full), 0o755)
			if os.WriteFile(full, data, 0o644) == nil {
				restored = append(restored, p)
			}
		}
	}
	if dry {
		return map[string]any{"run": n, "engine": u.Engine, "dry_run": true, "would_restore": restored, "would_delete": deleted, "left_alone": skipped}, nil
	}
	s.board.Post(Event{Kind: "undo", Task: task, Who: "desk", Text: fmt.Sprintf("run %d (%s) undone: %d restored, %d deleted, %d left alone", n, u.Engine, len(restored), len(deleted), len(skipped))})
	return map[string]any{"run": n, "engine": u.Engine, "restored": restored, "deleted": deleted, "left_alone": skipped}, nil
}

// writerOf: the first act that wrote rel (a file tool on that path, or a command that writes it), or nil.
func writerOf(acts []map[string]any, rel string) map[string]any {
	for _, a := range acts {
		if actWrites(a, rel) {
			return a
		}
	}
	return nil
}

var redirRe = regexp.MustCompile(`(?:^|[^0-9&>])>{1,2}\s*["']?([^\s"'|;&<>]+)`)

// actWrites: whether one hook record wrote rel. A denied action didn't happen. A file tool writes its path; a
// shell command writes a file it redirects into, or one it names after rm, mv, tee, touch, sed -i and the like
// (cp writes only its last argument). A script that writes files names none of them: that's not attributed.
func actWrites(a map[string]any, rel string) bool {
	if a["verdict"] == "deny" {
		return false
	}
	w, _ := a["judged"].(string) // the whole command (up to 1200 characters); "what" keeps 240
	if w == "" {
		w = fmt.Sprint(a["what"])
	}
	what := strings.ReplaceAll(w, `\`, "/")
	if !strings.Contains(what, rel) {
		return false
	}
	tool := strings.ToLower(fmt.Sprint(a["tool"]))
	for _, k := range []string{"write", "edit", "replace", "create", "patch"} {
		if strings.Contains(tool, k) {
			return strings.HasSuffix(strings.Trim(strings.TrimSpace(strings.SplitN(what, "\n", 2)[0]), `"'`), rel)
		}
	}
	names := func(arg string) bool { return strings.HasSuffix(strings.Trim(arg, `"'`), rel) }
	for _, seg := range splitRe.Split(what, -1) {
		if !strings.Contains(seg, rel) {
			continue
		}
		for _, m := range redirRe.FindAllStringSubmatch(seg, -1) {
			if names(m[1]) {
				return true
			}
		}
		f := strings.Fields(seg)
		if len(f) == 0 {
			continue
		}
		switch strings.ToLower(path.Base(f[0])) {
		case "rm", "mv", "tee", "touch", "truncate", "unlink", "shred", "remove-item", "move-item", "set-content", "add-content",
			"out-file", "new-item", "clear-content", "del", "erase", "ren", "rename-item":
			for _, x := range f[1:] {
				if names(x) {
					return true
				}
			}
		case "cp", "install", "ln", "copy-item", "copy":
			if names(f[len(f)-1]) {
				return true
			}
		case "sed", "perl":
			inPlace := false
			for _, x := range f[1:] {
				inPlace = inPlace || strings.HasPrefix(x, "-i") || x == "--in-place"
			}
			for _, x := range f[1:] {
				if inPlace && names(x) {
					return true
				}
			}
		}
	}
	return false
}

// undoGC drops kept contents no live task's snapshot still points to.
func (s *Server) undoGC() {
	keep := map[string]bool{}
	ents, _ := filepath.Glob(s.st.path("tasks", "*", "undo", "run-*.json"))
	for _, e := range ents {
		var u undoRun
		if b, err := os.ReadFile(e); err == nil && json.Unmarshal(b, &u) == nil {
			for _, m := range []map[string]snapEntry{u.Before, u.After} {
				for _, x := range m {
					keep[x.Sha] = true
				}
			}
		}
	}
	blobs, _ := os.ReadDir(s.st.path("undo", "blobs"))
	for _, b := range blobs {
		if !keep[b.Name()] {
			os.Remove(s.st.path("undo", "blobs", b.Name()))
		}
	}
	snapMu.Lock()
	for k := range snapCache { // the cache may name dropped contents: forget this project's entries
		if strings.HasPrefix(k, s.st.Root+"|") {
			delete(snapCache, k)
		}
	}
	snapMu.Unlock()
}

// hadDir: whether the snapshot had any file under dir (then the folder was there before the run).
func hadDir(before map[string]snapEntry, root, dir string) bool {
	rel, _ := filepath.Rel(root, dir)
	pre := filepath.ToSlash(rel) + "/"
	for p := range before {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// undoCloseOpen gives a run that outlived its server (an orphan) the "after" snapshot it never got, so task.undo
// can put that run back too (Tetris bench: a kill -9 mid-run left game's run 1 with no end).
func (s *Server) undoCloseOpen(task string) {
	ents, _ := filepath.Glob(s.tdir(task, "undo", "run-*.json"))
	for _, e := range ents {
		var u undoRun
		if b, err := os.ReadFile(e); err == nil && json.Unmarshal(b, &u) == nil && u.End == "" {
			u.End, u.After = now(), s.snap()
			writeJSON(e, u)
		}
	}
}
