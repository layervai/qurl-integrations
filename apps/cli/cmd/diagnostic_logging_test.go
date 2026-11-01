package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDaemonDiagnosticsUseOffsetsAtCLIEntry(t *testing.T) {
	if mode := os.Getenv("QURL_TEST_DIAGNOSTIC_HELPER"); mode != "" {
		os.Args = []string{"qurl", "--help"}
		if mode == "daemon" {
			os.Args = []string{"qurl", "daemon", "--help"}
		}
		if mode == "daemon-run" {
			os.Args = []string{"qurl", "daemon", "run", "--help"}
		}
		if mode == "daemon-flags" {
			os.Args = []string{"qurl", "--verbose", "daemon", "run", "--help"}
		}
		if Main("test") != 0 {
			os.Exit(2)
		}
		slog.Warn("cli-slog-probe")
		log.Print("cli-log-probe")
		os.Exit(0)
	}
	for _, mode := range []string{"interactive", "daemon", "daemon-run", "daemon-flags"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonDiagnosticsUseOffsetsAtCLIEntry$") //nolint:gosec // Exact current test binary; help commands never start a daemon.
			home := t.TempDir()
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home,
				"APPDATA=" + home, "LOCALAPPDATA=" + home, "TEMP=" + home, "TMP=" + home, "TMPDIR=" + home,
				"SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("WINDIR"),
				"QURL_ENDPOINT=http://127.0.0.1:1",
				"USER=cli-log-fixture", "USERNAME=cli-log-fixture", "QURL_TEST_DIAGNOSTIC_HELPER=" + mode}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("safe help helper: %v: %s", err, stderr.String())
			}
			found := 0
			for _, line := range strings.Split(stderr.String(), "\n") {
				if !strings.Contains(line, "cli-slog-probe") && !strings.Contains(line, "cli-log-probe") {
					continue
				}
				found++
				fields := strings.Fields(line)
				stamp, offsetFormat := strings.CutPrefix(fields[0], "time=")
				if mode == "interactive" {
					if offsetFormat {
						t.Fatal("normal command diagnostics changed format")
					}
				} else {
					if !offsetFormat {
						t.Fatalf("daemon did not configure offset timestamps: %q", line)
					}
					if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
						t.Fatalf("ambiguous daemon timestamp: %q", stamp)
					}
				}
			}
			if found != 2 {
				t.Fatalf("expected slog and standard log diagnostics: %q", stderr.String())
			}
		})
	}
}
