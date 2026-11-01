package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestDaemonLogTimeRejectsAmbiguousLegacyClocks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		zone, stamp string
		valid       bool
	}{
		{"America/Denver", "2026/11/01 01:30:00", false},
		{"America/Denver", "2026/03/08 02:30:00", false},
		{"America/Denver", "2026/11/01 03:30:00", true},
		{"Australia/Lord_Howe", "2026/04/05 01:45:00", false},
		{"UTC", "2026/11/01 01:30:00", true},
	} {
		location, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := parseDaemonLogTime(tc.stamp+" WARN fixture", location); ok != tc.valid {
			t.Errorf("%s %s valid=%t want=%t", tc.zone, tc.stamp, ok, tc.valid)
		}
	}
}

func TestDaemonLogOffsetKeepsBothOccurrencesDistinct(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	first, err := time.Parse(time.RFC3339, "2026-11-01T01:30:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	second, err := time.Parse(time.RFC3339, "2026-11-01T01:30:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, instant := range []time.Time{first, second} {
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, instant)
			}
			return a
		}}))
		logger.WarnContext(context.Background(), "share daemon retry", "crid", "abc")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "share-daemon.err.log"), logs.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	red := newRedactor("")
	for _, instant := range []time.Time{first, second} {
		got := collectDaemonLogs(dir, instant.Add(-time.Second), instant.Add(time.Second), []string{"abc"}, red, 10)
		if len(got) != 1 {
			t.Fatalf("offset-bearing window should select one occurrence: %v", got)
		}
		at, ok := parseDaemonLogTime(got[0], time.Local)
		if !ok || !at.Equal(instant) {
			t.Fatalf("selected wrong repeated-hour event: %q", got[0])
		}
		if matched := contextLines(got, []string{"abc"}, instant, time.Second, 10); len(matched) != 1 {
			t.Fatal("context filtering lost the explicit offset")
		}
	}
}
