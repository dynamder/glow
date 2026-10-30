package utils

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/glamour/v2/ansi"
	"github.com/muesli/cancelreader"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

const (
	osc66ST         = "\x1b\\"
	osc66MaxPayload = 4000

	markerPrefix = "\x1b]6666;"
	markerClose  = markerPrefix + "0" + osc66ST
)

// NOTE: caching the decision with sync.OnceValue makes it irreversible. That is
// a problem for the caller: ui.NewProgram needs to turn sizing back off when it
// falls back to the normal renderer (the OSC 66 payload would be eaten there),
// and it cannot. textsize_test.go works around it by reassigning this variable,
// which is a smell. This belongs in Config, decided once in main, before any
// renderer exists.
var textSizingEnabled = sync.OnceValue(detectTextSizing)

// TextSizingEnabled reports whether the terminal supports kitty's text
// sizing protocol (OSC 66), auto-detecting support on first call and caching
// the result. Set GLOW_TEXT_SIZING to on/off to override detection.
func TextSizingEnabled() bool {
	return textSizingEnabled()
}

func detectTextSizing() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GLOW_TEXT_SIZING"))) {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
		// ERROR: rely on the terminal behavior, if the terminal don't discard the control sequence and not support Kitty OSC 66, it might cause issue. But alacritty works fine.
		// NOTE: in the TUI it is worse than "might cause issue": the fallback
		// renderer discards the OSC 66 *together with its payload text*, so the
		// headings disappear rather than render unscaled (verified with
		// charmbracelet/ultraviolet's ScreenBuffer: "A\x1b]66;s=3;HIDDEN\x1b\\B"
		// comes out as "AB"). Forcing this on must therefore also guarantee that
		// the raw painter runs, which the current env override does not (see
		// ui.NewProgram).
	}

	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	if termEnv := os.Getenv("TERM"); termEnv == "" || termEnv == "dumb" {
		return false
	}

	if supported, decisive := probeTextSizing(); decisive {
		return supported
	}

	return os.Getenv("KITTY_WINDOW_ID") != "" ||
		os.Getenv("TERM_PROGRAM") == "kitty" ||
		strings.Contains(os.Getenv("TERM"), "kitty")
}

var cprPattern = regexp.MustCompile(`\x1b\[(\d+);(\d+)R`)

// NOTE: this talks to /dev/tty directly: it puts the tty into raw mode, writes
// two cursor-position queries plus a probe OSC 66 and gives up after 800ms.
// Measured on a pty: a plain "glow README.md" does emit one ESC[6n and one
// OSC 66, so the probe also runs on the non-TUI path (via utils.GlamourStyle ->
// TextSizingEnabled), not only in the TUI.
//   - A cursor-position reply that arrives after the timeout stays in the tty
//     input queue; bubbletea would then read it as keystrokes.
//   - Only the first two replies are consumed; anything else is left behind.
//
// That failure needs a terminal that answers slower than 800ms, which could not
// be reproduced here, so this is documented rather than counted as a fixable bug.
// Draining the input queue before returning, and only probing in TUI mode, would
// remove both.
func probeTextSizing() (supported, decisive bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, false
	}
	defer func() { _ = tty.Close() }()

	fd := int(tty.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false, false
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	reader, err := cancelreader.NewReader(tty)
	if err != nil {
		return false, false
	}
	defer func() { _ = reader.Close() }()

	if _, err := io.WriteString(tty, "\r\x1b[6n\x1b]66;w=2; \a\x1b[6n\r"); err != nil {
		return false, false
	}

	timer := time.AfterFunc(800*time.Millisecond, func() { reader.Cancel() })
	defer timer.Stop()

	var buf []byte
	tmp := make([]byte, 128)
	nextCol := func() (int, bool) {
		for {
			if loc := cprPattern.FindSubmatchIndex(buf); loc != nil {
				col, _ := strconv.Atoi(string(buf[loc[4]:loc[5]]))
				buf = buf[loc[1]:]
				return col, true
			}
			n, rerr := reader.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if rerr != nil {
				return 0, false
			}
		}
	}

	before, ok := nextCol()
	if !ok {
		return false, false
	}
	after, ok := nextCol()
	if !ok {
		return false, true
	}
	return after-before == 2, true
}

func markerOpen(level int) string {
	return markerPrefix + strconv.Itoa(level) + osc66ST
}

// NOTE: only a single-digit level is accepted, while markerOpen(level) happily
// writes longer numbers. A level >= 10 marker is therefore unrecognised:
// ApplyTextSizing re-emits markerPrefix and treats the rest as text, so the raw
// escape sequence is passed through to the terminal instead of being consumed.
// Latent only because nothing marks levels above 3 today.
func markerLevel(s string) int {
	if !strings.HasPrefix(s, markerPrefix) {
		return -1
	}
	rest := s[len(markerPrefix):]
	if len(rest) < 1+len(osc66ST) || rest[0] < '0' || rest[0] > '9' || !strings.HasPrefix(rest[1:], osc66ST) {
		return -1
	}
	return int(rest[0] - '0')
}

