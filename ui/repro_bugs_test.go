//go:build repro

// Reproductions for the rendering findings that need access to the unexported ui
// internals (rawPainter, paintModel, pagerModel, model).
//
// Run:
//
//	go test -tags repro ./ui -run 'TestB' -v
//
// Every test asserts the *correct* behaviour, so they are RED while the bugs
// exist and turn green as each one is fixed. The failure message names the
// finding, the expected/actual numbers and the fix. See review/repro/README.md
// for the index.
package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glow/v3/review/repro"
	uv "github.com/charmbracelet/ultraviolet"
)

// ---------------------------------------------------------------- helpers ----

type stubModel struct{}

func (stubModel) Init() tea.Cmd                       { return nil }
func (stubModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return stubModel{}, nil }
func (stubModel) View() tea.View                      { return tea.NewView("stub") }

// runCmd executes a command tree synchronously, ignoring the messages it
// produces. Enough for the paint commands, which never produce a message.
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmd(c)
		}
	}
}

// widestLineWith returns the line that contains the most occurrences of sub.
func widestLineWith(content, sub string) string {
	best, bestCount := "", -1
	for _, ln := range strings.Split(content, "\n") {
		if n := strings.Count(ln, sub); n > bestCount {
			best, bestCount = ln, n
		}
	}
	return best
}

// newTestPager returns a pagerModel wired to a sized viewport, the way
// model.Update does after a WindowSizeMsg.
func newTestPager(t *testing.T, cfg Config, w, h int) pagerModel {
	t.Helper()
	common := &commonModel{cfg: cfg, styles: newStyles(true)}
	m := newPagerModel(common)
	m.currentDocument = markdown{Note: "doc.md"}
	m.setSize(w, h)
	return m
}

// -------------------------------------------------------------- B02 ----------

// B02: bubbletea does not wait for commands when it shuts down, so a paint
// command queued by the last Update can still run after painter.stop() has left
// the alternate screen. Such a late frame must not be written: it would be
// painted on top of the restored primary screen.
//
// See ui/rawpaint.go, "BUG: no p.done check" in paint().
func TestB02_NoPaintAfterStop(t *testing.T) {
	var buf bytes.Buffer
	p := newRawPainter(&buf, nil, nil, false)
	p.start()

	late := p.paint("LATE FRAME")
	p.stop()
	before := buf.Len()
	late() // runs after the cleanup, exactly what a pending command does

	if extra := buf.String()[before:]; extra != "" {
		t.Fatalf("B02: a paint command ran after stop() and wrote %d bytes to the terminal.\n"+
			"  after stop() : %q\n"+
			"  bubbletea abandons commands on shutdown (tea.go:716), so this happens\n"+
			"  on every quit that loses the race; the frame lands on the primary screen.\n"+
			"  fix: check p.done (non-blocking) under p.mu in paint() before writing.",
			len(extra), extra)
	}
}

// B05: rawPainter writes the frame verbatim: it knows the terminal size (it
// reports it through windowSize) but never uses it to bound the frame. A frame
// taller than the terminal, or a line wider than it, must be clamped; otherwise
// the terminal scrolls and the pager's origin moves.
//
// See ui/rawpaint.go, "WARNING: the frame is written verbatim".
func TestB05_FrameIsClampedToTheTerminal(t *testing.T) {
	var buf bytes.Buffer
	p := newRawPainter(&buf, nil, nil, false) // no tty: windowSize() falls back
	size := p.windowSize()

	// Too many rows.
	p.paint(strings.Repeat("row\n", size.Height+6))()
	rows := strings.Count(buf.String(), "row\r\n")
	if rows > size.Height {
		t.Fatalf("B05: the frame is not clamped to the terminal height.\n"+
			"  terminal height: %d\n"+
			"  rows written   : %d\n"+
			"  Everything below the first %d rows is scrolled away by the terminal,\n"+
			"  which shifts the pager's origin (one content line == one row no longer\n"+
			"  holds).\n"+
			"  fix: pass the window size to the painter and clamp/pad the frame.",
			size.Height, rows, size.Height)
	}

	// A line wider than the terminal.
	buf.Reset()
	p.paint(strings.Repeat("x", size.Width+120))()
	var widest int
	for _, ln := range strings.Split(buf.String(), "\n") {
		widest = max(widest, repro.DisplayWidth(strings.TrimPrefix(ln, "\x1b[?2026h")))
	}
	if widest > size.Width {
		t.Fatalf("B05: the frame is not clamped to the terminal width.\n"+
			"  terminal width: %d\n"+
			"  widest line   : %d\n"+
			"  fix: truncate (or reject) lines wider than the window before writing.",
			size.Width, widest)
	}
}

