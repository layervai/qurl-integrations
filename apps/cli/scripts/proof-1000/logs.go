package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	daemonLogTimeLayout = "2006/01/02 15:04:05"
	daemonLogMaxLine    = 1 << 16
)

var daemonLogFiles = []string{"share-daemon.log", "share-daemon.err.log"}

// collectDaemonLogs returns the daemon's own log lines from the run window
// that mention any needle (proof CRIDs and resource ids) or a generic
// throttling/denial word, redacted and capped. Those lines are the daemon's
// diagnostic context for non-serving shares. Missing excerpts are not proof
// of zero failures; ambiguous legacy timestamps cannot be attributed to a run.
func collectDaemonLogs(logDir string, since, until time.Time, needles []string, red *redactor, limit int) []string {
	var out []string
	set := newNeedleSet(needles)
	for _, name := range daemonLogFiles {
		out = append(out, scanDaemonLog(filepath.Join(logDir, name), since, until, set, red, limit-len(out))...)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func scanDaemonLog(path string, since, until time.Time, needles *needleSet, red *redactor, limit int) []string {
	if limit <= 0 {
		return nil
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, daemonLogMaxLine), daemonLogMaxLine)
	var out []string
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < len(daemonLogTimeLayout) {
			continue
		}
		at, ok := parseDaemonLogTime(line, time.Local)
		if !ok || at.Before(since) || at.After(until) {
			continue
		}
		if !needles.matches(line) && !mentionsThrottle(line) {
			continue
		}
		out = append(out, red.apply(line))
		if len(out) >= limit {
			break
		}
	}
	return out
}

// parseDaemonLogTime accepts current slog timestamps and unambiguous legacy logs.
// A zone-less repeated hour cannot prove which instant a legacy entry belongs to.
func parseDaemonLogTime(line string, location *time.Location) (time.Time, bool) {
	if raw, ok := strings.CutPrefix(line, "time="); ok {
		stamp, _, _ := strings.Cut(raw, " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		return at, err == nil
	}
	if len(line) < len(daemonLogTimeLayout) {
		return time.Time{}, false
	}
	stamp := line[:len(daemonLogTimeLayout)]
	at, err := time.ParseInLocation(daemonLogTimeLayout, stamp, location)
	if err != nil || at.In(location).Format(daemonLogTimeLayout) != stamp {
		return time.Time{}, false
	}
	// Check both adjacent zone offsets, including non-hour DST transitions.
	start, end := at.ZoneBounds()
	_, offset := at.Zone()
	for i, boundary := range []time.Time{start, end} {
		if boundary.IsZero() {
			continue
		}
		if i == 0 {
			boundary = boundary.Add(-time.Nanosecond)
		}
		_, otherOffset := boundary.Zone()
		if otherOffset == offset {
			continue
		}
		alternate := at.Add(time.Duration(offset-otherOffset) * time.Second)
		if alternate.In(location).Format(daemonLogTimeLayout) == stamp {
			return time.Time{}, false
		}
	}
	return at, true
}

// needleSet indexes identifiers so a daemon log line is matched by looking
// up its crid=/resource_id= tokens instead of scanning thousands of needles.
type needleSet struct {
	ids   map[string]bool
	plain []string
}

var logIdentifierToken = regexp.MustCompile(`(?:crid|resource_id)=([A-Za-z0-9_-]+)`)

func newNeedleSet(needles []string) *needleSet {
	set := &needleSet{ids: make(map[string]bool, len(needles))}
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		set.ids[needle] = true
		if len(set.plain) < smallNeedleScan {
			set.plain = append(set.plain, needle)
		}
	}
	return set
}

// smallNeedleScan keeps a plain substring scan for small needle sets, where
// identifiers may appear without a key= prefix.
const smallNeedleScan = 64

func (n *needleSet) matches(line string) bool {
	if n == nil || len(n.ids) == 0 {
		return false
	}
	for _, m := range logIdentifierToken.FindAllStringSubmatch(line, -1) {
		if n.ids[m[1]] {
			return true
		}
	}
	if len(n.ids) <= smallNeedleScan {
		return mentionsAny(line, n.plain)
	}
	return false
}

func mentionsAny(line string, needles []string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

var throttleWords = []string{"429", "rate limit", "too many", "denied", "overload", "Retry-After"}

func mentionsThrottle(line string) bool {
	lower := strings.ToLower(line)
	for _, word := range throttleWords {
		if strings.Contains(lower, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

// contextLines filters already-collected log lines to one share within
// ±window of a moment, so a failure carries the connector's own events
// (session retries, retirements, rotations) from that moment only.
func contextLines(lines, needles []string, at time.Time, window time.Duration, limit int) []string {
	var out []string
	for _, line := range lines {
		if !mentionsAny(line, needles) || len(line) < len(daemonLogTimeLayout) {
			continue
		}
		stamp, ok := parseDaemonLogTime(line, time.Local)
		if !ok || stamp.Before(at.Add(-window)) || stamp.After(at.Add(window)) {
			continue
		}
		out = append(out, line)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// linesMentioning filters already-collected log lines to one share.
func linesMentioning(lines, needles []string, limit int) []string {
	var out []string
	for _, line := range lines {
		if mentionsAny(line, needles) {
			out = append(out, line)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
