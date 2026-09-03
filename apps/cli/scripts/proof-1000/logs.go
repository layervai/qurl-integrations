package main

import (
	"bufio"
	"os"
	"path/filepath"
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
// explanation for every non-serving share the report lists.
func collectDaemonLogs(logDir string, since, until time.Time, needles []string, red *redactor, limit int) []string {
	var out []string
	for _, name := range daemonLogFiles {
		out = append(out, scanDaemonLog(filepath.Join(logDir, name), since, until, needles, red, limit-len(out))...)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func scanDaemonLog(path string, since, until time.Time, needles []string, red *redactor, limit int) []string {
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
		at, err := time.ParseInLocation(daemonLogTimeLayout, line[:len(daemonLogTimeLayout)], time.Local)
		if err != nil || at.Before(since) || at.After(until) {
			continue
		}
		if !mentionsAny(line, needles) && !mentionsThrottle(line) {
			continue
		}
		out = append(out, red.apply(line))
		if len(out) >= limit {
			break
		}
	}
	return out
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
		stamp, err := time.ParseInLocation(daemonLogTimeLayout, line[:len(daemonLogTimeLayout)], time.Local)
		if err != nil || stamp.Before(at.Add(-window)) || stamp.After(at.Add(window)) {
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
