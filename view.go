package main

// view.go: parse, don't dump. Every model call re-sends everything the agent has read, so the
// cheapest token is the one never read. `osenv view <file> [--q "question"]` hands the agent a
// parsed view instead of the raw thing:
//   - an image: shrunk to at most 768 px (optionally cropped first) and saved as a JPEG
//   - a small text file: as is, with line numbers
//   - a big log: the errors and warnings with line numbers, plus the tail
//   - big code: an outline (functions, types, classes) with line numbers
//   - big JSON: its shape
//   - any big file with --q: Jev reads it in chunks and REASONS which parts answer the question
// The hook enforces it: a raw read of a big file or a big image is sent back once with the
// exact `osenv view` line to use instead. No budget to remember; the environment does the parsing.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const viewMaxPx, bigLines = 768, 300

var viewSuffixRe = regexp.MustCompile(`(-\d+px)+$`)

// bigCode: a source file an agent must review or edit is read whole below this. Linux run: 2 of 3
// big-read kickbacks stopped a reviewer and an editor from reading the file they had to work on.
const bigCode = 1000

type ViewIn struct {
	Task string `json:"task"`
	Path string `json:"path"`
	Q    string `json:"q"`    // with a question, Jev picks the parts of the file that answer it
	Crop string `json:"crop"` // images: "x,y,w,h" in pixels or fractions (0-1) of the original
}

var imgExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true}

func (s *Server) view(in ViewIn) (string, error) {
	p := in.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.st.Root, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a folder: list it with ls, then view a file", in.Path)
	}
	if imgExt[strings.ToLower(filepath.Ext(p))] {
		return s.viewImage(p, in)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	head := fmt.Sprintf("%s: %d lines, %d bytes", rel(s.st.Root, p), len(lines), len(b))
	if len(lines) <= bigLines && in.Q == "" {
		return head + "\n" + numbered(lines, 1), nil
	}
	if in.Q != "" {
		return s.viewAsk(head, lines, in.Q)
	}
	switch ext := strings.ToLower(filepath.Ext(p)); {
	case ext == ".json":
		var v any
		if json.Unmarshal(b, &v) == nil {
			return head + " (JSON shape; add --q to pull the part you need)\n" + shape(v, 0), nil
		}
	case codeExt[ext]:
		return head + " (outline; view a range with sed -n A,Bp, or add --q)\n" + outline(lines), nil
	}
	return head + " (log view: errors and warnings, then the tail; add --q to pull the part you need)\n" + logView(lines), nil
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func numbered(lines []string, from int) string {
	var sb strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&sb, "%5d  %s\n", from+i, clip(l, 400))
	}
	return sb.String()
}

var badRe = regexp.MustCompile(`(?i)\b(error|err|fail(ed|ure)?|fatal|panic|exception|traceback|warn(ing)?|denied|timeout|timed out|refused|not found|cannot|can't)\b`)

func logView(lines []string) string {
	var sb strings.Builder
	var hits []int
	for i, l := range lines {
		if badRe.MatchString(l) {
			hits = append(hits, i)
		}
	}
	fmt.Fprintf(&sb, "-- %d error/warning lines", len(hits))
	if len(hits) > 40 {
		fmt.Fprintf(&sb, " (the last 40 shown)")
		hits = hits[len(hits)-40:]
	}
	sb.WriteString("\n")
	for _, i := range hits {
		fmt.Fprintf(&sb, "%5d  %s\n", i+1, clip(lines[i], 300))
	}
	from := max(0, len(lines)-25)
	sb.WriteString("-- tail\n" + numbered(lines[from:], from+1))
	return sb.String()
}

var codeExt = map[string]bool{".go": true, ".py": true, ".gd": true, ".js": true, ".ts": true, ".sh": true, ".rs": true,
	".c": true, ".h": true, ".cpp": true, ".java": true, ".rb": true, ".lua": true, ".gdshader": true, ".md": true}
var declRe = regexp.MustCompile(`^\s*(func |def |class |type |struct |enum |interface |impl |fn |pub fn |static func |signal |const |var |export |async def |#{1,3} )`)

func outline(lines []string) string {
	var sb strings.Builder
	n := 0
	for i, l := range lines {
		if declRe.MatchString(l) && n < 150 {
			fmt.Fprintf(&sb, "%5d  %s\n", i+1, clip(strings.TrimRight(l, " {"), 160))
			n++
		}
	}
	if n == 0 {
		return logView(lines)
	}
	return sb.String()
}

