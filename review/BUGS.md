# glow v3 — terminal rendering defects in the kitty text-sizing path

Reviewed tree: `charmbracelet/glow` @ `8b0600f` (`main`), Go 1.26,
`bubbletea v2.0.8`, `bubbles v2.1.1`, `glamour v2.0.1`, `lipgloss v2.0.6`.

Scope: the raw-painter path added for kitty's text sizing protocol (OSC 66) —
`ui/rawpaint.go`, the `paintModel` wrapper, `ui/ui.go:NewProgram`, the render
side of `ui/pager.go`, and `utils/textsize.go`.

Every item below has an executable reproduction. They are Go tests that assert
the **correct** behaviour, so they are red while the bug exists and turn green
when it is fixed:

```sh
# 13 failing tests, no terminal needed (works on Windows too)
go test -tags repro ./review/repro ./ui -run 'TestB' -v

# one at a time
go test -tags repro ./ui -run TestB07 -v
```

Two findings are only observable in a real terminal and ship as scripts under
`review/repro/manual/` (POSIX / WSL / Linux / macOS; they force
`GLOW_TEXT_SIZING=on` so that the raw painter is used even on terminals without
OSC 66 support):

```sh
sh review/repro/manual/B03_suspend.sh          # ctrl+z
sh review/repro/manual/B13_bracketed_paste.sh  # multi-line paste
```

`review/repro/README.md` is the working index (Chinese); this file is the report.

## Summary

| # | Class | Sev. | Area | Symptom |
|---|-------|------|------|---------|
| B01 | user-visible | high | fallback path | every heading loses its text when text sizing is on but the raw painter is not |
| B02 | user-visible | medium | painter lifecycle | a frame queued before quit can be painted on the restored primary screen |
| B03 | user-visible | high | painter lifecycle | `ctrl+z` leaves the tty in raw mode, on the alternate screen, cursor hidden |
| B04 | user-visible | high | heading scaling | a scaled heading is wider than the terminal; the terminal wraps it and the layout drifts |
| B05 | user-visible | medium | painter / framing | the painter never clamps a frame to the window: one extra row or a long line scrolls the screen |
| B06 | user-visible | medium | pager | `fsnotify.NewWatcher` failure is only logged; `watchFile` then panics on a nil watcher |
| B07 | user-visible | medium | pager | the first render runs at viewport width 0, i.e. with word wrap disabled |
| B08 | user-visible | medium | pager | with line numbers, `MaxWidth` truncates real text (2 columns for a paragraph) |
| B09 | user-visible | medium | root model | a failed reload (`errMsg`) is dropped while a document is shown |
| B10 | latent | low | text sizing | `markerLevel` rejects multi-digit levels, so such a marker is forwarded as a raw escape |
| B11 | perf | medium | painter | every message paints a whole frame (`\x1b[2J` + full repaint); no coalescing |
| B12 | perf | low | painter | the view is rendered twice per message; the second result is discarded |
| B13 | user-visible | high | painter / input | bracketed paste is not enabled: a pasted block arrives as key presses |
| B15 | latent | low | painter | `watchResize` blocks in `Program.Send` and cannot be stopped if `Run` is never called |

`review/repro/manual/` and the tests cover the two hard-to-automate ones; the rest
are pure Go.

---

## User-visible

### B01 — the fallback renderer eats every heading

**Where:** `ui/ui.go:28` (`NewProgram`), `utils/textsize.go:32`
(`TextSizingEnabled`), `utils/utils.go` (`GlamourStyle`).

**Mechanism.** When text sizing is enabled, `NewProgram` tries
`openRawTerminal()`. If that fails it silently falls back to
`tea.NewProgram(m)`, the default renderer. But `GlamourStyle` has already put the
heading markers into the style config and `pager.glamourRender` has already
rewritten them into OSC 66 — and the default renderer parses every frame into a
cell buffer, where an OSC 66 is dropped **together with its payload**, i.e. the
heading text itself. `TextSizingEnabled` caches its answer in
`sync.OnceValue`, so the fallback cannot turn it back off.

This is not a corner case: `openRawTerminal` is a stub that always fails on
Windows (`ui/rawpaint_windows.go`), so `GLOW_TEXT_SIZING=on` on Windows deletes
all headings.

**Repro:** `go test -tags repro ./review/repro -run B01 -v`. The test renders a
document, feeds the frame through the same cell buffer the renderer uses
(`charmbracelet/ultraviolet`), and asserts the heading is still on screen.

Manual: on Windows, `set GLOW_TEXT_SIZING=on && glow -t README.md` — the body is
fine, H1/H2/H3 text is missing.

