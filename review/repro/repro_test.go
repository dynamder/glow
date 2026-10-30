//go:build repro

// Reproductions for the rendering findings that can be triggered through the
// public API only (no terminal needed).
//
// Run:
//
//	go test -tags repro ./review/repro -v
//
// Every test asserts the *correct* behaviour, so they are RED while the bugs
// exist and turn green as each one is fixed. The failure message names the
// finding, the expected/actual numbers and the fix. See README.md in this
// directory for the index.
package repro_test

import (
	"strings"
	"testing"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/glow/v3/review/repro"
	"charm.land/glow/v3/utils"
	uv "github.com/charmbracelet/ultraviolet"
)

// headingRenderer mirrors what utils.GlamourStyle does for markdown: heading
// markers in the style config, then ApplyTextSizing on the rendered output.
func headingRenderer(t *testing.T, wrap int) *glamour.TermRenderer {
	t.Helper()

	cfg := styles.DarkStyleConfig
	// The dark style gives H1 a background colour, which AddHeadingSizeMarkers
	// deliberately leaves unmarked; drop it so H1 is exercised too.
	cfg.H1.BackgroundColor = nil
	utils.AddHeadingSizeMarkers(&cfg)

	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// B01: when the raw painter is unavailable (Windows always, or any platform
// where /dev/tty cannot be opened) ui.NewProgram falls back to the normal
// renderer. That renderer parses the frame into a cell buffer and drops an
// OSC 66 together with its payload, so the heading text must not be wrapped in
// OSC 66 in the first place: text sizing has to stay off whenever the painter is
// not used.
//
// See ui/ui.go ("BUG: this fallback keeps text sizing enabled") and
// utils/textsize.go (detectTextSizing).
func TestB01_HeadingTextSurvivesTheFallbackRenderer(t *testing.T) {
	r := headingRenderer(t, 40)
	out, err := r.Render("# Hello\n\nBody text.\n")
	if err != nil {
		t.Fatal(err)
	}
	frame := utils.ApplyTextSizing(out)

	// What bubbletea's default renderer does with that frame
	// (cursed_renderer.go: uv.NewStyledString(view.Content).Draw(cellbuf)).
	screen := uv.NewScreenBuffer(40, 8)
	uv.NewStyledString(frame).Draw(screen, screen.Bounds())
	shown := screen.String()

	if !strings.Contains(shown, "Hello") {
		t.Fatalf("B01: the heading text is not on screen.\n"+
			"  text sizing is enabled while the default renderer is used, and that\n"+
			"  renderer drops an OSC 66 together with its payload (the heading itself).\n"+
			"  screen: %q\n"+
			"  fix: disable text sizing in ui.NewProgram when openRawTerminal fails\n"+
			"       (which means the once-cached decision has to move to Config).",
			shown)
	}
	if !strings.Contains(shown, "Body text.") {
		t.Fatalf("B01: the body text is missing too, the reproduction is not exercising the heading path: %q", shown)
	}
}

// B04: glamour wraps heading text at the body word-wrap width and the OSC 66
// payload then triples (H1) or doubles (H2) its advance width. The rendered
// content must still fit the terminal, otherwise the terminal wraps it itself
// and the block occupies more screen rows than the viewport counts.
//
// See utils/textsize.go (ApplyTextSizing and headingRows) and
// ui/pager.go (glamourRender, where ApplyTextSizing is called).
func TestB04_ScaledHeadingFitsTheTerminal(t *testing.T) {
	const cols = 40
	r := headingRenderer(t, cols)
	out, err := r.Render("# Getting Started With Glow\n")
	if err != nil {
		t.Fatal(err)
	}
	sized := utils.ApplyTextSizing(out)

	var widest int
	for _, ln := range strings.Split(sized, "\n") {
		widest = max(widest, repro.DisplayWidth(ln))
	}
	physical := repro.PhysicalRows(sized, cols)
	counted := repro.ContentRows(sized)

	if widest > cols || physical != counted {
		t.Fatalf("B04: the scaled heading does not fit a %d column terminal.\n"+
			"  widest line        : %d columns\n"+
			"  rows a terminal uses: %d\n"+
			"  rows the viewport tracks: %d (drift %d)\n"+
			"  With s=3 the heading keeps its natural width times three; the wrap\n"+
			"  width is still the body width, and no width helper in the codebase can\n"+
			"  measure the payload (reflow over-counts it, lipgloss reports zero).\n"+
			"  fix: derive w/n/d from the columns left on the line, or skip scaling\n"+
			"       for headings that would not fit.",
			cols, widest, physical, counted, physical-counted)
	}
}

// B10: ApplyTextSizing owns the \x1b]6666; marker namespace. It must consume
// every marker it recognises and never emit one of its own markers to the
// terminal. markerLevel only accepts a single digit while markerOpen can write
// several, so a level >= 10 marker is re-emitted verbatim today.
//
// See utils/textsize.go (markerLevel and the level<1||level>3 branch).
func TestB10_NoRawMarkerReachesTheTerminal(t *testing.T) {
	inputs := []string{
		// Single-digit levels outside 1..3 must fail open to plain text.
		"\x1b]6666;4\x1b\\Title\x1b]6666;0\x1b\\",
		"\x1b]6666;9\x1b\\Title\x1b]6666;0\x1b\\",
		// Multi-digit levels: markerOpen can produce these even if nothing marks
		// them today.
		"\x1b]6666;10\x1b\\Title\x1b]6666;0\x1b\\",
	}
	for _, in := range inputs {
		got := utils.ApplyTextSizing(in)
		if strings.Contains(got, "\x1b]6666;") {
			t.Fatalf("B10: ApplyTextSizing emitted its own marker to the terminal.\n"+
				"  in : %q\n"+
				"  out: %q\n"+
				"  An unrecognised marker is consumed as plain text (which levels 4..9\n"+
				"  already do); it must never be forwarded as an escape sequence.\n"+
				"  fix: make markerLevel parse a multi-digit level.",
				in, got)
		}
	}
}
