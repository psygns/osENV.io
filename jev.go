package main

// jev.go: the TypeSafe System One client. Jev is the judge: it scores every tool call,
// picks the engine for a step, keeps reviewer feedback in its lane, and reasons about
// which learned corrections matter right now. One keep-alive connection for everything
// (Cloudflare in front of Jev 403s bursts of fresh connections), at most 2 calls in flight.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type Q struct {
	Type         string            `json:"type"`
	Instructions any               `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Noul asks for the probability that a proposition is true in the given state.
func Noul(p string) Q { return Q{Type: "noul", Instructions: map[string]string{"proposition": p}} }

// Choice asks Jev to pick one criterion.
func Choice(instr string, crit map[string]string) Q {
	return Q{Type: "choice", Instructions: instr, Criteria: crit}
}

type Ans struct {
	Noul   *float64           `json:"noul"`
	PTrue  *float64           `json:"p_true"`
	Conf   *float64           `json:"confidence"`
	Choice string             `json:"choice"`
	Probs  map[string]float64 `json:"probabilities"`
}

// P is the number out of any answer shape.
func (a Ans) P() float64 {
	for _, v := range []*float64{a.Noul, a.PTrue, a.Conf} {
		if v != nil {
			return *v
		}
	}
	if a.Choice != "" && a.Probs != nil {
		return a.Probs[a.Choice]
	}
	return 0
}

// ask is the one door to Jev; tests and the offline simulator swap it for a stub.
var ask = func(state map[string]any, qs map[string]Q) (map[string]Ans, error) { return jev.Ask(state, qs) }

type jevClient struct {
	url, model, key string
	http            *http.Client
	slots           chan struct{}
	logPath         string // optional: every call kept as (state, questions, answers) for training a local judge later
	health          jevHealth
}

// jevHealth: the desk hears when Jev stops answering and when it answers again, once each. Windows light rc10: the
// judge was off for ten minutes and the only sign was a quiet board.
type jevHealth struct {
	mu   sync.Mutex
	down bool
	tell func(string) // set by osenv serve: a post to the board
}

func (h *jevHealth) note(err error) {
	h.mu.Lock()
	changed := (err != nil) != h.down
	h.down = err != nil
	tell := h.tell
	h.mu.Unlock()
	switch {
	case !changed || tell == nil:
	case err != nil:
		tell("Jev isn't answering (" + clip(err.Error(), 160) + "): the hybrids' actions run unscored until it answers again")
	default:
		tell("Jev answers again: actions are scored")
	}
}

var jev = &jevClient{http: jevHTTP(30 * time.Second), slots: make(chan struct{}, 2)}

// jevHTTP: HTTP/1.1 keep-alive, never HTTP/2. On Windows (light rc10) the one HTTP/2 connection every Jev call shared
// went silent: each call waited out its timeout on it and retried on it, for ten minutes, while a fresh connection
// answered in 0.4 s, and the judge was off without a word. Over HTTP/1.1 a stuck connection holds only its own call and
// is dropped when that call times out; after any failure the retry starts on a fresh one (see Ask). A call takes 0.1
// to 0.5 s, so 30 s is a wide margin.
// A fresh transport, not a clone of the default one: a clone carries the default's TLS setup, which offers HTTP/2 in
// the handshake, and Jev's front end took it and dropped every HTTP/1.1 request (EOF on each call, measured).
func jevHTTP(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // the documented way to turn HTTP/2 off
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func (j *jevClient) configure(c Config) {
	j.url, j.model = c.Jev.URL, c.Jev.Model
	j.key = readKey("OSENV_JEV_KEY", c.Jev.KeyFile)
	j.logPath = os.Getenv("OSENV_JEV_LOG")
}

var errNoJev = errors.New("no Jev key: set OSENV_JEV_KEY or put the key in the file named by jev.key_file")

func (j *jevClient) Ask(state map[string]any, qs map[string]Q) (a map[string]Ans, err error) {
	if j.key == "" {
		return nil, errNoJev
	}
	defer func() { j.health.note(err) }()
	body, _ := json.Marshal(map[string]any{"model": j.model, "state": state, "questions": qs})
	j.slots <- struct{}{}
	defer func() { <-j.slots }()
	var last error
	for _, wait := range []time.Duration{time.Second, 3 * time.Second, 8 * time.Second, 0} {
		req, _ := http.NewRequest("POST", j.url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+j.key)
		req.Header.Set("User-Agent", "osenv/0.1")
		resp, err := j.http.Do(req)
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var out struct {
					Answers map[string]Ans `json:"answers"`
				}
				if err := json.Unmarshal(data, &out); err != nil {
					return nil, err
				}
				j.keep(state, qs, out.Answers)
				return out.Answers, nil
			}
			last = fmt.Errorf("jev http %d: %.200s", resp.StatusCode, data)
			if resp.StatusCode != 403 && resp.StatusCode != 429 && resp.StatusCode < 500 {
				return nil, last
			}
		} else {
			last = err
			j.http.CloseIdleConnections() // never retry on a connection that may be the stuck one
		}
		if wait == 0 {
			break
		}
		time.Sleep(wait)
	}
	return nil, last
}

func (j *jevClient) keep(state map[string]any, qs map[string]Q, a map[string]Ans) {
	if j.logPath == "" {
		return
	}
	b, _ := json.Marshal(map[string]any{"at": now(), "state": state, "questions": qs, "answers": a})
	f, err := os.OpenFile(home(j.logPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
}
