package main

// learn.go: the self-learning loop.
//
// A correction (from the Opus corrector, or the desk) comes in through learn.add. Jev REASONS
// about it before it is kept:
//   - fit:     can muse learn this as a rule (a lesson), or does that kind of step belong
//              to another model (a route to deepseek, qwen or opus)?
//   - general: is it a rule that helps on many tasks (kept for every future hybrid), or a
//              detail of this one task (kept only until this hybrid retires)? That is the
//              noise filter: one-task details never reach the shared learned loop.
//   - same:    does it describe a mistake an existing lesson already covers? Then the old
//              lesson gets sharper (its detect is replaced, its escapes go up) instead of a
//              near-copy piling up.
// When a hybrid starts a run, Jev reads its orders and REASONS about which lessons matter for
// its next steps (surfacing). Only those are checked on every tool call, and only those are
// shown to muse. Word overlap is used for one thing: trimming a very long list down to a
// shortlist before Jev judges it. It never decides relevance.
// Lessons that Jev keeps calling relevant but that never catch anything are noise: they retire.

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

type Item struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`         // lesson | route
	Scope    string   `json:"scope"`        // global | task:<name>
	Text     string   `json:"text"`         // the right way, shown to muse when it matters
	Detect   string   `json:"detect"`       // lesson: the mistake as an action about to happen; route: a kind of next step
	To       string   `json:"to,omitempty"` // route target: deepseek | qwen | opus
	Severity string   `json:"severity"`     // nudge | kick (kick sends the action back once)
	Source   string   `json:"source"`
	Evidence string   `json:"evidence,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Made     string   `json:"made"`
	Surfaced int      `json:"surfaced"` // steps Jev judged it relevant to
	Catches  int      `json:"catches"`  // mistakes it caught before they happened
	Escapes  int      `json:"escapes"`  // times the mistake happened anyway (repeats merged in)
	Status   string   `json:"status"`   // active | retired
	Why      string   `json:"why,omitempty"`
}

type Learn struct {
	mu      sync.Mutex
	path    string
	seqPath string // the highest ID number ever handed out: a dropped task lesson's ID is never reused (Linux video bench #23)
	items   []*Item
	seq     int
}

// coreRoutes are the hybrid's design, not something learned: looks go to qwen, code to deepseek.
// Their wording is what Jev matches reliably (sim: "asks for a code review" 0.98, while
// "asks for a code review of code the agent changed" scored 0.49 - Jev can't see who changed it).
var coreRoutes = []*Item{
	{ID: "R-visual", Kind: "route", Scope: "global", To: "qwen", Severity: "nudge", Status: "active", Source: "core",
		Text:   "Judging how something looks (renders, frames, UI, art) goes to qwen, the best eyes.",
		Detect: "An ESCALATE line in the orders asks for a visual review."},
	{ID: "R-code", Kind: "route", Scope: "global", To: "deepseek", Severity: "nudge", Status: "active", Source: "core",
		Text:   "Reviewing or fixing code goes to deepseek, the strong cheap coder.",
		Detect: "An ESCALATE line in the orders asks for a code review, or for help with code: a script, a crash, a failing test or a build."},
}

func openLearn(st *Store) *Learn {
	l := &Learn{path: st.path("learn.json"), seqPath: st.path("learn-seq.json")}
	readJSON(l.path, &l.items)
	readJSON(l.seqPath, &l.seq)
	for _, c := range coreRoutes {
		if l.get(c.ID) == nil {
			cp := *c
			cp.Made = now()
			l.items = append(l.items, &cp)
		}
	}
	for _, it := range l.items {
		var n int
		fmt.Sscanf(strings.TrimLeft(it.ID, "LRO-"), "%d", &n) // owner rules share the numbering (O3 counted as 0)
		if n > l.seq {
			l.seq = n
		}
	}
	return l
}

func (l *Learn) save() {
	writeJSON(l.path, l.items)
	writeJSON(l.seqPath, l.seq)
}