**Expected / actual:** heading text visible / `"AB"` for `A<OSC66>HIDDEN<ST>B`.

**Fix:** decide once, before anything renders, whether the raw painter will be
used, and only enable text sizing in that case. The cached `sync.OnceValue` is
the obstacle — the decision belongs in `ui.Config`.

### B02 — a queued frame is painted after the screen is restored

**Where:** `ui/rawpaint.go:92` (`paint`) and `:52` (`stop`).

**Mechanism.** `q` makes `Update` batch a `paint` command, and bubbletea does not
wait for commands on shutdown (`handleCommands` abandons the goroutines,
`tea.go:716`). The cleanup returned by `NewProgram` is `painter.stop`; it may run
before the queued command, which then writes a whole frame (`\x1b[2J` included)
after `\x1b[?1049l`, on the user's primary screen.

**Repro:** `go test -tags repro ./ui -run B02 -v` — start, queue a paint, stop,
then run the queued command; asserts that nothing is written after `stop()`.

**Expected / actual:** zero bytes after `stop()` / a full 33-byte frame.

**Fix:** check `p.done` (non-blocking) under `p.mu` in `paint` before writing.

### B03 — `ctrl+z` leaves the terminal unusable

**Where:** `ui/ui.go:249` (`ctrl+z` → `tea.Suspend`), `ui/rawpaint.go`.

**Mechanism.** bubbletea handles `SuspendMsg` itself (`p.suspend()`): it releases
the terminal through the renderer and re-acquires it after `SIGCONT`. With
`tea.WithoutRenderer` the renderer is `nilRenderer`, whose `reset()` is a no-op,
so nothing releases the terminal that the raw painter took over: the shell
inherits a tty in raw mode, on the alternate screen, with the cursor hidden.
The model only receives `SuspendMsg` *after* resuming, which is far too late.

**Repro:** `sh review/repro/manual/B03_suspend.sh` (confirmed on WSL; the script
prints the exact `stty` checks). Windows cannot reproduce it: the raw painter is
unreachable there.

**Expected / actual:** cooked terminal, visible cursor, primary screen / raw
mode + alternate screen + hidden cursor; everything the shell prints is lost
when glow later exits.

**Fix:** intercept `ctrl+z` before returning `tea.Suspend`, call
`painter.stop()`, and `painter.start()` again on `tea.ResumeMsg`.

### B04 — a scaled heading does not fit the terminal

**Where:** `utils/textsize.go:174` (`ApplyTextSizing`), `:162` (`headingRows`),
`ui/pager.go:354` (`glamourRender`).

**Mechanism.** glamour wraps heading text at the body word-wrap width
(`ansi/heading.go`: `lipgloss.Wrap(..., bs.Width(ctx))`); the OSC 66 payload then
multiplies the advance width by 2 (H2) or 3 (H1). Nothing re-wraps or clamps the
result, and no width helper in the codebase can see it: `ansi.PrintableRuneWidth`
counts the payload as printable, lipgloss measures it as zero width. The terminal
wraps the heading itself, so the heading occupies more rows than the
`headingRows()` compensation reserved, and everything below it drifts.

On an 80-column terminal any H1 wider than ~26 cells (almost all of them) hits
this; the numbers for a 40-column terminal and `# Getting Started With Glow`:

- widest physical line: **81** columns
- rows a terminal needs: **8**, rows the viewport counts: **6** (drift 2)

**Repro:** `go test -tags repro ./review/repro -run B04 -v`; the test models a
compliant terminal (`review/repro/width.go`, `DisplayWidth`) and asserts both the
width and the row count.

Visual: in kitty, `GLOW_TEXT_SIZING=on glow -t doc-with-long-h1.md`.

**Fix:** derive `w`/`n`/`d` from the columns actually left on the line so the
scaled span fits, or skip scaling for headings that would not fit. The body wrap
width cannot simply be divided by the scale, because the same `wordWrap` value is
used for both.

### B05 — the painter writes the frame verbatim

**Where:** `ui/rawpaint.go:92` (`paint`).

**Mechanism.** `paint` has no idea how big the terminal is — `windowSize()` is
only used to feed `WindowSizeMsg` — so it neither limits the number of rows nor
clamps the width. The renderer that used to absorb an over-long frame is gone.
Anything that produces a frame taller or wider than the window (B04, B07, a
terminal resize race, a wrapped line) is therefore written as-is: the terminal
scrolls, the pager's origin moves, and "one content line == one row" stops being
true.

**Repro:** `go test -tags repro ./ui -run B05 -v` — paints 30 rows for a 24-row
terminal and a 200-column line for an 80-column one.