// -------------------------------------------------------------- B06 ----------

// B06: when fsnotify.NewWatcher fails, pagerModel.watcher stays nil, but
// watchFile is still queued as a command after contentRenderedMsg. It must
// become a no-op instead of dereferencing the nil watcher.
//
// See ui/pager.go, "BUG: m.watcher stays nil".
func TestB06_WatchFileWithNilWatcherDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("B06: watchFile panicked on a nil watcher: %v\n"+
				"  initWatcher only logs NewWatcher failures, and watchFile is still\n"+
				"  queued after every contentRenderedMsg, so a failed watcher takes the\n"+
				"  whole program down.\n"+
				"  fix: return early from watchFile/unwatchFile when m.watcher is nil.",
				r)
		}
	}()

	var m pagerModel // watcher is nil, as after a failed initWatcher
	m.watchFile()
}

// -------------------------------------------------------------- B07 ----------

// B07: pagerModel.Init queues renderWithGlamour, and that render runs before the
// first WindowSizeMsg has been processed. It must still wrap at the configured
// width instead of relying on the viewport size being known.
//
// See ui/pager.go, "BUG: on the first render the viewport width is still 0".
func TestB07_FirstRenderWrapsAtTheConfiguredWidth(t *testing.T) {
	const cols = 40
	common := &commonModel{
		cfg:    Config{GlamourEnabled: true, GlamourStyle: "dark", GlamourMaxWidth: cols},
		styles: newStyles(true),
	}
	m := newPagerModel(common)
	m.currentDocument = markdown{Note: "doc.md"}
	if w := m.viewport.Width(); w != 0 {
		t.Fatalf("repro setup problem: the viewport already has a width (%d)", w)
	}

	out, err := glamourRender(m, strings.Repeat("word ", 30)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	var widest int
	for _, ln := range strings.Split(out, "\n") {
		widest = max(widest, repro.DisplayWidth(ln))
	}
	if widest > cols {
		t.Fatalf("B07: the first render ignores the word wrap.\n"+
			"  viewport width : %d (no WindowSizeMsg yet)\n"+
			"  configured wrap: %d\n"+
			"  widest line    : %d\n"+
			"  width = min(GlamourMaxWidth, viewport.Width()) = 0, and\n"+
			"  glamour.WithWordWrap(0) means \"do not wrap\"; the unsized frame is\n"+
			"  painted and can even win the race against the resize render.\n"+
			"  fix: do not render until a size is known, or fall back to\n"+
			"       cfg.GlamourMaxWidth when the viewport is unsized.",
			m.viewport.Width(), cols, widest)
	}
}

// -------------------------------------------------------------- B08 ----------

// B08: with line numbers (and for code) glamourRender clamps each line with
// MaxWidth(viewportWidth - lineNumberWidth). MaxWidth truncates instead of
// re-wrapping, so enabling line numbers must not change the text itself.
//
// See ui/pager.go, "WARNING: MaxWidth truncates, it does not re-wrap".
func TestB08_LineNumbersKeepAllText(t *testing.T) {
	const cols = 40
	// glamour wraps a paragraph at the viewport width but already reserves a two
	// column left margin, so this is the longest single line it produces - and it
	// fills the line completely, i.e. the last columns are real text.
	body := strings.Repeat("a", cols-4) + "\n"

	plain := newTestPager(t, Config{
		GlamourEnabled: true, GlamourStyle: "dark", GlamourMaxWidth: cols,
	}, cols, 20)
	withNumbers := newTestPager(t, Config{
		GlamourEnabled: true, GlamourStyle: "dark", GlamourMaxWidth: cols, ShowLineNumbers: true,
	}, cols, 20)

	plainOut, err := glamourRender(plain, body)
	if err != nil {
		t.Fatal(err)
	}
	numOut, err := glamourRender(withNumbers, body)
	if err != nil {
		t.Fatal(err)
	}

	plainA := strings.Count(widestLineWith(plainOut, "a"), "a")
	numA := strings.Count(widestLineWith(numOut, "a"), "a")
	if plainA != cols-4 {
		t.Fatalf("repro setup problem: expected %d 'a's without line numbers, got %d", cols-4, plainA)
	}
	if numA != plainA {
		t.Fatalf("B08: enabling line numbers dropped %d columns of text.\n"+
			"  without line numbers: %d of %d characters survive: %q\n"+
			"  with line numbers   : %d of %d characters survive: %q\n"+
			"  MaxWidth(viewport-lineNumberWidth) truncates, but glamour wrapped at\n"+
			"  the full width, so whatever reaches the right edge is cut off.\n"+
			"  fix: pass the reduced width to glamour instead of truncating after.",
			plainA-numA, plainA, cols-4, widestLineWith(plainOut, "a"),
			numA, cols-4, widestLineWith(numOut, "a"))
	}
}

// -------------------------------------------------------------- B09 ----------

// B09: a reload of a deleted file fails, loadLocalMarkdown returns errMsg, and
// model.Update has no errMsg case: the pager drops it (only the stash stores
// it), so the user keeps looking at the stale document with no error at all.
//
// See ui/ui.go, "BUG: errMsg is never handled here".
func TestB09_ReloadErrorIsShown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	const body = "original body"
	if err := os.WriteFile(path, []byte("# Title\n\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var m tea.Model = newModel(Config{
		GlamourEnabled: true, GlamourStyle: "dark", GlamourMaxWidth: 40, Path: path,
	}, "")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})

	// Render and store the document, as the real message flow does.
	doc := m.(model).pager.currentDocument
	m, _ = m.Update(renderWithGlamour(m.(model).pager, doc.Body)())
	if !strings.Contains(m.View().Content, "original") {
		t.Fatalf("repro setup problem: the document is not displayed: %q", m.View().Content)
	}

	// The file disappears; the reload command fails.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	msg := loadLocalMarkdown(&doc)()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("repro setup problem: reload returned %T, want errMsg", msg)
	}

	m, _ = m.Update(msg)
	view := m.View().Content

	// The correct behaviour: the failed reload is visible somewhere.
	if !strings.Contains(view, "ERROR") && !strings.Contains(view, "no such file") {
		t.Fatalf("B09: the reload failed and the user was told nothing.\n"+
			"  reload error : %v\n"+
			"  stale text still on screen: %v\n"+
			"  model.Update forwards messages to the active child only; the stash has\n"+
			"  an errMsg case, the pager does not, and neither does the root model, so\n"+
			"  the error is dropped and the old frame looks current.\n"+
			"  fix: handle errMsg at the top level (e.g. show errorView like the stash).",
			msg, strings.Contains(view, "original"))
	}
}

