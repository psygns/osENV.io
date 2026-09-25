package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// osenv proof: one command per common kind of evidence, and one command for the desk to check it all.
// No server needed. Saving proof is where the most common mistake happened (PowerShell's > writes UTF-16,
// which other tools read as binary) and checking proof is where the desk spent most of its time.
//
//	osenv proof run <out.txt> -- <command> [args]   a UTF-8 transcript: the command, its output, its exit code
//	osenv proof http <out.txt> <url> [--status 200] [--contains <text>]   a quick check of a running server
//	osenv proof shot <out.png> <page.html|url> [--width 768]   a full-page screenshot in a headless browser
//	osenv proof check <file|folder>...   every proof file: broken, blank, failed or ok

func proofClient(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: osenv proof run <out.txt> -- <command> | http <out.txt> <url> | shot <out.png> <page|url> | mutate <out.txt> <source> -- <tests> | check <file|folder>...")
		os.Exit(2)
	}
	code := 0
	var err error
	if helpFlag(args) { // proof run --help and the like: the usage, never a usage error
		args = []string{"--help"}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println("usage: osenv proof run <out.txt> [--expect N] -- <command> | http <out.txt> <url> [--status 200] [--contains X] | shot <out.png> <page|url> [--width 768] | mutate <out.txt> <source> [--n 10] [--allow N] -- <tests> | check <file|folder>...")
		os.Exit(0)
	case "run":
		code, err = proofRunArgs(args[1:])
	case "http":
		code, err = proofHTTPArgs(args[1:])
	case "shot":
		code, err = proofShotArgs(args[1:])
	case "check":
		code = proofCheck(os.Stdout, args[1:])
	case "mutate":
		code, err = proofMutateArgs(args[1:])
	default:
		err = fmt.Errorf("unknown proof kind %q (run, http, shot, mutate or check)", args[0])
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "osenv proof:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func proofRunArgs(args []string) (int, error) {
	i := indexOf(args, "--")
	fs := flag.NewFlagSet("proof run", flag.ContinueOnError)
	expect := fs.Int("expect", 0, "the exit code this run is meant to have (a negative test: 1)")
	var pos []string
	if i > 0 {
		pos = parseAnyOrder(fs, args[:i])
	}
	if len(pos) != 1 || len(args) < i+2 {
		return 2, fmt.Errorf("usage: osenv proof run <out.txt> [--expect <code>] -- <command> [args]")
	}
	code, text, err := proofRunExpect(pos[0], args[i+1:], *expect)
	fmt.Print(tail(text, 20) + "\n")
	fmt.Printf("saved %s (exit=%d, expected %d)\n", pos[0], code, *expect)
	if code == *expect {
		return 0, err
	}
	return max(code, 1), err
}

// proofRunExpect: proofRun for a run meant to exit with a given code (a negative test); proof check reads the expect line.
func proofRunExpect(out string, argv []string, expect int) (int, string, error) {
	code, t, err := proofRun(out, argv)
	if err != nil || expect == 0 {
		return code, t, err
	}
	t += fmt.Sprintf("expect=%d (a negative test: exit %d is the intended result)\n", expect, expect)
	return code, t, os.WriteFile(out, []byte(t), 0o644)
}

// proofRun runs argv (no shell: pass "sh -c ..." or "cmd /c ..." for shell syntax) and saves the transcript.
// Python is told to write UTF-8; UTF-16 output is converted and NUL bytes dropped, so the file is always UTF-8.
func proofRun(out string, argv []string) (int, string, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	start := time.Now()
	code := 0
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 127
			buf.WriteString(err.Error() + "\n")
		}
	}
	text := strings.ReplaceAll(strings.ToValidUTF8(string(plainText(buf.Bytes())), "\uFFFD"), "\x00", "")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	t := fmt.Sprintf("$ %s\n%sexit=%d (osenv proof run, %s, %.1fs)\n", shellLine(argv), text, code, now(), time.Since(start).Seconds())
	os.MkdirAll(filepath.Dir(out), 0o755)
	return code, t, os.WriteFile(out, []byte(t), 0o644)
}

func proofHTTPArgs(args []string) (int, error) {
	fs := flag.NewFlagSet("proof http", flag.ContinueOnError)
	status := fs.Int("status", 200, "the status code that counts as up")
	contains := fs.String("contains", "", "text the body must contain")
	pos := parseAnyOrder(fs, args)
	if len(pos) != 2 {
		return 2, fmt.Errorf("usage: osenv proof http <out.txt> <url> [--status 200] [--contains <text>]")
	}
	ok, t, err := proofHTTP(pos[0], pos[1], *status, *contains)
	fmt.Print(t)
	if err != nil || !ok {
		return 1, err
	}
	return 0, nil
}

