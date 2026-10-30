package ui

import (
	"io"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"
)

// rawPainter paints frames directly to the terminal. It is used when kitty
// text sizing support is detected: bubbletea's default renderer parses each
// frame into a cell buffer which discards OSC 66 sequences along with their
// payload text, so the program runs with [tea.WithoutRenderer] and this
// painter drives the terminal instead.
type rawPainter struct {
	w     io.Writer
	tty   *os.File
	state *term.State
	mouse bool
	done  chan struct{}

	mu      sync.Mutex
	gen     int
	painted int
}

func newRawPainter(w io.Writer, tty *os.File, state *term.State, mouse bool) *rawPainter {
	return &rawPainter{w: w, tty: tty, state: state, mouse: mouse, done: make(chan struct{})}
}

func (p *rawPainter) windowSize() tea.WindowSizeMsg {
	if p.tty != nil {
		if w, h, err := term.GetSize(int(p.tty.Fd())); err == nil && w > 0 && h > 0 {
			return tea.WindowSizeMsg{Width: w, Height: h}
		}
	}
	// NOTE: bubbletea only learns the size through p.ttyOutput, and that is set
	// by initTerminal(), which returns early when tea.WithoutRenderer is used.
	// So bubbletea itself sends WindowSizeMsg{0,0} once at startup and never
	// starts its SIGWINCH listener at all (handleResize() closes the channel
	// when ttyOutput is nil). 80x24 is therefore not a "default" but the last
	// resort for a tty that cannot even be sized; see paintModel.Update, which
	// substitutes it for bubbletea's 0x0 message.
	return tea.WindowSizeMsg{Width: 80, Height: 24} // QUESTION: why hard code a default size?
}

func (p *rawPainter) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.w, "\x1b[?1049h\x1b[?25l") // enter alternate screen + hide the cursor
	if p.mouse {
		_, _ = io.WriteString(p.w, "\x1b[?1002h\x1b[?1006h") // button-event mouse tracking + sgr extended mouse coordinate.
	}
	// BUG: cursedRenderer.start() (the renderer we replace) also restores the text
	// cursor, enables bracketed paste, focus events, window title, the kitty
	// keyboard protocol and modifyOtherKeys2. None of that happens here, and the
	// bracketed paste gap is user visible: the terminal then sends a pasted block
	// as raw bytes, so every newline arrives as its own key press. Decoded by the
	// same decoder bubbletea uses, "doc\nq" becomes d, o, c, ctrl+j, q; the file
	// filter treats ctrl+j like Enter (ui/stash.go), so it commits there and the
	// trailing "q" quits glow. With the default renderer (bracketed paste on) the
	// same paste arrives as one PasteMsg and bubbles' textinput collapses the
	// newline to a space, i.e. the paste works.
	// Minimal fix: emit ?2004h here and ?2004l in stop(). Whatever View()
	// advertises has to be duplicated in this function, by hand, forever.
	// repro: TestB13_MultiLinePasteArrivesAsKeyPresses and
	// review/repro/manual/B13_bracketed_paste.sh
}

func (p *rawPainter) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mouse {
		_, _ = io.WriteString(p.w, "\x1b[?1002l\x1b[?1006l")
	}
	// BUG: bubbletea does not wait for commands when it shuts down
	// (handleCommands just abandons the goroutines, tea.go:716), so a paint
	// command scheduled by the last Update can still be in flight here and
	// would then paint a whole frame on top of the restored primary screen.
	// paint() must re-check p.done under p.mu before writing.
	// repro: TestB02_NoPaintAfterStop
	//
	// NOTE: if the process dies in the middle of a frame, the terminal stays
	// inside the synchronized-update block (?2026h) opened by paint(). Terminals
	// differ in what that costs: kitty buffers strictly until ?2026l, while
	// alacritty and wezterm flush at the end of each batch, so the freeze is not
	// reproducible on them. That makes this a one-line defensive fix (?2026l
	// before leaving the alternate screen), not a bug with a repro.
	_, _ = io.WriteString(p.w, "\x1b[?25h\x1b[?1049l")
	if p.tty != nil {
		if p.state != nil {
			_ = term.Restore(int(p.tty.Fd()), p.state)
		}
		_ = p.tty.Close()
	}
	// NOTE: closing done is what wakes watchResize() up, so it stops listening
	// and unregisters the SIGWINCH handler. That is its only use.
	// QUESTION: what thing done? Maybe for gracefully shutdown
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// watchResize sends the initial window size to the program and reports
// terminal resizes. Without a renderer bubbletea never initializes the tty,
// so its own size reporting never happens.
//
// NOTE: the second half of that sentence is literal: bubbletea's
// handleResize() gives up entirely when ttyOutput is nil, and initTerminal()
// bails out before setting ttyOutput when tea.WithoutRenderer is used. So this
// goroutine is the only resize source the program has.
//
// NOTE: prog.Send() blocks until the event loop is running (msgs is unbuffered),
// so if Run() is never called this goroutine and its SIGWINCH handler live for
// the rest of the process; stop() is the only escape.
func (p *rawPainter) watchResize(prog *tea.Program) {
	ch := make(chan os.Signal, 1)
	winchNotify(ch)
	go func() {
		prog.Send(p.windowSize())
		for {
			select {
			case <-p.done:
				winchStop(ch)
				return
			case <-ch:
				prog.Send(p.windowSize())
			}
		}
	}()
}

