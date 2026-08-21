package output

import (
	"fmt"
	"time"
)

// Text helpers shared by the renderings. All durations format with the
// largest sensible unit; all "ago"/"in" phrasing goes through the injected
// clock so goldens are deterministic.

func formatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func (p *Printer) relativeTime(t time.Time) string {
	d := p.now().Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return formatDuration(d) + " ago"
}

// expiredLabel is shared by every rendering of a lapsed expiry.
const expiredLabel = "expired"

func (p *Printer) formatExpiry(t time.Time) string {
	remaining := t.Sub(p.now())
	if remaining <= 0 {
		return expiredLabel
	}
	return fmt.Sprintf("%s (in %s)", t.UTC().Format(time.RFC3339), formatDuration(remaining))
}

// ellipsis is the truncation marker, degraded to "..." for non-UTF-8
// locales.
func (p *Printer) ellipsis() string {
	if p.ascii {
		return "..."
	}
	return "…"
}

// middleEllipsis shortens s to at most max characters by cutting the middle,
// keeping both the distinctive head and the checksum-bearing tail visible.
// Inputs at or under max come back untouched.
func (p *Printer) middleEllipsis(s string, maxLen int) string {
	marker := []rune(p.ellipsis())
	runes := []rune(s)
	if len(runes) <= maxLen || maxLen <= len(marker)+2 {
		return s
	}
	keep := maxLen - len(marker)
	head := keep / 2
	tail := keep - head
	return string(runes[:head]) + string(marker) + string(runes[len(runes)-tail:])
}

// truncateEnd shortens s to at most max characters, marking the cut.
func (p *Printer) truncateEnd(s string, maxLen int) string {
	marker := []rune(p.ellipsis())
	runes := []rune(s)
	if len(runes) <= maxLen || maxLen <= len(marker) {
		return s
	}
	return string(runes[:maxLen-len(marker)]) + string(marker)
}
