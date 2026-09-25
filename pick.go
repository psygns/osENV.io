package main

// pick.go: which engine runs a seat's next step. muse holds the momentum; deepseek, qwen and
// the Opus corrector are ONE-run remedies, and the run after any remedy is always muse.
// Order: hand back after a remedy > desk FAIL > the Opus cadence > a routed ask > an unrouted
// ask Jev calls a real mistake > muse. (The simulator caught routed asks pushing the cadence
// back to 9 muse runs; the 6th-run review now always comes first, and it serves open asks too.)

import (
	"fmt"
	"strings"
	"time"
)

var visWords = []string{"visual", "look", "render", "image", "frame", "screen", "ui"}

func visualAsk(m string) bool {
	m = strings.ToLower(m)
	for _, w := range visWords {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

// deskFail is the newest desk FAIL on this task's CURRENT life that no Opus run has served yet.
func (s *Server) deskFail(t *Task) int {
	born, fail := 0, 0
	for _, e := range s.board.Since(0, t.Name, []string{"task.new", "verdict"}, 0) {
		switch {
		case e.Kind == "task.new":
			born, fail = e.N, 0 // a reused name: old FAILs belong to the old hybrid
		case e.Data["pass"] == false && e.N > born:
			fail = e.N
		}
	}
	if fail > t.ServedFail {
		return fail
	}
	return 0
}

// pick mutates t's loop bookkeeping and returns the engine, why, whether it was routed,
// and the notes (served asks cleared, a BLOCKED line added after a second remedy crash).
func (s *Server) pick(t *Task, notes string) (engine, reason string, routed bool, out string) {
	cfg := s.st.Cfg
	today := time.Now().UTC().Format("2006-01-02")
	if t.OpusDay != today {
		t.OpusDay, t.OpusToday = today, 0
	}
	capOK := cfg.OpusCap == 0 || t.OpusToday < cfg.OpusCap
	okExit := t.LastExit == 0
	last := t.LastEngine

	// 1. A remedy is one run: hand straight back to muse, clearing the ask it served.
	if last == "opus" || ((last == "deepseek" || last == "qwen") && strings.HasPrefix(t.Reason, "route")) {
		if okExit {
			t.Crashes = 0
			if last == "opus" {
				t.ServedFail = s.board.Head() // an Opus run reads the board: every earlier FAIL is served
			}
		} else {
			t.Crashes++
		}
		if okExit || t.Crashes >= 2 {
			served := func(m string) bool {
				switch last {
				case "qwen":
					return visualAsk(m)
				case "deepseek":
					return !visualAsk(m)
				}
				return true
			}
			notes = dropLines(notes, func(m, _ string) bool { return strings.HasPrefix(m, "ESCALATE:") && served(m) })
			if !okExit {
				notes = underState(notes, fmt.Sprintf("- BLOCKED: the %s remedy crashed twice in a row (%s); the desk looks at it.", last, t.Reason))
				s.board.Post(Event{Kind: "blocked", Task: t.Name, Who: t.Seat, Text: last + " crashed twice: " + t.Reason})
				t.Crashes = 0
			}
		}
		t.Runs++
		by := last
		if strings.HasSuffix(t.Reason, "(plain check: qwen not called)") { // VM 0.2.3 #16
			by = "the deepseek plain check"
		}
		return "muse", "back to muse after " + by, false, notes
	}

	// 2. The desk failed it: the desk's look is the verdict, no vote. Opus corrects, then muse.
	if f := s.deskFail(t); f > 0 && capOK {
		t.ServedFail, t.Runs, t.OpusToday = f, 0, t.OpusToday+1
		return "opus", fmt.Sprintf("escalation: desk FAIL (board n%d)", f), false, notes
	}

	// 3. The cadence: every Nth muse run gets an Opus review.
	if cfg.Cadence > 0 && t.Runs+1 >= cfg.Cadence && capOK {
		n := t.Runs
		t.Runs, t.OpusToday = 0, t.OpusToday+1
		return "opus", fmt.Sprintf("cadence: %d muse runs since the last Opus review", n), false, notes
	}

	open := asks(notes)
	// 4. An ask that matches a learned route goes to the model that is good at it. Jev reasons it.
	if routes := s.learn.list("route", t.Name); len(routes) > 0 && len(open) > 0 {
		qs := map[string]Q{}
		for _, r := range routes {
			qs[r.ID] = Noul(r.Detect)
		}
		if a, err := ask(map[string]any{"orders": orders(notes)}, qs); err == nil {
			var best *Item
			for _, r := range routes {
				if a[r.ID].P() >= 0.7 && (best == nil || a[r.ID].P() > a[best.ID].P()) {
					best = r
				}
			}
			if best != nil && (best.To != "opus" || capOK) {
				if best.To == "opus" {
					t.Runs, t.OpusToday = 0, t.OpusToday+1
				}
				return best.To, fmt.Sprintf("route %s -> %s: %s", best.ID, best.To, best.Text), true, notes
			}
		}
	}

	// 5. An ask no route took: Opus, if Jev reasons it is a real mistake (not just waiting on something).
	if len(open) > 0 && capOK {
		a, err := ask(map[string]any{"orders": orders(notes)}, map[string]Q{"fix": Noul(
			"The agent made a real mistake or is stuck in a way a stronger model should now correct; it is NOT merely blocked, waiting on someone, or missing an asset.")})
		if err == nil && a["fix"].P() >= 0.7 {
			t.Runs, t.OpusToday = 0, t.OpusToday+1
			return "opus", "escalation: " + open[0], false, notes
		}
	}

	t.Runs++
	return "muse", "", false, notes
}

// effort: Jev picks muse's reasoning effort for the next step from its orders.
func effort(notes string) string {
	a, err := ask(map[string]any{"orders": orders(notes)}, map[string]Q{"effort": Choice(
		"Which kind of work is the agent's NEXT step? Pick the tier that fits it.", map[string]string{
			"medium": "Low tasks and audits: going back over finished work, reviews, status checks, reading and summarizing.",
			"high":   "The workhorse: normal coding, fixes, features, research and multi-step work.",
			"max":    "Gritty, complex patterns and building: intricate systems, tricky architecture, hard pipelines.",
		})})
	if err != nil {
		return "high"
	}
	switch a["effort"].Choice {
	case "medium", "high":
		return a["effort"].Choice
	case "max":
		return "ultra"
	}
	return "high"
}
