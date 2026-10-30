package ui

import (
	"errors"
	"os"

	"golang.org/x/term"
)

// NOTE: this makes the raw-paint path unreachable on Windows, so text sizing can
// never be used there. Worse, GLOW_TEXT_SIZING=on still enables the markers and
// the OSC 66 rewriting while ui.NewProgram falls back to the normal renderer,
// which drops the OSC together with its payload - i.e. the heading text itself.
func openRawTerminal() (*os.File, *term.State, error) {
	return nil, nil, errors.New("raw terminal is not supported on windows")
}

// NOTE: no-op on Windows, so watchResize() never gets a resize signal; the raw
// painter is unreachable here anyway (see openRawTerminal).
func winchNotify(chan os.Signal) {}

func winchStop(chan os.Signal) {}
