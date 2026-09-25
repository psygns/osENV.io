package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The tool library: a script in the project's exec/ folder with a "usage:" line near its top is a tool.
// The folder is the registry. At run start Jev lists the tools that fit muse's job in its brief; on each
// action, one that does by hand what a listed tool does gets a note: there's a tool for that.

type Tool struct{ Path, Usage string }

var usageRe = regexp.MustCompile(`(?i)\busage:\s*(.+)`)

// tools: every tool in <root>/exec/ (one level deep), sorted by path.
func (s *Server) tools() []Tool {
	var out []Tool
	ents, _ := os.ReadDir(filepath.Join(s.st.Root, "exec"))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		rel := "exec/" + e.Name()
		f, err := os.Open(filepath.Join(s.st.Root, rel))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for i := 0; i < 10 && sc.Scan(); i++ {
			if m := usageRe.FindStringSubmatch(sc.Text()); m != nil {
				out = append(out, Tool{rel, clip(strings.TrimSpace(strings.TrimRight(m[1], `"'*/ `)), 300)})
				break
			}
		}
		f.Close()
	}
	return out
}

// Measured on the real Jev (3 tools x 3 jobs, 6 actions): fitting tools 0.53-0.74, others 0.34 or less;
// an action doing a listed tool's step 0.45-0.78, others 0.16 or less. Plainer, single-claim wordings
// beat "...so the agent should use it instead" (0.49-0.70 vs up to 0.31).
const toolFitP, toolHandP, toolsMax = 0.45, 0.4, 5

func toolFitQ(t Tool) string  { return "Doing this job includes the step this tool does: " + t.Usage }
func toolHandQ(t Tool) string { return "This action does the same thing as this tool: " + t.Usage }

// fitTools: the tools Jev judges this job needs, best first, up to toolsMax. Jev down: none.
func (s *Server) fitTools(name string) []Tool {
	all := s.tools()
	if len(all) == 0 {
		return nil
	}
	qs := map[string]Q{}
	for i, t := range all {
		qs[fmt.Sprint(i)] = Noul(toolFitQ(t))
	}
	a, err := ask(map[string]any{"task": clip(s.job(name), 2000), "platform": platformName()}, qs)
	if err != nil {
		return nil
	}
	var out []Tool
	p := map[string]float64{}
	for i, t := range all {
		if p[t.Path] = a[fmt.Sprint(i)].P(); p[t.Path] >= toolFitP {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return p[out[i].Path] > p[out[j].Path] })
	return out[:min(len(out), toolsMax)]
}

func (s *Server) toolByPath(p string) (Tool, bool) {
	for _, t := range s.tools() {
		if t.Path == p {
			return t, true
		}
	}
	return Tool{}, false
}