**Expected / actual:** rows and columns clamped to `windowSize()` / 30 rows, 200
columns written verbatim.

**Fix:** pass the window size to the painter (or use `windowSize()` inside
`paint`) and clamp/pad each frame.

### B06 — a failed fsnotify watcher panics

**Where:** `ui/pager.go:416` (`initWatcher`), `:424` (`watchFile`).

**Mechanism.** `initWatcher` logs the error and leaves `m.watcher` nil, but
`contentRenderedMsg` unconditionally queues `m.watchFile`, which calls
`m.watcher.Add(dir)`. `fsnotify.NewWatcher` can fail for ordinary reasons (fd
exhaustion, inotify limits), and the result is a nil-pointer panic instead of a
degraded pager.

**Repro:** `go test -tags repro ./ui -run B06 -v`.

**Expected / actual:** `watchFile` is a no-op / `runtime error: invalid memory
address or nil pointer dereference`.

**Fix:** return early from `watchFile`/`unwatchFile` when `m.watcher == nil`.

### B07 — the first render ignores the word wrap

**Where:** `ui/pager.go:354` (`glamourRender`), `:183` (`model.Init`).

**Mechanism.** `model.Init` queues `renderWithGlamour`, and that render can run
before the first `WindowSizeMsg` is applied: `m.viewport.Width()` is still 0, and
`width = min(GlamourMaxWidth, viewport.Width())` becomes 0.
`glamour.WithWordWrap(0)` means "do not wrap", so the whole document is laid out
on one line and painted once. The resize-triggered render then runs, but the two
renders go through the same command queue with no ordering guarantee — the
unwrapped result can arrive last and stick until the next resize.

**Repro:** `go test -tags repro ./ui -run B07 -v` — a pager with an unsized
viewport and `GlamourMaxWidth: 40` produces a **151**-column line.

**Fix:** do not render until a size is known, or fall back to
`cfg.GlamourMaxWidth` when the viewport is unsized.

### B08 — line numbers truncate real text

**Where:** `ui/pager.go:354` (`glamourRender`, the `trunc` variable), `:395`.

**Mechanism.** glamour wraps at `width` (= min(GlamourMaxWidth, viewport width))
and reserves a 2-column left margin, so a full paragraph line is
`viewportWidth - 2` columns wide. `trunc` is
`MaxWidth(viewportWidth - lineNumberWidth)`, and `MaxWidth` **truncates** rather
than re-wrapping, so the last 2 columns of a full-width line are silently cut
(up to `lineNumberWidth - leftMargin` for blocks with a wider margin).

**Repro:** `go test -tags repro ./ui -run B08 -v` — a 36-character paragraph at
`viewport 40`: **36** characters without line numbers, **34** with them.

**Fix:** pass `width - lineNumberWidth` to glamour instead of truncating after.

### B09 — a reload failure is silently dropped

**Where:** `ui/ui.go:196` (`model.Update`), `ui/pager.go:130` (`pagerModel.update`).

**Mechanism.** `model.Update` has no `errMsg` case; it only forwards messages to
the active child. `stashModel.update` handles `errMsg`, `pagerModel.update` does
not. While a document is shown — e.g. after pressing `r` (reload) or when
fsnotify reports a deleted file — the failure vanishes and the stale frame keeps
looking current.

**Repro:** `go test -tags repro ./ui -run B09 -v` — real end-to-end: open a file,
delete it, run the reload command, feed the resulting `errMsg` to the model;
asserts that the view tells the user something.

**Expected / actual:** an error is shown / the old content is still there and no
error text appears anywhere.

**Fix:** handle `errMsg` at the top level (e.g. reuse `errorView`, as the stash
does), or at least surface a status message.

## Latent / robustness

### B10 — multi-digit marker levels are forwarded as escapes

**Where:** `utils/textsize.go:123` (`markerLevel`), `:196` (`ApplyTextSizing`).

**Mechanism.** `markerOpen(level)` writes the level as a decimal number, but
`markerLevel` accepts exactly one digit. For `level >= 10` the marker is not
recognised: `ApplyTextSizing` re-emits `markerPrefix` and treats the rest as text,
so a raw OSC 66 escape reaches the terminal. Levels 0 and 4..9 are handled
correctly (the span degrades to plain text), which is asserted as a control in the
test.

Unreachable today (`AddHeadingSizeMarkers` only marks 1..3), but it is a hole in
`ApplyTextSizing`'s contract — and that function is exported.

**Repro:** `go test -tags repro ./review/repro -run B10 -v`.

**Fix:** parse the level with `strconv.Atoi` up to the `\x1b\\`.

