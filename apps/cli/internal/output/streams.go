// Package output owns every byte the qURL CLI writes.
//
// Stream discipline is the contract: stdout carries data and only data — the
// minted link, the CRID, JSON documents — while everything meant for a human
// (decorations, warnings, notes, prompts, errors) goes to stderr. A script
// can always pipe stdout without scraping prose out of it.
//
// TTY detection is injected, never sniffed at use sites, so tests exercise
// the real TTY/non-TTY renderings byte-for-byte. JSON projections are
// repo-owned structs — SDK types never marshal straight to the user, so an
// SDK field rename cannot silently change the CLI's output contract.
package output

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Streams carries the process I/O and its TTY-ness.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer

	InIsTTY  bool
	OutIsTTY bool
	ErrIsTTY bool
}

// Detect returns the real process streams with TTY detection applied.
func Detect() *Streams {
	return &Streams{
		In:       os.Stdin,
		Out:      os.Stdout,
		Err:      os.Stderr,
		InIsTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		OutIsTTY: term.IsTerminal(int(os.Stdout.Fd())),
		ErrIsTTY: term.IsTerminal(int(os.Stderr.Fd())),
	}
}

// Color modes accepted by --color.
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// ResolveColor decides whether a stream gets ANSI color. --color=always and
// --color=never are absolute; auto means "TTY and NO_COLOR is not set"
// (https://no-color.org/ — presence disables, even when empty).
func ResolveColor(mode string, lookup func(string) (string, bool), tty bool) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default:
		if !tty {
			return false
		}
		if lookup != nil {
			if _, set := lookup("NO_COLOR"); set {
				return false
			}
		}
		return true
	}
}

// ResolveASCII reports whether output should degrade to plain ASCII. An
// explicitly configured non-UTF-8 locale (LC_ALL, LC_CTYPE, LANG — first one
// set wins, per POSIX precedence) degrades; everything else assumes UTF-8,
// which is what modern terminals speak.
func ResolveASCII(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		return false
	}
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v, set := lookup(key)
		if !set || v == "" {
			continue
		}
		folded := strings.ToLower(v)
		return !strings.Contains(folded, "utf-8") && !strings.Contains(folded, "utf8")
	}
	return false
}