func visible(it *Item, task string) bool {
	return it.Status == "active" && (it.Scope == "global" || it.Scope == "task:"+task)
}

func (l *Learn) list(kind, task string) []*Item {
	var out []*Item
	for _, it := range l.items {
		if (kind == "" || it.Kind == kind) && (task == "" || visible(it, task)) {
			out = append(out, it)
		}
	}
	return out
}

func (l *Learn) get(id string) *Item {
	for _, it := range l.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// ---- intake -----------------------------------------------------------------------------

type AddIn struct {
	Task     string `json:"task"`
	Text     string `json:"text"`   // the right way, in one or two sentences
	Detect   string `json:"detect"` // ONE proposition about the action as it is about to happen
	Next     string `json:"next"`   // for a step muse can't learn: one proposition about that kind of next step
	Source   string `json:"source"`
	Evidence string `json:"evidence"`
	// Seen: lessons already added or merged from this same review. A duplicate finding merges into one of
	// them without counting as an escape (Linux run: a reviewer's two copies of one finding counted as a repeat).
	Seen map[string]bool `json:"-"`
	// Newer: the desk's orders and verdicts posted while the review ran. A reviewer that read a document the desk
	// changed mid-review filed lessons baking in the old rule (video bench, Linux #15).
	Newer string `json:"-"`
}

// general: a way of working, including a rule about the platform (VM test: the curl-in-PowerShell
// rule was scoped to one task and died with it), never a fact about this task's own files or values.
// generalP: 0.3-0.7 is Jev unsure, and unsure stays with its task (Linux run: L5, a detail of the todo
// task, scored 0.59 and went global). Every labelled general rule in the sim scored 0.82 or more.
const generalP = 0.7

const generalQ = "This correction is a way of working (a habit, a check, or a rule about the tools or operating system) that applies to the agent's other tasks too; it is not a fact about this task's own files, settings, values, coordinates or machines."

const learnableQ = "muse can follow this correction as a short written rule the next time it is about to make the same mistake; it does not need a stronger or different model to do this kind of work."

func (l *Learn) Add(in AddIn) (*Item, string, error) {
	if strings.TrimSpace(in.Text) == "" || strings.TrimSpace(in.Detect) == "" {
		return nil, "", fmt.Errorf("learn.add needs text (the right way) and detect (the mistake as an action about to happen)")
	}
	if in.Source == "owner" { // the owner's standing call: no filing to judge, a hard block from day one
		l.mu.Lock()
		defer l.mu.Unlock()
		l.seq++
		it := &Item{ID: fmt.Sprintf("O%d", l.seq), Kind: "lesson", Scope: "global", Text: in.Text, Detect: in.Detect, Severity: "kick",
			Source: "owner", Evidence: in.Evidence, Tags: tagsOf(in.Text + " " + in.Detect), Made: now(), Status: "active", Why: "owner rule"}
		l.items = append(l.items, it)
		l.save()
		return it, "added", nil
	}
	l.mu.Lock()
	cands := l.shortlist(in.Text+" "+in.Detect, tagsOf(in.Text+" "+in.Detect), in.Task, "", 3)
	l.mu.Unlock()

	// Lesson first (VM test: all 3 real corrections were misfiled as routes when "who should handle
	// this" was one open choice). A route is only for a KIND OF STEP muse can't do, never a rule.
	qs := map[string]Q{
		"learnable": Noul(learnableQ),
		"route": Choice("If muse could NOT follow it as a rule, which model should take that kind of step from now on?",
			map[string]string{
				"deepseek": "writing or fixing hard code, tests or build logic that muse keeps getting wrong",
				"qwen":     "judging how something looks: screens, renders, UI, art or visual quality",
				"opus":     "hard, intricate or cross-cutting judgement only the strongest model gets right",
			}),
		"general": Noul(generalQ),
		"tool":    Noul(toolQ),
	}
	if in.Newer != "" {
		qs["outdated"] = Noul(outdatedQ)
	}
	for _, c := range cands {
		qs["same_"+c.ID] = Noul("The new correction describes the same mistake as this existing lesson, so one lesson should cover both: \"" + c.Text + "\" (it catches: " + c.Detect + ")")
		qs["contra_"+c.ID] = Noul(contraQ + "\"" + c.Text + "\" (it catches: " + c.Detect + ")")
	}
	state := map[string]any{"new_correction": in.Text, "the_mistake_as_an_action": in.Detect, "task": in.Task,
		"platform": platformName(), "model_profiles": profiles}
	if in.Newer != "" {
		state["desk_orders_since_the_review_began"] = clip(in.Newer, 1500)
	}
	a, err := ask(state, qs)

	if err == nil && a["tool"].P() >= toolP { // not for an agent: no action of muse's could ever match it
		return nil, ForDesk, nil
	}
	if err == nil && in.Newer != "" && a["outdated"].P() >= outdatedP {
		return nil, Outdated, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		best, bp := (*Item)(nil), 0.0
		for _, c := range cands {
			if p := a["same_"+c.ID].P(); p >= 0.7 && p > bp {
				best, bp = c, p
			}
		}
		if best != nil { // the mistake repeated: sharpen the old lesson instead of adding a near-copy
			repeat := !in.Seen[best.ID] // the same finding twice in one review is one occurrence
			if repeat {
				best.Escapes++
			}
			best.Detect = in.Detect
			best.Tags = union(best.Tags, tagsOf(in.Text+" "+in.Detect))
			if best.Escapes >= 2 {
				best.Severity = "kick"
			}
			if repeat && best.Scope != "global" && a["general"].P() >= generalP {
				best.Scope = "global" // it keeps coming back: promote it
			}
			best.Why += fmt.Sprintf(" | merged %s (same %.2f)", now()[:16], bp)
			l.save()
			return best, "merged", nil
		}
	}
	l.seq++
	how := "added"
	it := &Item{ID: fmt.Sprintf("L%d", l.seq), Kind: "lesson", Scope: "task:" + in.Task, Text: in.Text, Detect: in.Detect,
		Severity: "nudge", Source: in.Source, Evidence: in.Evidence, Tags: tagsOf(in.Text + " " + in.Detect), Made: now(), Status: "active"}
	if err != nil {
		it.Why = "Jev unreachable at intake: kept as a lesson for this task only"
	} else {
		lp, g, to := a["learnable"].P(), a["general"].P(), a["route"].Choice
		if g >= generalP {
			it.Scope = "global"
		}
		fit := "teach"
		if lp < 0.35 && (to == "deepseek" || to == "qwen" || to == "opus") {
			fit = to
			it.Kind, it.To = "route", to
			it.ID = fmt.Sprintf("R%d", l.seq)
			if in.Next != "" {
				it.Detect = in.Next
			}
		}
		it.Why = fmt.Sprintf("fit %s (learnable %.2f), general %.2f", fit, lp, g)
		for _, c := range cands { // Linux I-7: L6 endorsed try/except int(), L8 said that's the mistake; both stood
			if p := a["contra_"+c.ID].P(); p >= contraP {
				it.Why += fmt.Sprintf("; contradicts %s (%.2f): the desk decides which stands", c.ID, p)
				how = "added, CONTRADICTS " + c.ID + " (desk: retire or edit one)"
			}
		}
	}
	l.items = append(l.items, it)
	l.save()
	return it, how, nil
}

// ---- surfacing ---------------------------------------------------------------------------

const surfaceMax, relevantP = 6, 0.55

// toolQ: Windows 0.2.3 #19: Opus filed "keep number-formatting fixes instead of dropping them" as a muse lesson.
// Measured on the real Jev: 2 corrections aimed at osenv 0.73-0.84, 2 agent habits 0.09-0.14.
const toolQ, toolP = "This correction is about how the system that runs the agents should behave (its filters, checks or routing), not about something an agent does in its own work.", 0.7

// outdatedQ: measured on the real Jev (6 cases, twice): corrections resting on a rule the desk's newer orders
// changed 0.73-0.86; unrelated ones 0.21-0.28.
const outdatedQ, outdatedP = "This correction is outdated by the desk's newer orders.", 0.6

// Outdated: Add's answer when the desk changed the rule a correction rests on while the review ran.
const Outdated = "not filed as a lesson: the desk's orders since the review began outdate it"

// ForDesk: Add's answer when a correction is about osenv itself: it goes to the desk, not into the loop.
const ForDesk = "for the desk, not filed as a lesson (it is about how osenv behaves)"

// contraQ: measured on the real Jev: 2 contradicting pairs 0.57-0.82, 3 compatible or duplicate pairs
// 0.05-0.09. Unsure scores are common here, so it only flags to the desk, never retires on its own.
const contraQ, contraP = "The new correction and this existing lesson cannot both be followed: doing what one says breaks the other: ", 0.5

// detectP: a lesson fires when Jev puts its detect at or above this on the action (sim: 0.8 keeps
// the 94.7% catch rate and cuts false alarms from 2.7% to 2.1%). lessonsPerCheck caps one call: a call's state
// is billed once and its questions cost almost nothing, so every lesson of a store this size is checked
// (Windows light rc8: a cap of 16 checked L1-L9 of 84 on every action and never the one that named the bug).
const detectP, lessonsPerCheck = 0.8, 128

// checkSet: the lessons to check on an action. Owner rules and the ones Jev judged relevant come first,
// then the rest by how close they are to the action itself, up to n. A small store is checked whole.
func (l *Learn) checkSet(task string, surfaced []string, n int, action string) []*Item {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*Item
	seen := map[string]bool{}
	for _, it := range l.items { // owner rules are always checked, whatever else fits
		if it.Source == "owner" && it.Status == "active" && it.Kind == "lesson" {
			out, seen[it.ID] = append(out, it), true
		}
	}
	for _, id := range surfaced {
		if it := l.get(id); it != nil && !seen[id] && visible(it, task) && it.Kind == "lesson" {
			out, seen[id] = append(out, it), true
		}
	}
	// Not by track record: imported lessons all start at none, so ID order won (Windows light rc8: every action
	// got L1-L9 of 84, and L71, which names the bug muse wrote, was never checked).
	for _, it := range l.shortlist(action, tagsOf(action), task, "lesson", n+len(seen)) {
		if len(out) >= n {
			break
		}
		if !seen[it.ID] {
			out, seen[it.ID] = append(out, it), true
		}
	}
	return out
}

// Relevant: Jev reads the seat's orders and reasons about which lessons matter for its next
// steps. One call; only the chosen few are checked on each tool call and shown to muse.
func (l *Learn) Relevant(task, job, ord string, count bool) ([]*Item, error) {
	l.mu.Lock()
	cands := l.shortlist(ord+"\n"+job, tagsOf(ord+"\n"+job), task, "lesson", 24)
	l.mu.Unlock()
	if len(cands) == 0 {
		return nil, nil
	}
	qs := map[string]Q{}
	for _, c := range cands {
		qs[c.ID] = Noul(relevantProp(c))
	}
	a, err := ask(map[string]any{"task": job, "orders": ord, "platform": platformName()}, qs)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool { return a[cands[i].ID].P() > a[cands[j].ID].P() })
	var out []*Item
	l.mu.Lock()
	for _, c := range cands {
		if a[c.ID].P() >= relevantP && len(out) < surfaceMax {
			if count {
				c.Surfaced++
			}
			out = append(out, c)
		}
	}
	if count {
		l.save()
	}
	l.mu.Unlock()
	return out, nil
}