### B15 — `watchResize` cannot be shut down if `Run` is never called

**Where:** `ui/rawpaint.go:75` (`watchResize`), `:52` (`stop`).

**Mechanism.** `prog.Send` blocks on an unbuffered channel until the event loop
runs. `stop()` closes `p.done`, but the goroutine is parked in `Send`, not in its
`select`, so it never observes it: the goroutine and the registered `SIGWINCH`
handler leak. The current binary always calls `Run`, so this is not user-visible;
it matters for anyone else using `ui.NewProgram`.

**Repro:** `go test -tags repro ./ui -run B15 -v` — the test looks for
`rawPainter).watchResize` in `runtime.Stack` after `stop()`.

**Fix:** give the watcher its own cancellable context, or send from a
`select` that also watches `p.done`.

## Performance

### B11 — every message paints a whole frame

**Where:** `ui/rawpaint.go:128` (`paintModel.Update`), `:92` (`paint`).

`paintModel.Update` schedules a complete `\x1b[?2026h\x1b[H\x1b[2J … ?2026l`
frame for every processed message — spinner ticks, fsnotify events, key presses,
even messages that do not change the view. The generation counter only prevents
out-of-order frames, not redundant ones; the renderer being replaced coalesced
frames at ~60 fps and skipped intermediate states.

**Repro:** `go test -tags repro ./ui -run B11 -v` — 50 identical messages produce
**50** full-screen clears.

**Fix:** remember the last painted frame and skip identical ones, and/or coalesce
paints to a frame budget.

### B12 — the view is rendered twice per message

**Where:** `ui/rawpaint.go:128` (`paintModel.Update`), `:137` (`paintModel.View`).

`paintModel.Update` calls `inner.View()` to feed the paint command, and bubbletea
calls `model.View()` again for its renderer (`tea.go:872` Update, `tea.go:880`
render); with `nilRenderer` that second result is discarded.

**Repro:** `go test -tags repro ./ui -run B12 -v` — one message renders the view
twice.

**Fix:** paint from `View()` (which bubbletea calls exactly once per message)
instead of `Update`.

---

## Investigated and not reported as bugs

Three suspicions were dropped after measurement — recorded here so they are not
rediscovered:

- **Shared `os.Stdout` without a common lock.** The painter serialises its own
  writes with `p.mu`, while bubbletea (`p.execute`) and `termenv.Copy` (OSC 52)
  write to the same fd without it. A large frame is written in several syscalls
  (`os.File.Write` loops), so a foreign sequence can land *between* chunks — but
  each writer emits complete sequences, and an OSC inside the synchronized-update
  block is processed normally, so there is no observable corruption. Hygiene
  note, not a defect.
- **`?2026h` left open if the process dies mid-frame.** `paint` writes the whole
  frame as one string, but the terminal can receive half of it if the process is
  killed between the two writes. Terminals differ: kitty buffers strictly until
  `?2026l`, while alacritty and wezterm flush at the end of each batch (verified
  on both: no freeze), so this cannot be reproduced reliably. One-line defensive
  fix (`emit ?2026l` in `stop`), not a bug report.
- **`probeTextSizing` writes cursor-position queries to `/dev/tty`.** Confirmed
  that a plain `glow README.md` does emit `ESC[6n` and an OSC 66 (captured on a
  pty), i.e. the probe also runs outside the TUI. The suspected consequence — a
  reply arriving after the 800 ms timeout and being read as keystrokes — needs a
  terminal slower than the timeout and has no visible symptom, so it is
  documented rather than claimed.

## Notes for whoever fixes these

Order by cost/benefit (all are small):

1. B13 — emit `?2004h` in `start()` and `?2004l` in `stop()` (two writes).
2. B02 — check `p.done` in `paint` (a non-blocking select).
3. B09 — one `errMsg` case in `model.Update`.
4. B07 — fall back to `cfg.GlamourMaxWidth` when the viewport is unsized.
5. B06 — nil checks in `watchFile`/`unwatchFile`.
6. B08 — pass the reduced width to glamour.
7. B04/B05 — the two that need design: a width-aware heading scale and a size-aware
   painter (clamp/pad).

While writing the reproductions, two earlier assumptions had to be corrected and
are worth recording: glamour **does** hard-wrap at the word-wrap width (so the
"unclipped lines" symptom is limited to when the wrap width is 0, i.e. B07, and to
scaled headings, i.e. B04), and pasting multi-line text into the filter works
fine **with** the default renderer because bracketed paste is enabled there —
bubbles' textinput then collapses the newline to a space. Without bracketed paste
the newline becomes `ctrl+j`, which the filter treats like Enter (B13).
