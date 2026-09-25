package main

// board.go: one append-only event log for everything that happens. The desk (your Claude)
// reads it with "wait", which blocks until something it should act on arrives.

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Event struct {
	N    int            `json:"n"`
	At   string         `json:"at"`
	Kind string         `json:"kind"` // task.new, run.start, run.end, say, waiting, verdict, retired, blocked, kick, learn
	Task string         `json:"task,omitempty"`
	Who  string         `json:"who,omitempty"`
	Text string         `json:"text,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

type Board struct {
	mu     sync.Mutex
	cond   *sync.Cond
	st     *Store
	events []Event // the tail, in memory; the file keeps everything
	n      int
}

const boardKeep = 5000

func openBoard(st *Store) *Board {
	b := &Board{st: st}
	b.cond = sync.NewCond(&b.mu)
	if f, err := os.Open(st.path("board.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				b.events = append(b.events, e)
				if e.N > b.n {
					b.n = e.N
				}
				if len(b.events) > 2*boardKeep { // a year's board never sits in memory whole
					b.events = append([]Event(nil), b.events[len(b.events)-boardKeep:]...)
				}
			}
		}
		f.Close()
		if len(b.events) > boardKeep {
			b.events = b.events[len(b.events)-boardKeep:]
		}
	}
	return b
}

func (b *Board) Post(e Event) Event {
	b.mu.Lock()
	b.n++
	e.N, e.At = b.n, now()
	b.events = append(b.events, e)
	if len(b.events) > boardKeep {
		b.events = b.events[len(b.events)-boardKeep:]
	}
	b.mu.Unlock()
	b.st.appendJSONL("board.jsonl", e)
	b.cond.Broadcast()
	return e
}

func match(e Event, task string, kinds []string) bool {
	if task != "" && e.Task != task {
		return false
	}
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func (b *Board) since(n int, task string, kinds []string, limit int) []Event {
	var out []Event
	for _, e := range b.events {
		if e.N > n && match(e, task, kinds) {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (b *Board) Since(n int, task string, kinds []string, limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.since(n, task, kinds, limit)
}

// Wait blocks until an event after n matches, or the timeout passes. One call, no polling.
func (b *Board) Wait(n int, task string, kinds []string, timeout time.Duration) []Event {
	deadline := time.Now().Add(timeout)
	t := time.AfterFunc(timeout, b.cond.Broadcast)
	defer t.Stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if out := b.since(n, task, kinds, 50); len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		b.cond.Wait()
	}
}

// Head is the newest event number (the desk's cursor starts here).
func (b *Board) Head() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}