func proofHTTP(out, url string, status int, contains string) (bool, string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return false, "", fmt.Errorf("proof http needs an http:// or https:// URL")
	}
	t := "GET " + url + "\n"
	ok := false
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(url)
	if err != nil {
		t += "error: " + err.Error() + "\n"
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		text := strings.ToValidUTF8(string(plainText(body)), "\uFFFD")
		t += fmt.Sprintf("status: %d\ncontent-type: %s\nbody (%d bytes, first 2000):\n%s\n", resp.StatusCode, resp.Header.Get("Content-Type"), len(body), clip(text, 2000))
		ok = resp.StatusCode == status && (contains == "" || strings.Contains(text, contains))
	}
	want := fmt.Sprintf("status %d", status)
	if contains != "" {
		want += fmt.Sprintf(" and a body containing %q", contains)
	}
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
	}
	t += fmt.Sprintf("result: %s (expected %s; osenv proof http, %s)\n", verdict, want, now())
	os.MkdirAll(filepath.Dir(out), 0o755)
	return ok, t, os.WriteFile(out, []byte(t), 0o644)
}

func proofShotArgs(args []string) (int, error) {
	fs := flag.NewFlagSet("proof shot", flag.ContinueOnError)
	width := fs.Int("width", 768, "the page width in pixels")
	pos := parseAnyOrder(fs, args)
	if len(pos) != 2 {
		return 2, fmt.Errorf("usage: osenv proof shot <out.png> <page.html|url> [--width 768]")
	}
	w, h, browser, err := proofShot(pos[0], pos[1], *width)
	if err != nil {
		return 1, err
	}
	fmt.Printf("saved %s (%dx%d, full page, %s)\n", pos[0], w, h, browser)
	return 0, nil
}

// proofShot: a full-page screenshot. Firefox captures the whole page at a given width; Chrome and Edge
// capture a tall window, and the empty band below the page is trimmed. The browser profile lives beside
// the output and is removed after (never /tmp: it hides the evidence and can hang a headless run).
func proofShot(out, target string, width int) (int, int, string, error) {
	url := target
	if !strings.Contains(target, "://") {
		abs, err := filepath.Abs(target)
		if err != nil {
			return 0, 0, "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return 0, 0, "", fmt.Errorf("no page at %s", abs)
		}
		url = "file://" + filepath.ToSlash(abs)
		if runtime.GOOS == "windows" {
			url = "file:///" + filepath.ToSlash(abs)
		}
	}
	out, _ = filepath.Abs(out)
	os.MkdirAll(filepath.Dir(out), 0o755)
	os.Remove(out)
	prof, err := os.MkdirTemp(filepath.Dir(out), ".osenv-shot-")
	if err != nil {
		return 0, 0, "", err
	}
	defer os.RemoveAll(prof)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name, chrome := findBrowser()
	if name == "" {
		return 0, 0, "", fmt.Errorf("no headless browser found (firefox, chrome, chromium or edge)")
	}
	var c *exec.Cmd
	narrow := chrome && width < chromeMinWidth
	if narrow {
		// Edge on Windows lays a page out at about 500 px however narrow the window, and the PNG was a slice of
		// that wider layout (Windows 0.3 run). An iframe of exactly the width renders the true layout; the
		// shot is then cut to the iframe.
		wrap := filepath.Join(prof, "frame.html")
		os.WriteFile(wrap, []byte(fmt.Sprintf(`<!doctype html><html><body style="margin:0;background:#fff"><iframe src=%q scrolling="no" style="display:block;border:0;width:%dpx;height:%dpx"></iframe></body></html>`, url, width, 8000)), 0o644)
		url = "file://" + filepath.ToSlash(wrap)
		if runtime.GOOS == "windows" {
			url = "file:///" + filepath.ToSlash(wrap)
		}
	}
	if !chrome {
		c = exec.CommandContext(ctx, name, "--headless", "--no-remote", "--profile", prof, "--window-size", fmt.Sprint(width), "--screenshot", out, url)
	} else {
		c = exec.CommandContext(ctx, name, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--allow-file-access-from-files", "--user-data-dir="+prof,
			fmt.Sprintf("--window-size=%d,%d", max(width, chromeMinWidth), 8000), "--screenshot="+out, url)
	}
	msg, err := c.CombinedOutput()
	if err != nil && !fileExists(out) {
		return 0, 0, "", fmt.Errorf("%s: %v: %s", filepath.Base(name), err, clip(string(msg), 300))
	}
	if narrow {
		cropWidth(out, width)
	}
	if chrome {
		trimBottom(out)
	}
	f, err := os.Open(out)
	if err != nil {
		return 0, 0, "", fmt.Errorf("the browser wrote no screenshot")
	}
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		return 0, 0, "", fmt.Errorf("the screenshot doesn't decode: %v", err)
	}
	return cfg.Width, cfg.Height, filepath.Base(name), nil
}