func relevantProp(c *Item) string {
	return "This learned lesson matters for what the agent is about to do next, given its task and these orders, so it should be reminded of it now: \"" +
		c.Text + "\" (the mistake it prevents: " + c.Detect + ")"
}

// shortlist trims a long list by cheap overlap + credibility. It only ever NARROWS; Jev decides.
func (l *Learn) shortlist(text string, tags []string, task, kind string, k int) []*Item {
	var pool []*Item
	for _, it := range l.items {
		if (kind == "" || it.Kind == kind) && (task == "" || visible(it, task)) {
			pool = append(pool, it)
		}
	}
	docs := make([]string, len(pool))
	for i, it := range pool {
		docs[i] = it.Text + " " + it.Detect + " " + strings.Join(it.Tags, " ")
	}
	q := vec(text, docs)
	type sc struct {
		it *Item
		s  float64
	}
	var s []sc
	for i, it := range pool {
		cred := float64(it.Catches+it.Escapes+1) / float64(it.Catches+it.Escapes+1+it.Surfaced/20)
		s = append(s, sc{it, (cos(q, vec(docs[i], docs)) + 0.15*float64(overlap(tags, it.Tags))) * cred})
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].s > s[j].s })
	k = min(k, len(s))
	out := make([]*Item, 0, k)
	for _, x := range s[:k] {
		out = append(out, x.it)
	}
	return out
}

