package main

import (
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

func TestShareSchedule(t *testing.T) {
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	end := start.Add(time.Hour)
	window := []string{"--not-before=" + start.Format(time.RFC3339), "--expires-at=" + end.Format(time.RFC3339)}
	for _, tc := range []struct {
		name    string
		args    []string
		saved   *accessrules.LinkLimits
		wantErr bool
		once    bool
	}{
		{name: "default single use", args: window, once: true},
		{name: "saved reusable", args: window, saved: &accessrules.LinkLimits{ExpiresIn: "5m"}},
		{name: "explicit reusable", args: append(append([]string{}, window...), "--one-time-use=false")},
		{name: "explicit single use", args: append(append([]string{}, window...), "--one-time-use=true"), saved: &accessrules.LinkLimits{ExpiresIn: "5m"}, once: true},
		{name: "ttl conflict", args: append(append([]string{}, window...), "--ttl=5m"), wantErr: true},
		{name: "one time use alone", args: []string{"--one-time-use"}, wantErr: true},
		{name: "missing expiry", args: window[:1], wantErr: true},
		{name: "missing start", args: window[1:], wantErr: true},
		{name: "past start", args: []string{"--not-before=2000-01-01T00:00:00Z", window[1]}, wantErr: true},
		{name: "zero window", args: []string{window[0], "--expires-at=" + start.Format(time.RFC3339)}, wantErr: true},
		{name: "subsecond start", args: []string{"--not-before=" + start.Add(time.Millisecond).Format(time.RFC3339Nano), window[1]}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schedule shareSchedule
			cmd := &cobra.Command{}
			flags := cmd.Flags()
			flags.StringVar(&schedule.notBefore, "not-before", "", "")
			flags.StringVar(&schedule.expiresAt, "expires-at", "", "")
			flags.BoolVar(&schedule.oneTimeUse, "one-time-use", false, "")
			flags.Duration("ttl", 0, "")
			if err := flags.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			limits, err := schedule.apply(cmd, tc.saved)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected scheduling options to be rejected")
				}
				return
			}
			if err != nil || limits == nil || limits.Window == nil || !limits.Window.Start.Equal(start) || !limits.Window.End.Equal(end) {
				t.Fatalf("expected access window, got %+v, %v", limits, err)
			}
			if once := limits.OneTimeUse != nil && *limits.OneTimeUse; once != tc.once {
				t.Fatalf("one time use = %t, want %t", once, tc.once)
			}
		})
	}
}