func findBrowser() (string, bool) {
	if b := os.Getenv("OSENV_BROWSER"); b != "" { // pick one: a path or a name on PATH
		if p, err := exec.LookPath(b); err == nil {
			return p, !strings.Contains(strings.ToLower(filepath.Base(p)), "firefox")
		}
	}
	for _, n := range []string{"firefox", "firefox-esr"} {
		if p, err := exec.LookPath(n); err == nil {
			return p, false
		}
	}
	for _, n := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(n); err == nil {
			return p, true
		}
	}
	for _, p := range []string{`C:\Program Files\Mozilla Firefox\firefox.exe`} {
		if fileExists(p) {
			return p, false
		}
	}
	for _, p := range []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, `C:\Program Files\Microsoft\Edge\Application\msedge.exe`} {
		if fileExists(p) {
			return p, true
		}
	}
	return "", false
}

// chromeMinWidth: below this, Chrome and Edge shots go through an iframe of the exact width.
const chromeMinWidth = 600

// cropWidth keeps the left w pixels of a PNG.
func cropWidth(p string, w int) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil || img.Bounds().Dx() <= w {
		return
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return
	}
	b := img.Bounds()
	if g, err := os.Create(p); err == nil {
		png.Encode(g, sub.SubImage(image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Max.Y)))
		g.Close()
	}
}

// trimBottom drops the band of rows below the page that all match the last row's first pixel.
func trimBottom(p string) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		return
	}
	b := img.Bounds()
	bg := img.At(b.Min.X, b.Max.Y-1)
	same := func(y int) bool {
		for x := b.Min.X; x < b.Max.X; x += max(1, b.Dx()/64) {
			if img.At(x, y) != bg {
				return false
			}
		}
		return true
	}
	y := b.Max.Y
	for y > b.Min.Y+1 && same(y-1) {
		y--
	}
	if y >= b.Max.Y-1 {
		return
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return
	}
	g, err := os.Create(p)
	if err != nil {
		return
	}
	png.Encode(g, sub.SubImage(image.Rect(b.Min.X, b.Min.Y, b.Max.X, y+8)))
	g.Close()
}

// proofCheck: one line per proof file. BROKEN (UTF-16, NUL bytes, not UTF-8, doesn't decode), BLANK (an image of
// one colour), FAILED (a transcript whose exit code isn't 0, or an http check that failed), or ok. Exit 1 if any isn't ok.
func proofCheck(w io.Writer, paths []string) int {
	var files []string
	for _, p := range paths {
		filepath.WalkDir(p, func(f string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !strings.Contains(filepath.ToSlash(f), "/.osenv-shot-") {
				files = append(files, f)
			}
			return nil
		})
	}
	if len(files) == 0 {
		fmt.Fprintln(w, "no proof files found in", strings.Join(paths, ", "))
		return 1
	}
	bad := 0
	for _, f := range files {
		v := checkOne(f)
		if !strings.HasPrefix(v, "ok") {
			bad++
		}
		fmt.Fprintf(w, "%-8s %s\n", strings.SplitN(v, " ", 2)[0], f+"  "+strings.TrimPrefix(v, strings.SplitN(v, " ", 2)[0]))
	}
	fmt.Fprintf(w, "%d files, %d not ok\n", len(files), bad)
	if bad > 0 {
		return 1
	}
	return 0
}