func (l *Learn) caught(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if it := l.get(id); it != nil {
			it.Catches++
		}
	}
	l.save()
}

// sweep retires noise: lessons Jev kept calling relevant that never caught a thing.
func (l *Learn) sweep() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var gone []string
	for _, it := range l.items {
		if it.Status == "active" && it.Kind == "lesson" && it.Source != "owner" && it.Surfaced >= 40 && it.Catches == 0 && it.Escapes == 0 {
			it.Status = "retired"
			it.Why += " | retired as noise: surfaced 40+ times, never caught"
			gone = append(gone, it.ID)
		}
	}
	if len(gone) > 0 {
		l.save()
	}
	return gone
}

// dropTask forgets a retired hybrid's task-only lessons: its noise dies with it.
func (l *Learn) dropTask(task string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []*Item
	for _, it := range l.items {
		if it.Scope != "task:"+task {
			keep = append(keep, it)
		}
	}
	l.items = keep
	l.save()
}

// graph exports the learned loop as nodes and edges (the same shape as a kgraph.json).
func (l *Learn) graph() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var nodes, edges []map[string]any
	seen := map[string]bool{}
	for _, it := range l.items {
		nodes = append(nodes, map[string]any{"id": it.ID, "kind": it.Kind, "summary": it.Text, "scope": it.Scope,
			"status": it.Status, "catches": it.Catches, "escapes": it.Escapes, "surfaced": it.Surfaced})
		for _, t := range it.Tags {
			if !seen[t] {
				seen[t] = true
				nodes = append(nodes, map[string]any{"id": "tag:" + t, "kind": "tag"})
			}
			edges = append(edges, map[string]any{"from": it.ID, "rel": "about", "to": "tag:" + t})
		}
		if it.To != "" {
			edges = append(edges, map[string]any{"from": it.ID, "rel": "routes_to", "to": "model:" + it.To})
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

// ---- small text tools (only for the shortlist) -----------------------------------------------

var wordRe = regexp.MustCompile(`[a-z0-9_./-]+`)
var stop = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "is": true, "it": true, "this": true, "that": true, "with": true, "its": true, "be": true,
	"not": true, "no": true, "as": true, "at": true, "by": true, "from": true, "into": true, "action": true, "agent": true}

func words(s string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		w = strings.Trim(w, "./-")
		if len(w) > 1 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

func vec(text string, corpus []string) map[string]float64 {
	df := map[string]int{}
	for _, d := range corpus {
		seen := map[string]bool{}
		for _, w := range words(d) {
			if !seen[w] {
				df[w]++
				seen[w] = true
			}
		}
	}
	v := map[string]float64{}
	for _, w := range words(text) {
		v[w] += math.Log(1 + float64(len(corpus)+1)/float64(df[w]+1))
	}
	return v
}

func cos(a, b map[string]float64) float64 {
	var dot, na, nb float64
	for k, x := range a {
		dot += x * b[k]
		na += x * x
	}
	for _, y := range b {
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

var tagRe = regexp.MustCompile(`\.(gd|py|go|md|png|jpe?g|glb|json|sh|ts|js|tscn|gdshader|blend|html|css|sql|yaml|toml)\b|/tmp\b|\b(bash|edit|write|curl|git|rm|mv|cp|test|tests|deploy|render|image|frame|board|scratch|build|server|client|api|key|token|secret|websocket|shadow|mesh|texture|rig|ui)\b`)

func tagsOf(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range tagRe.FindAllString(strings.ToLower(s), -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func overlap(a, b []string) int {
	n := 0
	for _, x := range a {
		for _, y := range b {
			if x == y {
				n++
			}
		}
	}
	return n
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

var profiles = mustRead("prompts/profiles.md")

func mustRead(p string) string {
	b, err := prompts.ReadFile(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv: missing embedded", p)
		return ""
	}
	return string(b)
}

// platformName tells Jev where the agents run: a lesson about PowerShell matters on Windows only.
func platformName() string {
	if runtime.GOOS == "windows" {
		return "Windows (the agent's shell is PowerShell 5.1)"
	}
	return runtime.GOOS
}

// ownerRules: the owner's standing calls, shown to muse at every run start.
func (l *Learn) ownerRules() []*Item {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*Item
	for _, it := range l.items {
		if it.Source == "owner" && it.Status == "active" {
			out = append(out, it)
		}
	}
	return out
}

// ---- lesson packs ------------------------------------------------------------------------
// A pack carries the lessons that proved themselves (caught a real mistake) into another project, so a new
// project starts with them instead of paying to learn them again. Owner rules stay home: they're one owner's
// calls, not lessons. An imported lesson starts its own record at zero; its old record is kept in why.

type Pack struct {
	Pack    int          `json:"osenv_lesson_pack"`
	From    string       `json:"from"`
	Made    string       `json:"made"`
	Lessons []PackLesson `json:"lessons"`
}

type PackLesson struct {
	Text     string `json:"text"`
	Detect   string `json:"detect"`
	Severity string `json:"severity"`
	Catches  int    `json:"catches"`
	Escapes  int    `json:"escapes"`
}

func (l *Learn) exportPack(from string, minCatches int) Pack {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := Pack{Pack: 1, From: from, Made: now()}
	for _, it := range l.items {
		if it.Kind == "lesson" && it.Status == "active" && it.Scope == "global" && it.Source != "owner" && it.Catches >= minCatches {
			p.Lessons = append(p.Lessons, PackLesson{it.Text, it.Detect, it.Severity, it.Catches, it.Escapes})
		}
	}
	return p
}

// importPack adds the pack's lessons as global lessons; one whose text is already here is skipped.
func (l *Learn) importPack(p Pack) (added, skipped []string) {
	added, skipped = []string{}, []string{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, pl := range p.Lessons {
		dup := ""
		for _, it := range l.items {
			if it.Kind == "lesson" && it.Status == "active" && strings.TrimSpace(it.Text) == strings.TrimSpace(pl.Text) {
				dup = it.ID
			}
		}
		if dup != "" || strings.TrimSpace(pl.Text) == "" || strings.TrimSpace(pl.Detect) == "" {
			skipped = append(skipped, clip(pl.Text, 80))
			continue
		}
		sev := pl.Severity
		if sev != "kick" {
			sev = "nudge"
		}
		l.seq++
		it := &Item{ID: fmt.Sprintf("L%d", l.seq), Kind: "lesson", Scope: "global", Text: pl.Text, Detect: pl.Detect, Severity: sev,
			Source: "pack", Evidence: "pack from " + p.From, Tags: tagsOf(pl.Text + " " + pl.Detect), Made: now(), Status: "active",
			Why: fmt.Sprintf("imported from %s's lesson pack (%s): %d catches, %d escapes there", p.From, p.Made[:10], pl.Catches, pl.Escapes)}
		l.items = append(l.items, it)
		added = append(added, it.ID)
	}
	l.save()
	return added, skipped
}