func shape(v any, depth int) string {
	pad := strings.Repeat("  ", depth)
	switch t := v.(type) {
	case map[string]any:
		if depth > 2 {
			return pad + fmt.Sprintf("{%d keys}\n", len(t))
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for i, k := range keys {
			if i == 40 {
				fmt.Fprintf(&sb, "%s... %d more keys\n", pad, len(keys)-40)
				break
			}
			fmt.Fprintf(&sb, "%s%s:\n%s", pad, k, shape(t[k], depth+1))
		}
		return sb.String()
	case []any:
		if len(t) == 0 {
			return pad + "[]\n"
		}
		return pad + fmt.Sprintf("[%d items], first:\n", len(t)) + shape(t[0], depth+1)
	case string:
		return pad + strconv.Quote(clip(t, 80)) + "\n"
	default:
		return pad + fmt.Sprint(t) + "\n"
	}
}

// viewAsk: Jev reads the file in chunks and reasons which ones answer the question. The word
// overlap below only trims a huge file to 24 chunks first; Jev picks.
func (s *Server) viewAsk(head string, lines []string, q string) (string, error) {
	const size, step = 40, 35
	type chunk struct {
		from, to int
		text     string
	}
	var cs []chunk
	for i := 0; i < len(lines); i += step {
		j := min(len(lines), i+size)
		cs = append(cs, chunk{i, j, strings.Join(lines[i:j], "\n")})
		if j == len(lines) {
			break
		}
	}
	if len(cs) > 24 {
		docs := make([]string, len(cs))
		for i, c := range cs {
			docs[i] = c.text
		}
		qv := vec(q, docs)
		sort.SliceStable(cs, func(a, b int) bool {
			sa := cos(qv, vec(cs[a].text, docs)) + 0.02*float64(len(badRe.FindAllString(cs[a].text, -1)))
			sb := cos(qv, vec(cs[b].text, docs)) + 0.02*float64(len(badRe.FindAllString(cs[b].text, -1)))
			return sa > sb
		})
		cs = cs[:24]
	}
	qs := map[string]Q{}
	for i, c := range cs {
		qs[fmt.Sprintf("c%d", i)] = Noul(fmt.Sprintf("Lines %d-%d of the file help answer the question: %s\n---\n%s", c.from+1, c.to, q, clip(c.text, 3000)))
	}
	a, err := ask(map[string]any{"question": q, "file": head}, qs)
	if err != nil {
		return head + " (Jev unavailable: log view instead)\n" + logView(lines), nil
	}
	type scored struct {
		c chunk
		p float64
	}
	ss := make([]scored, len(cs))
	for i, c := range cs {
		ss[i] = scored{c, a[fmt.Sprintf("c%d", i)].P()}
	}
	sort.SliceStable(ss, func(x, y int) bool { return ss[x].p > ss[y].p })
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (Jev picked the parts that answer: %s)\n", head, q)
	shown := 0
	for _, x := range ss {
		if shown == 3 || (shown > 0 && x.p < 0.5) {
			break
		}
		fmt.Fprintf(&sb, "-- lines %d-%d (Jev %.2f)\n%s", x.c.from+1, x.c.to, x.p, numbered(lines[x.c.from:x.c.to], x.c.from+1))
		shown++
	}
	if shown == 0 {
		sb.WriteString("-- no part of the file clearly answers that; log view instead\n" + logView(lines))
	}
	return sb.String(), nil
}

func (s *Server) viewImage(p string, in ViewIn) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	img, _, err := image.Decode(bufio.NewReader(f))
	f.Close()
	if err != nil {
		return "", fmt.Errorf("can't decode %s: %v", in.Path, err)
	}
	b := img.Bounds()
	src := b
	if in.Crop != "" {
		if r, ok := cropRect(in.Crop, b); ok {
			src = r
		}
	}
	w, h := src.Dx(), src.Dy()
	scale := 1.0
	if m := max(w, h); m > viewMaxPx {
		scale = float64(viewMaxPx) / float64(m)
	}
	ow, oh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	out := boxResize(img, src, ow, oh)
	dir := s.st.path("views")
	if in.Task != "" {
		dir = filepath.Join(s.st.Dir, "tasks", slug(in.Task), "views")
	}
	os.MkdirAll(dir, 0o755)
	base := viewSuffixRe.ReplaceAllString(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)), "") // a view of a view: plants-768px.jpg, not plants-768px-768px.jpg
	name := fmt.Sprintf("%s-%dpx.jpg", base, max(ow, oh))
	if in.Crop != "" {
		name = fmt.Sprintf("%s-crop-%s-%dpx.jpg", base, slug(strings.ReplaceAll(in.Crop, ".", "")), max(ow, oh))
	}
	dst := filepath.Join(dir, name)
	df, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	jpeg.Encode(df, out, &jpeg.Options{Quality: 82})
	df.Close()
	return fmt.Sprintf("%s: %dx%d -> %s (%dx%d JPEG). Read that file once; for detail, view a crop: --crop x,y,w,h",
		rel(s.st.Root, p), b.Dx(), b.Dy(), rel(s.st.Root, dst), ow, oh), nil
}