func checkOne(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return "BROKEN can't read: " + err.Error()
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	f.Close()
	ext := strings.ToLower(filepath.Ext(p))
	// Video, audio and other binaries by their first bytes or extension: never read as text (video bench, Windows:
	// every MP4 came back "BROKEN has NUL bytes"), and never read whole into memory.
	if kind, media := binaryKind(head, ext); kind != "" {
		info, _ := os.Stat(p)
		if !media {
			return fmt.Sprintf("ok %s, %d bytes (binary, not checked)", kind, info.Size())
		}
		return checkMedia(p, kind, info.Size())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "BROKEN can't read: " + err.Error()
	}
	if imgExt[ext] {
		img, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			return "BROKEN doesn't decode as an image"
		}
		bd := img.Bounds()
		first, blank := img.At(bd.Min.X, bd.Min.Y), true
		for y := bd.Min.Y; y < bd.Max.Y && blank; y += max(1, bd.Dy()/48) {
			for x := bd.Min.X; x < bd.Max.X; x += max(1, bd.Dx()/48) {
				if img.At(x, y) != first {
					blank = false
					break
				}
			}
		}
		if blank {
			return fmt.Sprintf("BLANK %dx%d, one colour (a blank screenshot, unless it's a frame of flat-colour test media)", bd.Dx(), bd.Dy()) // Linux video bench #36
		}
		return fmt.Sprintf("ok %dx%d image", bd.Dx(), bd.Dy())
	}
	switch {
	case len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff):
		return "BROKEN UTF-16 (a PowerShell > or Out-File without -Encoding utf8?): other tools read it as binary"
	case bytes.IndexByte(b, 0) >= 0:
		return "BROKEN has NUL bytes (UTF-16 without a BOM?)"
	case !utf8.Valid(b):
		return "BROKEN not valid UTF-8"
	}
	s := string(b)
	expect := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "expect=") {
			fmt.Sscanf(l, "expect=%d", &expect)
		}
	}
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.HasPrefix(l, "exit=") {
			var code int
			fmt.Sscanf(l, "exit=%d", &code)
			if code != expect {
				return fmt.Sprintf("FAILED exit=%d (expected %d): %s", code, expect, firstLine(s))
			}
			if expect != 0 {
				return fmt.Sprintf("ok exit=%d, as expected (a negative test): %s", code, firstLine(s))
			}
			return "ok exit=0: " + firstLine(s)
		}
		if strings.HasPrefix(l, "result: FAIL") {
			return "FAILED " + l
		}
		if strings.HasPrefix(l, "result: PASS") {
			return "ok " + l
		}
	}
	return fmt.Sprintf("ok UTF-8 text, %d lines (no exit code recorded)", strings.Count(s, "\n"))
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

var mediaExt = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true, ".ts": true,
	".mpg": true, ".mpeg": true, ".wav": true, ".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true, ".flac": true}

// binaryKind: a binary format by its first bytes (or a media extension, so a corrupt clip is still checked as media),
// and whether it's audio or video.
func binaryKind(h []byte, ext string) (string, bool) {
	at := func(i int, s string) bool { return len(h) >= i+len(s) && string(h[i:i+len(s)]) == s }
	switch {
	case at(4, "ftyp"):
		return "mp4/mov", true
	case at(0, "\x1a\x45\xdf\xa3"):
		return "mkv/webm", true
	case at(0, "RIFF") && at(8, "AVI "):
		return "avi", true
	case at(0, "RIFF") && at(8, "WAVE"):
		return "wav", true
	case at(0, "OggS"):
		return "ogg", true
	case at(0, "fLaC"):
		return "flac", true
	case at(0, "ID3"):
		return "mp3", true
	case at(0, "%PDF"):
		return "pdf", false
	case at(0, "PK\x03\x04"):
		return "zip", false
	case at(0, "\x1f\x8b"):
		return "gzip", false
	case mediaExt[ext]:
		return strings.TrimPrefix(ext, "."), true
	}
	return "", false
}

// checkMedia: ffprobe reads it, when ffprobe is here. A file it can't read is BROKEN.
func checkMedia(p, kind string, size int64) string {
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		return fmt.Sprintf("ok %s media, %d bytes (not checked: no ffprobe on PATH)", kind, size)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var errb bytes.Buffer
	c := exec.CommandContext(ctx, fp, "-v", "error", "-show_entries", "format=duration:stream=codec_type,codec_name,width,height", "-of", "json", p)
	c.Stderr = &errb
	out, err := c.Output()
	if err != nil {
		return "BROKEN ffprobe can't read it: " + clip(firstLine(strings.TrimSpace(errb.String())+" "+err.Error()), 200)
	}
	var r struct {
		Format  struct{ Duration string }
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int
			Height    int
		}
	}
	json.Unmarshal(out, &r)
	var parts []string
	for _, st := range r.Streams {
		if st.CodecType == "video" {
			parts = append(parts, fmt.Sprintf("%s %dx%d", st.CodecName, st.Width, st.Height))
		} else if st.CodecType == "audio" {
			parts = append(parts, st.CodecName)
		}
	}
	if len(parts) == 0 {
		return "BROKEN ffprobe found no audio or video stream"
	}
	return fmt.Sprintf("ok %s s media: %s", r.Format.Duration, strings.Join(parts, ", "))
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./\\-]+$`)

// shellLine: argv as it would be typed again, quoting what needs it (Linux light run: `sh -c 'pgrep -af "[s]leep 300"'`
// was recorded as `sh -c pgrep -af "[s]leep 300"`, which replays as a different command).
func shellLine(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a != "" && plainArg.MatchString(a):
			out[i] = a
		case runtime.GOOS == "windows": // PowerShell: '' inside single quotes
			out[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
		default:
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
