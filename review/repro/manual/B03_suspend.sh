#!/bin/sh
# B03 - ctrl+z leaves the terminal in raw mode, on the alternate screen, with the
# cursor hidden.
#
# What happens: ui/ui.go returns tea.Suspend on ctrl+z. bubbletea handles
# SuspendMsg itself (tea.go:772 -> p.suspend()) and releases the terminal through
# the renderer; with tea.WithoutRenderer that renderer is nilRenderer, whose
# reset() is a no-op. The model is only told about SuspendMsg *after* SIGCONT, so
# paintModel cannot release anything in time.
#
# Requirements: a real terminal, POSIX shell, /dev/tty. The script forces
# GLOW_TEXT_SIZING=on so that glow takes the raw-painter path even on a terminal
# that does not advertise kitty text sizing.
#
# Cannot run on Windows brush: there is no /dev/tty raw terminal there
# (ui/rawpaint_windows.go returns an error), so ui.NewProgram always falls back
# to the default renderer, bubbletea restores the terminal itself, and the bug
# does not exist. Use WSL, Linux or macOS.
#
# This cannot be automated: the state to observe exists only while the process is
# suspended, i.e. while the shell that would run the check is what just got
# suspended with it. The steps below are the reproduction.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
bin="$tmp/glow"

echo "building glow ..."
(cd "$root" && go build -o "$bin" .)

cat <<'EOF'

------------------------------------------------------------------
B03: ctrl+z suspends glow with the terminal still in raw mode
------------------------------------------------------------------
1. glow starts now. When the document is on screen, press ctrl+z.
2. You are back at your shell prompt, but the terminal is still raw, so you have
   to type blind - nothing is echoed and the cursor is invisible. Type this and
   press Enter:

     stty -a | grep -o -- -icanon

   -> -icanon    (raw mode: the suspend did not restore the terminal)
3. Type (blind, then Enter):  stty sane
   Now typing is visible again - but note that everything you printed since
   ctrl+z went to the alternate screen.
4. Resume glow with "fg", then press q to quit.
5. Run:  stty -a | grep -o -- -icanon
   -> icanon     (only glow's exit restored it)
   and notice that the step-3 output is gone: it was written on the alternate
   screen, which glow just discarded.

Expected: the shell gets a cooked terminal with a visible cursor, on the primary
screen. Actual: raw mode, cursor hidden, and everything the shell prints lands on
the alternate screen (your previous shell output is gone).

Correct fix: intercept ctrl+z before returning tea.Suspend, call painter.stop(),
and painter.start() again on tea.ResumeMsg.
------------------------------------------------------------------

EOF

echo "forcing GLOW_TEXT_SIZING=on so the raw painter (and ctrl+z) is used"
printf '# suspend me\n\npress ctrl+z while this is on screen\n' | GLOW_TEXT_SIZING=on "$bin" -t