func cropRect(spec string, b image.Rectangle) (image.Rectangle, bool) {
	parts := strings.Split(spec, ",")
	if len(parts) != 4 {
		return b, false
	}
	var v [4]float64
	for i, x := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return b, false
		}
		v[i] = f
	}
	if v[0] <= 1 && v[1] <= 1 && v[2] <= 1 && v[3] <= 1 { // fractions of the image
		v[0], v[2] = v[0]*float64(b.Dx()), v[2]*float64(b.Dx())
		v[1], v[3] = v[1]*float64(b.Dy()), v[3]*float64(b.Dy())
	}
	r := image.Rect(b.Min.X+int(v[0]), b.Min.Y+int(v[1]), b.Min.X+int(v[0]+v[2]), b.Min.Y+int(v[1]+v[3])).Intersect(b)
	return r, !r.Empty()
}

// boxResize averages the source pixels under each output pixel (stdlib only, good enough to judge a picture).
func boxResize(img image.Image, src image.Rectangle, ow, oh int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	sx, sy := float64(src.Dx())/float64(ow), float64(src.Dy())/float64(oh)
	for y := 0; y < oh; y++ {
		y0, y1 := src.Min.Y+int(float64(y)*sy), src.Min.Y+max(int(float64(y)*sy)+1, int(float64(y+1)*sy))
		for x := 0; x < ow; x++ {
			x0, x1 := src.Min.X+int(float64(x)*sx), src.Min.X+max(int(float64(x)*sx)+1, int(float64(x+1)*sx))
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1 && yy < src.Max.Y; yy++ {
				for xx := x0; xx < x1 && xx < src.Max.X; xx++ {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			if n > 0 {
				out.Set(x, y, color.RGBA64{uint16(r / n), uint16(g / n), uint16(bl / n), uint16(a / n)})
			}
		}
	}
	return out
}

// bigRead: the hook's parse check. A raw read of a big file or a big image goes back once with
// the osenv view line to use instead. Returns "" when the read is fine as is.
func (s *Server) bigRead(tool string, inp map[string]any, task string) string {
	p := toolPath(inp)
	lower := strings.ToLower(tool)
	isRead := strings.Contains(lower, "read") || strings.Contains(lower, "view") || strings.Contains(lower, "image")
	if cmd, ok := inp["command"].(string); ok {
		f := strings.Fields(cmd)
		dump := map[string]bool{"cat": true, "less": true, "more": true, "type": true, "get-content": true, "gc": true}
		if len(f) == 2 && dump[strings.ToLower(f[0])] && !strings.ContainsAny(cmd, "|>;&") {
			p, isRead = strings.Trim(f[1], `'"`), true
		}
	}
	if p == "" || !isRead {
		return ""
	}
	if _, ranged := inp["offset"]; ranged {
		return ""
	}
	if _, ranged := inp["limit"]; ranged {
		return ""
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(s.st.Root, full)
	}
	if strings.Contains(full, string(filepath.Separator)+"views"+string(filepath.Separator)) {
		return "" // already a parsed view
	}
	use := fmt.Sprintf(`%s view --task %s "%s"`, s.cli, task, p) // s.cli: callable from cmd, PowerShell and bash
	if imgExt[strings.ToLower(filepath.Ext(full))] {
		if f, err := os.Open(full); err == nil {
			c, _, err := image.DecodeConfig(f)
			f.Close()
			if err == nil && max(c.Width, c.Height) > viewMaxPx {
				return fmt.Sprintf("PARSE, DON'T DUMP: that image is %dx%d; every later call would re-send it. Run: %s (add --crop x,y,w,h for a detail), then read the small JPEG it names.", c.Width, c.Height, use)
			}
		}
		return ""
	}
	limit := bigLines
	if codeExt[strings.ToLower(filepath.Ext(full))] {
		limit = bigCode
	}
	if n := countLines(full, limit+1); n > limit {
		return fmt.Sprintf("PARSE, DON'T DUMP: that file has over %d lines; every later call would re-send all of it. Run: %s --q \"<what you need from it>\" (Jev picks the parts that answer), or read a range with offset/limit.", limit, use)
	}
	return ""
}

func countLines(p string, stop int) int {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		if n++; n >= stop {
			break
		}
	}
	return n
}

func init() {
	_ = png.Decode // register decoders
	_ = gif.Decode
}

// contactSheet: every screenshot of the task's work tiled into one JPEG in its views/ folder, each tile at
// view size (longest side 768), two columns. A qwen review costs what its session costs, and every turn
// re-sends every image it has opened: one sheet is one image. It returns the sheet's path relative to the
// project and the tiles' names in order; no screenshots, no sheet.
func (s *Server) contactSheet(name string) (string, []string) {
	var tiles []image.Image
	var names []string
	for _, p := range s.taskImages(name, sheetMax) {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(bufio.NewReader(f))
		f.Close()
		if err != nil {
			continue
		}
		b := img.Bounds()
		// Scale to the column width, not the longest side: a full-page shot scaled to 768 tall became an
		// unreadable 110-150 px sliver (Linux 0.3 run). A page taller than a tile shows its top and its bottom.
		sc := min(1.0, float64(viewMaxPx)/float64(b.Dx()))
		w, h := max(1, int(float64(b.Dx())*sc)), max(1, int(float64(b.Dy())*sc))
		if h <= sheetTileH {
			tiles = append(tiles, boxResize(img, b, w, h))
		} else {
			topH := sheetTileH - sheetBottomH - sheetCut
			srcTop := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+int(float64(topH)/sc))
			srcBot := image.Rect(b.Min.X, b.Max.Y-int(float64(sheetBottomH)/sc), b.Max.X, b.Max.Y)
			t := image.NewRGBA(image.Rect(0, 0, w, sheetTileH))
			draw.Draw(t, t.Bounds(), image.NewUniform(color.RGBA{200, 40, 40, 255}), image.Point{}, draw.Src) // the cut
			draw.Draw(t, image.Rect(0, 0, w, topH), boxResize(img, srcTop, w, topH), image.Point{}, draw.Src)
			draw.Draw(t, image.Rect(0, topH+sheetCut, w, sheetTileH), boxResize(img, srcBot, w, sheetBottomH), image.Point{}, draw.Src)
			tiles = append(tiles, t)
		}
		names = append(names, filepath.Base(p))
	}
	if len(tiles) == 0 {
		return "", nil
	}
	cols := min(2, len(tiles))
	const gap = 8
	var rowH []int
	for i, t := range tiles {
		if i%cols == 0 {
			rowH = append(rowH, 0)
		}
		rowH[len(rowH)-1] = max(rowH[len(rowH)-1], t.Bounds().Dy())
	}
	w, h := cols*viewMaxPx+(cols-1)*gap, (len(rowH)-1)*gap
	for _, r := range rowH {
		h += r
	}
	sheet := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{64, 64, 64, 255}), image.Point{}, draw.Src)
	y := 0
	for i, t := range tiles {
		if i > 0 && i%cols == 0 {
			y += rowH[i/cols-1] + gap
		}
		x := (i % cols) * (viewMaxPx + gap)
		draw.Draw(sheet, t.Bounds().Add(image.Pt(x, y)), t, t.Bounds().Min, draw.Src)
	}
	dst := s.tdir(name, "views", "sheet.jpg")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	f, err := os.Create(dst)
	if err != nil {
		return "", nil
	}
	jpeg.Encode(f, sheet, &jpeg.Options{Quality: 85})
	f.Close()
	return rel(s.st.Root, dst), names
}

// sheetMax tiles per sheet; qwenSheetTurns caps a review that has a sheet ("a small turn cap"). A tile is at
// most sheetTileH tall: a taller page shows its top, a red cut band, and its last sheetBottomH pixels.
const sheetMax, qwenSheetTurns, sheetTileH, sheetBottomH, sheetCut = 6, 16, 1200, 360, 12
