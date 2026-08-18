package output

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Format selects the stdout projection.
type Format string

// The two output formats.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ValidFormat reports whether f is an accepted --output value.
func ValidFormat(f Format) bool {
	return f == FormatText || f == FormatJSON
}

// Printer renders command results under the stream discipline. Build one per
// invocation with New; it is not safe for concurrent use.
type Printer struct {
	out    io.Writer
	err    io.Writer
	outTTY bool
	color  bool
	ascii  bool
	quiet  bool
	format Format
	now    func() time.Time
}

// New builds a Printer. now is injected so relative times are testable;
// nil means time.Now.
func New(s *Streams, format Format, quiet, color, ascii bool, now func() time.Time) *Printer {
	if now == nil {
		now = time.Now
	}
	return &Printer{
		out:    s.Out,
		err:    s.Err,
		outTTY: s.OutIsTTY,
		color:  color,
		ascii:  ascii,
		quiet:  quiet,
		format: format,
		now:    now,
	}
}

// Warnf writes one redacted warning line to stderr. Stderr writes are
// best-effort by design: a broken stderr must not fail a command whose data
// already reached stdout.
func (p *Printer) Warnf(format string, args ...any) {
	line := qurlapi.Redact(fmt.Sprintf(format, args...))
	_, _ = fmt.Fprintf(p.err, "%s %s\n", p.yellow("Warning:"), line)
}

// Notef writes one redacted informational line to stderr, best-effort like
// Warnf.
func (p *Printer) Notef(format string, args ...any) {
	line := qurlapi.Redact(fmt.Sprintf(format, args...))
	_, _ = fmt.Fprintf(p.err, "%s\n", p.dim(line))
}

// writeJSON emits v to stdout as indented JSON with a trailing newline.
func (p *Printer) writeJSON(v any) error {
	enc := json.NewEncoder(p.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ANSI styling. Color is per-printer state, never a global, so parallel
// tests and injected streams cannot race on it.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

func (p *Printer) style(code, s string) string {
	if !p.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (p *Printer) bold(s string) string   { return p.style(ansiBold, s) }
func (p *Printer) dim(s string) string    { return p.style(ansiDim, s) }
func (p *Printer) green(s string) string  { return p.style(ansiGreen, s) }
func (p *Printer) yellow(s string) string { return p.style(ansiYellow, s) }

// errWriter stops writing after the first error, so multi-line renderings
// need one error check instead of one per line.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

type flusher interface{ Flush() error }

// flush returns the first write error, or flushes fl when set.
func (ew *errWriter) flush(fl flusher) error {
	if ew.err != nil {
		return ew.err
	}
	if fl != nil {
		return fl.Flush()
	}
	return nil
}