func (p *rawPainter) paint(frame string) tea.Cmd {
	p.mu.Lock()
	p.gen++
	gen := p.gen
	p.mu.Unlock()

	// NOTE: the generation counter only keeps frames from being painted out of
	// order, it does not keep them from being painted far too often: every
	// message (spinner tick, fsnotify event, key press) schedules a full \x1b[2J
	// plus a complete repaint of the document, and bubbletea builds the same view
	// a second time for its nil renderer. There is no coalescing and no frame
	// budget, unlike the renderer this replaces.
	return func() tea.Msg {
		p.mu.Lock()
		defer p.mu.Unlock()
		if gen <= p.painted {
			return nil
		}
		// BUG: no p.done check. stop() may already have restored the primary
		// screen and closed the tty, and this frame would then be written on top
		// of it (or into a closed file, error ignored). Use a non-blocking
		// select on p.done here (note the tests build rawPainter directly, so
		// done may be nil).
		p.painted = gen

		var b strings.Builder
		b.WriteString("\x1b[?2026h\x1b[H\x1b[2J")
		// WARNING: the frame is written verbatim: rows and columns are not
		// clamped to the terminal size, and the height is not even known here.
		// Any line the terminal wraps, or any one row too many, scrolls the
		// whole frame up and silently shifts the pager's origin - the viewport
		// math (one content line == one row) stops matching the screen.
		b.WriteString(strings.ReplaceAll(frame, "\n", "\r\n"))
		b.WriteString("\x1b[?2026l")
		// NOTE: os.Stdout is shared with bubbletea (p.execute writes the
		// background-colour, clipboard and mode queries there) and with
		// termenv.Copy (OSC 52) in the pager, none of which take p.mu. A large
		// frame is written in several syscalls (os.File.Write loops), so another
		// writer's sequence can land between the chunks - inside the block, but
		// after a complete write of ours. Both writers emit whole sequences, so
		// the terminal copes; nothing enforces that, which is why this is hygiene
		// rather than an observable bug.
		_, _ = io.WriteString(p.w, b.String())
		return nil
	}
}

// paintModel wraps the root model, painting a frame after every processed
// message. Bubbletea's renderer never calls View when it is disabled, so the
// wrapper drives painting itself.
//
// NOTE: only the first sentence holds. View is still called (and thrown away)
// on every message; see the note in paintModel.View.
type paintModel struct {
	inner   tea.Model
	painter *rawPainter
}

func (m paintModel) Init() tea.Cmd {
	m.painter.start()
	// NOTE: this first frame is painted before any WindowSizeMsg has been
	// processed, i.e. with a 0x0 viewport. bubbletea queues a 0x0 resize message
	// at startup (see windowSize), so the frame is thrown away a moment later.
	return tea.Batch(m.inner.Init(), m.painter.paint(m.inner.View().Content))
}

func (m paintModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok && (ws.Width <= 0 || ws.Height <= 0) {
		// NOTE: bubbletea sends WindowSizeMsg{0,0} at startup because it has no
		// ttyOutput in this mode; substituting the real size here is what makes
		// the initial layout correct. A genuinely unknown size is silently
		// turned into 80x24 as well, which hides the failure.
		msg = m.painter.windowSize()
	}
	inner, cmd := m.inner.Update(msg)
	m.inner = inner
	// PERF: inner.View() is rendered here for every single message, even though
	// the paint command may be skipped as stale, and bubbletea renders the view
	// a second time for its own (nil) renderer right after this returns
	// (tea.go:880 -> render -> View). Two full viewport renders per message.
	return m, tea.Batch(cmd, m.painter.paint(inner.View().Content))
}

func (m paintModel) View() tea.View {
	// NOTE: this is not dead code. tea.render() calls model.View() for every
	// message and passes it to the renderer; even nilRenderer receives it (the
	// renderer is only skipped when the field is nil, and WithoutRenderer
	// installs a nilRenderer rather than leaving it nil). The value is then
	// dropped on the floor, which is exactly why the painted frame in Update is
	// the only one that matters. If the frame were painted here instead, it
	// would happen once per message instead of twice - at the price of a side
	// effect in View.
	// QUESTION: why just return a dummy one? this can be misleading.
	return m.inner.View()
}
