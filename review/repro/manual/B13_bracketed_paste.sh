#!/bin/sh
# B13 - the raw painter replaces the renderer but does not mirror bracketed paste,
# so a pasted block is delivered as individual key presses.
#
# Expected: paste the two lines below into the filter -> the filter receives them
#           as text (one line, "doc q") and stays open.
# Actual:   without bracketed paste the terminal sends the block as raw bytes.
#           The newline arrives as ctrl+j, which ui/stash.go's handleFiltering
#           treats like Enter, so the filter is committed right there; the "q"
#           on the second line is then a key press in the file listing, and glow
#           quits.
#
# The automatic version of this is TestB13_MultiLinePasteArrivesAsKeyPresses
# (go test -tags repro ./ui -run B13). It decodes the pasted bytes with the same
# decoder bubbletea uses, then asserts that start() enables bracketed paste.
#
# This script needs the raw painter, so it forces GLOW_TEXT_SIZING=on. Without it
# (or on Windows/brush, where openRawTerminal always fails) glow uses the default
# renderer, which enables bracketed paste - the paste then works and nothing
# reproduces.
#
# Requirements: POSIX terminal (WSL / Linux / macOS) with /dev/tty.
# Cannot run on Windows brush.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
bin="$tmp/glow"
dir="$tmp/stash"

mkdir -p "$dir"
for i in 1 2 3 4 5; do
	printf '# document %s\n\nbody\n' "$i" >"$dir/doc$i.md"
done

echo "building glow ..."
(cd "$root" && go build -o "$bin" .)

cat <<'EOF'

------------------------------------------------------------------
B13: a multi-line paste becomes key presses (no bracketed paste)
------------------------------------------------------------------
1. glow starts in the file listing. Press "/" to open the filter.
2. Copy the two lines between the markers (including the line break) and paste
   them into the filter with your terminal's paste shortcut:

---8<---
doc
q
---8<---

Expected: the filter shows "doc q" and stays open.
Actual:   the filter closes on the pasted newline (ctrl+j) and the "q" that
          follows quits glow - you land back at your shell prompt.

If the filter shows "doc q" and glow keeps running, bracketed paste was on:
this run did not take the raw-painter path.
------------------------------------------------------------------

EOF

echo "forcing GLOW_TEXT_SIZING=on so the raw painter is used"
GLOW_TEXT_SIZING=on "$bin" "$dir"