// -------------------------------------------------------------- B11 ----------

// B11: every processed message schedules a complete "clear screen + repaint"
// frame. A frame that is identical to the one already on screen must not be
// painted again; the renderer being replaced coalesces frames and skips
// intermediate ones.
//
// See ui/rawpaint.go, "NOTE: the generation counter only keeps frames from being
// painted out of order".
func TestB11_UnchangedFrameIsNotRepainted(t *testing.T) {
	var buf bytes.Buffer
	p := newRawPainter(&buf, nil, nil, false)
	pm := paintModel{inner: stubModel{}, painter: p} // constant view

	const messages = 50
	for range messages {
		im, cmd := pm.Update(struct{}{})
		pm = im.(paintModel)
		runCmd(cmd)
	}

	repaints := strings.Count(buf.String(), "\x1b[2J")
	if repaints > 1 {
		t.Fatalf("B11: an unchanged frame was repainted %d times for %d identical messages.\n"+
			"  bytes written: %d\n"+
			"  Each message does \\x1b[2J + a full repaint; the generation counter only\n"+
			"  prevents out-of-order frames, not rewrites of identical ones. The\n"+
			"  renderer this replaces merged frames and dropped intermediate ones.\n"+
			"  fix: remember the last painted frame (skip identical ones) and/or\n"+
			"       coalesce paints to a frame budget.",
			repaints, messages, buf.Len())
	}
}

// -------------------------------------------------------------- B12 ----------

type countingModel struct{ views int }

func (c *countingModel) Init() tea.Cmd { return nil }
func (c *countingModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return c, nil
}
func (c *countingModel) View() tea.View {
	c.views++
	return tea.NewView("counting")
}

// B12: paintModel.Update builds the view for its paint command, and bubbletea
// builds it again for its own (nil) renderer right after Update returns
// (tea.go:872 Update, tea.go:880 render -> View). One message must render the
// view once, not twice.
//
// See ui/rawpaint.go, "PERF: inner.View() is rendered here".
func TestB12_OneViewRenderPerMessage(t *testing.T) {
	var buf bytes.Buffer
	p := newRawPainter(&buf, nil, nil, false)
	cm := &countingModel{}
	pm := paintModel{inner: cm, painter: p}

	// Exactly what bubbletea does for one message.
	im, cmd := pm.Update(struct{}{})
	pm = im.(paintModel)
	runCmd(cmd)
	_ = pm.View()

	if cm.views != 1 {
		t.Fatalf("B12: one message rendered the view %d times.\n"+
			"  paintModel.Update calls inner.View() for the paint command, then\n"+
			"  bubbletea calls model.View() again for its renderer and throws the\n"+
			"  result away (nilRenderer).\n"+
			"  fix: paint from View() (called once per message) instead of Update,\n"+
			"       or let View return the already-built frame.",
			cm.views)
	}
}