// AddHeadingSizeMarkers marks the heading styles of levels 1 to 3 in the
// given style config so that ApplyTextSizing can find the rendered heading
// text and scale it. Headings with a background color are left unmarked:
// the background does not cover the scaled glyphs and renders broken.
// The style config is modified in place; pass a copy if the original is
// shared.
//
// WARNING: This function only used in test and GlamourStyle, the later is in this file, why expose this?
// This function bury a pin in the style config, so we can replace the pin with kitty osc66ST later.
//
// WARNING: it also mutates the config in place, and the "copy" that
// styleConfigFor hands over is a shallow struct copy: pointer fields (e.g. the
// *uint margins set for code styles) still alias styles.DefaultStyles. Only
// strings are written today, so it happens to be safe - but the next person to
// touch a pointer field here corrupts the global default style for every later
// renderer.
//
// NOTE: for styles whose prefix starts with '#', the entire prefix is dropped
// (see below), so H2/H3 lose their "##"/"###" indicator; for any other prefix the
// prefix is kept and *scaled* (H1's single leading space becomes three columns).
// Two visually different outcomes from one rule, and a single TrimPrefix only
// strips one space, so a custom "##   Title" keeps the leftovers.
func AddHeadingSizeMarkers(cfg *ansi.StyleConfig) {
	blocks := []struct {
		level int
		block *ansi.StyleBlock
	}{
		{1, &cfg.H1},
		{2, &cfg.H2},
		{3, &cfg.H3},
	}
	for _, b := range blocks {
		if b.block.BackgroundColor != nil {
			continue
		}
		prefix := b.block.Prefix
		if strings.HasPrefix(prefix, "#") {
			prefix = strings.TrimPrefix(strings.TrimLeft(prefix, "#"), " ")
		}
		b.block.Prefix = markerOpen(b.level) + prefix
		b.block.Suffix += markerClose
	}
}

func headingRows(level int) int {
	// WARNING: the row budget is hard-coded and applied blindly: it is only
	// correct if the terminal scales s=3 into exactly three rows and the 1.5x
	// span into two, and it is emitted even when the heading was re-wrapped by
	// the terminal because it did not fit (see the BUG note in ApplyTextSizing).
	// Nothing here can detect that, so the whole vertical compensation assumes
	// headingRows(level) terminal rows per rendered line, whatever really happens.
	if level == 1 {
		return 3
	}
	return 2
}

// ApplyTextSizing replaces the heading markers inserted by
// AddHeadingSizeMarkers with kitty text sizing (OSC 66) sequences: level 1
// headings are scaled 3x, level 2 headings 2x, and level 3 headings 1.5x via
// fractional scaling with a tight width so glyphs keep their natural width.
// Output without markers is returned unchanged.
//
// BUG: a scaled heading can end up wider than the terminal, and nothing here can
// notice. glamour wrapped the heading text at the body word-wrap width (see
// ansi/heading.go, lipgloss.Wrap at bs.Width(ctx)), then s=3 triples its advance
// width; no re-wrap or clamp happens afterwards. Every width helper in the
// codebase either miscounts or ignores the payload - ansi.PrintableRuneWidth
// counts the whole OSC as printable, lipgloss measures it as zero width - so the
// truncate in pager.glamourRender cannot clip it either. On an 80 column
// terminal any H1 wider than ~26 cells (i.e. almost all of them) is wrapped by
// the terminal itself: the heading needs six rows instead of three, the
// headingRows() compensation below is off, and everything after it drifts.
// Fix direction: derive w/n/d from the columns actually left on the line, or
// skip the scaling when the span would not fit.
func ApplyTextSizing(s string) string {
	if !strings.Contains(s, markerPrefix) {
		return s
	}
	// WARNING: if the markdown already contains the content of prefix and suffix, the text sizing will be applied wrong.
	// this problem is caused by glamour, we call glamour then apply the kitty osc66ST.
	// So without a pin, the glamout style control sequences will appear in the osc66ST payload. which is an unexpected behavior for the terminal.
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		i := strings.Index(s, markerPrefix)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		s = s[i:]

		level := markerLevel(s)
		if level < 0 {
			b.WriteString(markerPrefix)
			s = s[len(markerPrefix):]
			continue
		}
		s = s[len(markerOpen(level)):]
		if level < 1 || level > 3 {
			// NOTE: this fails open, and correctly: the span text is kept and the
			// close marker disappears on the next iteration (markerLevel sees
			// "...;0..." and lands in this same branch). Levels 0 and 4..9
			// therefore render as plain text. The one real gap is multi-digit
			// levels - markerLevel rejects them, so the open marker is re-emitted
			// verbatim instead of being consumed (see markerLevel).
			continue
		}

		end := strings.Index(s, markerClose)
		if end < 0 {
			b.WriteString(s)
			break
		}
		writeSizedSpan(&b, level, s[:end])
		s = s[end+len(markerClose):]
		// NOTE: these padding newlines are emitted before the escape sequence that
		// closes the span (glamour's SGR reset still sits in s), so the blank rows
		// inherit the heading's style. Harmless today only because headings with a
		// background are never marked; it makes the row accounting depend on where
		// the style happens to put its reset.
		b.WriteString(strings.Repeat("\n", headingRows(level)-1))
	}
	return b.String()
}

func writeSizedSpan(b *strings.Builder, level int, span string) {
	lines := strings.Split(span, "\n")
	for i, ln := range lines {
		if i > 0 {
			b.WriteString(strings.Repeat("\n", headingRows(level)))
		}
		writeSizedLine(b, level, ln, i > 0)
	}
}

type segment struct {
	escape bool
	value  string
}

func writeSizedLine(b *strings.Builder, level int, ln string, continuation bool) {
	// NOTE: leading whitespace is treated differently per line: on the first line
	// it stays inside the payload and is scaled (H1's one-space style prefix
	// becomes three columns), while on continuation lines it is trimmed out (see
	// below). A wrapped heading therefore has its first row indented and its
	// following rows flush left.
	var segs []segment
	for len(ln) > 0 {
		i := strings.IndexByte(ln, '\x1b')
		if i < 0 {
			segs = append(segs, segment{value: ln})
			break
		}
		if i > 0 {
			segs = append(segs, segment{value: ln[:i]})
			ln = ln[i:]
			continue
		}
		seq := scanEscape(ln)
		segs = append(segs, segment{escape: true, value: seq})
		ln = ln[len(seq):]
	}

	firstContent, lastContent := -1, -1
	for i, sg := range segs {
		if !sg.escape && strings.Trim(sg.value, " \t") != "" {
			if firstContent < 0 {
				firstContent = i
			}
			lastContent = i
		}
	}

	for i, sg := range segs {
		if sg.escape || firstContent < 0 {
			b.WriteString(sg.value)
			continue
		}
		text := sg.value
		switch {
		case i < firstContent && continuation:
			b.WriteString(text)
			continue
		case i > lastContent:
			b.WriteString(text)
			continue
		}
		if i == firstContent && continuation {
			trimmed := strings.TrimLeft(text, " \t")
			b.WriteString(text[:len(text)-len(trimmed)])
			text = trimmed
		}
		var trail string
		if i == lastContent {
			trimmed := strings.TrimRight(text, " \t")
			trail = text[len(trimmed):]
			text = trimmed
		}
		if text != "" {
			b.WriteString(sizedChunk(level, text))
		}
		b.WriteString(trail)
	}
}

func scanEscape(s string) string {
	if len(s) < 2 {
		return s
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return s[:i+1]
			}
		}
		return s
	case ']':
		for i := 2; i < len(s); i++ {
			if s[i] == '\a' {
				return s[:i+1]
			}
			if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
				return s[:i+2]
			}
		}
		return s
	default:
		i := 1
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) {
			i++
		}
		return s[:i]
	}
}

func sizedChunk(level int, text string) string {
	// NOTE: every C0/C1 character is dropped from the payload, tabs included. A
	// heading containing a tab silently loses it (the row becomes narrower than
	// the source text), and if a malformed escape ever reaches the content its ESC
	// is stripped here as well.
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, text)
	if text == "" {
		return ""
	}
	switch level {
	case 1:
		return osc66Chunks("s=3", text)
	case 2:
		return osc66Chunks("s=2", text)
	default:
		return osc66TightChunks(text)
	}
}

func osc66Chunks(meta, text string) string {
	var b strings.Builder
	var chunk strings.Builder
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		cluster := g.Str()
		if chunk.Len() > 0 && chunk.Len()+len(cluster) > osc66MaxPayload {
			writeOSC66(&b, meta, chunk.String())
			chunk.Reset()
		}
		chunk.WriteString(cluster)
	}
	if chunk.Len() > 0 {
		writeOSC66(&b, meta, chunk.String())
	}
	return b.String()
}

func osc66TightChunks(text string) string {
	// QUESTION: s=2:n=3:d=4 and w=(width*3+3)/4 are undocumented magic. If w is
	// meant to be the cell width of the *scaled* result, four columns of text
	// should declare w=6, not 3; if it is the natural width, it should be 4, not 3
	// - the formula rounds (width*1.5) up/down in a way that is not stated
	// anywhere, and for width 3 it maps 4.5 to 3. Also unclear is how an integer
	// s=2 combines with n/d; if kitty multiplies them the effective scale is 3,
	// not 1.5. A spec link plus one worked example in a comment would keep
	// somebody from "fixing" these numbers later.
	var b strings.Builder
	var chunk strings.Builder
	width := 0
	flush := func() {
		if chunk.Len() == 0 {
			return
		}
		w := (width*3 + 3) / 4
		writeOSC66(&b, fmt.Sprintf("s=2:n=3:d=4:w=%d", w), chunk.String())
		chunk.Reset()
		width = 0
	}
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		cluster := g.Str()
		cw := uniseg.StringWidth(cluster)
		if width > 0 && width+cw > 4 {
			flush()
		}
		chunk.WriteString(cluster)
		width += cw
	}
	flush()
	return b.String()
}

func writeOSC66(b *strings.Builder, meta, text string) {
	b.WriteString("\x1b]66;")
	b.WriteString(meta)
	b.WriteByte(';')
	b.WriteString(text)
	b.WriteString(osc66ST)
}