// -------------------------------------------------------------- B13 ----------

// B13: the painter replaces the renderer, so it must also mirror the terminal
// modes the renderer enables. The one with an observable consequence is
// bracketed paste: without it the terminal sends a pasted block as raw bytes, so
// every newline arrives as its own key press.
//
// This test is self-contained evidence: it first decodes the bytes a terminal
// sends for a two-line paste with the same decoder bubbletea uses, then asserts
// that the painter asked for bracketed paste.
//
// See ui/rawpaint.go, the NOTE in start(), and ui/ui.go, the note under View.
func TestB13_MultiLinePasteArrivesAsKeyPresses(t *testing.T) {
	// 1. What the terminal sends without bracketed paste.
	var decoder uv.EventDecoder
	raw := []byte("doc\nq")
	var ks []string
	for len(raw) > 0 {
		n, ev := decoder.Decode(raw)
		if n == 0 {
			break
		}
		raw = raw[n:]
		if k, ok := ev.(uv.KeyPressEvent); ok {
			ks = append(ks, uv.Key(k).String())
		}
	}
	t.Logf("pasting %q without bracketed paste decodes to key presses: %q", "doc\nq", ks)
	if len(ks) != 5 || ks[3] != "ctrl+j" {
		t.Fatalf("repro setup problem: unexpected decoding %q", ks)
	}
	// The filter (ui/stash.go, handleFiltering) treats ctrl+j like Enter, so the
	// filter commits there and the "q" then acts on the listing - it quits glow.

	// 2. The painter must therefore enable bracketed paste, like the renderer it
	// replaces does (cursed_renderer.go: ansi.SetModeBracketedPaste).
	var out bytes.Buffer
	p := newRawPainter(&out, nil, nil, false)
	p.start()
	if !strings.Contains(out.String(), "\x1b[?2004h") {
		t.Fatalf("B13: rawPainter.start() does not enable bracketed paste.\n"+
			"  A pasted block is delivered as individual key presses (%q), so the\n"+
			"  newline becomes ctrl+j: the file filter commits on it and the next\n"+
			"  character acts on the listing (\"q\" quits).\n"+
			"  The default renderer enables bracketed paste (?2004h) and resets it on\n"+
			"  close, so pasting works there; the raw painter must do the same.\n"+
			"  fix: emit ?2004h in start() and ?2004l in stop().",
			ks)
	}

	out.Reset()
	p.stop()
	if !strings.Contains(out.String(), "\x1b[?2004l") {
		t.Fatalf("B13: rawPainter.stop() does not reset bracketed paste.\n" +
			"  start() enabled it, so stop() has to disable it, otherwise the mode\n" +
			"  leaks into the shell after glow exits.\n" +
			"  fix: emit ?2004l in stop().")
	}
}

// -------------------------------------------------------------- B15 ----------

// B15: watchResize calls prog.Send before Run has started; msgs is unbuffered,
// so the goroutine blocks inside Send until the event loop runs. If Run is never
// called, the goroutine and its SIGWINCH handler leak - and stop() cannot wake
// it, because the goroutine is not in the select yet.
//
// See ui/rawpaint.go, "NOTE: prog.Send() blocks until the event loop is running".
func TestB15_WatchResizeStopsAfterStop(t *testing.T) {
	stacks := func() string {
		buf := make([]byte, 1<<20)
		return string(buf[:runtime.Stack(buf, true)])
	}
	hasWatch := func() bool { return strings.Contains(stacks(), "rawPainter).watchResize") }

	var buf bytes.Buffer
	p := newRawPainter(&buf, nil, nil, false)
	prog := tea.NewProgram(stubModel{}, tea.WithoutRenderer())

	if hasWatch() {
		t.Fatal("repro setup problem: a watchResize goroutine exists before the test")
	}
	p.watchResize(prog)

	deadline := time.Now().Add(2 * time.Second)
	for !hasWatch() {
		if time.Now().After(deadline) {
			t.Fatal("repro setup problem: watchResize did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	p.stop()
	time.Sleep(100 * time.Millisecond)

	if hasWatch() {
		t.Fatalf("B15: watchResize did not stop after painter.stop().\n" +
			"  The goroutine is still blocked in Program.Send (msgs is unbuffered\n" +
			"  and Run was never called), so p.done is never observed and\n" +
			"  winchStop() never runs: goroutine plus SIGWINCH handler leak.\n" +
			"  fix: give the watcher its own context/cancel, or check p.done before\n" +
			"       the blocking Send (e.g. send from a select with p.done).")
	}
}
