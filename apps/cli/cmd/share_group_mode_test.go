package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// TestShareGroupModeResolvesThroughSettingsPrecedence pins the session group
// mode to the one precedence chain every setting uses: the daemon run flag
// beats QURL_SHARE_GROUP_MODE, which beats the profile's share_group_mode,
// which beats the built-in single default.
func TestShareGroupModeResolvesThroughSettingsPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		flag   string
		env    map[string]string
		config string
		want   connectordaemon.GroupMode
	}{
		{"flag wins over everything", "single", map[string]string{"QURL_SHARE_GROUP_MODE": "per-share"}, "per-share", connectordaemon.GroupModeSingle},
		{"env wins over config", "", map[string]string{"QURL_SHARE_GROUP_MODE": "per-share"}, "single", connectordaemon.GroupModePerShare},
		{"config wins over default", "", nil, "per-share", connectordaemon.GroupModePerShare},
		{"default when nothing set", "", nil, "", connectordaemon.GroupModeSingle},
		{"empty env value falls through", "", map[string]string{"QURL_SHARE_GROUP_MODE": ""}, "per-share", connectordaemon.GroupModePerShare},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			if tc.config != "" {
				if err := os.WriteFile(config.Path(configDir), []byte("share_group_mode: "+tc.config+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}, func(g *globalOpts) {
				g.configDir = configDir
				g.lookupEnv = func(key string) (string, bool) { v, ok := tc.env[key]; return v, ok }
			})
			opts.shareGroupMode = tc.flag
			if err := opts.resolveSettings(); err != nil {
				t.Fatal(err)
			}
			if opts.resolvedShareGroupMode != tc.want {
				t.Fatalf("resolved share group mode = %q, want %q", opts.resolvedShareGroupMode, tc.want)
			}
		})
	}
}

func TestShareGroupModeRejectsUnknownValuesAtEachSource(t *testing.T) {
	// A flag or environment typo is a usage error; a config-file typo routes to
	// the configuration exit code like every other file setting.
	flag := runCLI(t, &runOpts{args: []string{"daemon", "run", "--share-group-mode", "both"}})
	if flag.code != 2 || !strings.Contains(flag.stderr.String(), "invalid share group mode") {
		t.Fatalf("flag typo = exit %d stderr %q, want usage error", flag.code, flag.stderr.String())
	}
	env := runCLI(t, &runOpts{args: []string{"list"}, env: map[string]string{"QURL_API_KEY": testAPIKey, "QURL_SHARE_GROUP_MODE": "both"}})
	if env.code != 2 || !strings.Contains(env.stderr.String(), "invalid share group mode") {
		t.Fatalf("env typo = exit %d stderr %q, want usage error", env.code, env.stderr.String())
	}
	configDir := t.TempDir()
	if err := os.WriteFile(config.Path(configDir), []byte("share_group_mode: both\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := runCLI(t, &runOpts{args: []string{"list"}, configDir: configDir})
	if file.code != 3 || !strings.Contains(file.stderr.String(), "share_group_mode") {
		t.Fatalf("config typo = exit %d stderr %q, want configuration error", file.code, file.stderr.String())
	}
}

// stubDaemonJobRefusalHold replaces the refusal hold with a recorder so a
// refused job returns at once.
func stubDaemonJobRefusalHold(t *testing.T) *[]time.Duration {
	t.Helper()
	original := waitDaemonJobRefusal
	t.Cleanup(func() { waitDaemonJobRefusal = original })
	var holds []time.Duration
	waitDaemonJobRefusal = func(_ context.Context, hold time.Duration) { holds = append(holds, hold) }
	return &holds
}

// TestDaemonJobRefusalHoldEndsWhenTheDaemonIsStopped pins that a held refusal
// does not delay a job replacement: the manager's stop cancels the context.
func TestDaemonJobRefusalHoldEndsWhenTheDaemonIsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		waitDaemonJobRefusal(ctx, daemonJobRefusalHold)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refusal hold outlived its canceled context")
	}
}

// TestDaemonRunRejectsAJobVersionForAnotherMode pins that the resident
// daemon's job version is mode-bearing: a job installed for one mode cannot
// start the daemon in another, so a definition change is always a restart in
// the new mode rather than a silent divergence. Each refusal holds once before
// it exits, so the native manager cannot restart it in a tight loop.
func TestDaemonRunRejectsAJobVersionForAnotherMode(t *testing.T) {
	holds := stubDaemonJobRefusalHold(t)
	res := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "5/test", "--share-group-mode", "per-share"}})
	if res.code != 1 || !strings.Contains(res.stderr.String(), `does not match binary "5/test/per-share"`) {
		t.Fatalf("mode-mismatched job version = exit %d stderr %q", res.code, res.stderr.String())
	}
	single := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "5/test/per-share", "--share-group-mode", "single"}})
	if single.code != 1 || !strings.Contains(single.stderr.String(), `does not match binary "5/test"`) {
		t.Fatalf("single-mode daemon accepted a per-share job version: exit %d stderr %q", single.code, single.stderr.String())
	}
	protocol := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "4/test"}})
	if protocol.code != 1 || !strings.Contains(protocol.stderr.String(), `does not match binary "5/test"`) {
		t.Fatalf("daemon accepted a job for another protocol: exit %d stderr %q", protocol.code, protocol.stderr.String())
	}
	if !slices.Equal(*holds, []time.Duration{daemonJobRefusalHold, daemonJobRefusalHold, daemonJobRefusalHold}) {
		t.Fatalf("refusal holds = %v, want one %s hold per refused start", *holds, daemonJobRefusalHold)
	}
}

// TestDaemonRunHoldsAJobItCannotParse covers a downgrade across an argument
// change: flag parsing fails before the job version is compared, and that
// start must hold like any other refused job. A hand-run daemon with a
// mistyped flag passes no job version and fails at once.
func TestDaemonRunHoldsAJobItCannotParse(t *testing.T) {
	holds := stubDaemonJobRefusalHold(t)
	job := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "6/later", "--added-by-a-later-release", "value"}})
	if job.code != 2 || !strings.Contains(job.stderr.String(), "unknown flag: --added-by-a-later-release") {
		t.Fatalf("unparsable job = exit %d stderr %q, want a usage error", job.code, job.stderr.String())
	}
	if !slices.Equal(*holds, []time.Duration{daemonJobRefusalHold}) {
		t.Fatalf("unparsable job holds = %v, want one %s hold", *holds, daemonJobRefusalHold)
	}
	hand := runCLI(t, &runOpts{args: []string{"daemon", "run", "--not-a-flag"}})
	if hand.code != 2 || len(*holds) != 1 {
		t.Fatalf("hand-run flag typo = exit %d holds %v, want an immediate usage error", hand.code, *holds)
	}
}

// TestDaemonRunAcceptsAJobFromAnotherBinaryVersion covers a package upgrade:
// the binary is replaced and the job that starts it still carries the earlier
// version. The daemon must get past the job check instead of exiting, and it
// keeps reporting the job's own version so the next ensure replaces the job.
func TestDaemonRunAcceptsAJobFromAnotherBinaryVersion(t *testing.T) {
	holds := stubDaemonJobRefusalHold(t)
	res := runCLI(t, &runOpts{args: []string{"daemon", "run", "--state-dir", t.TempDir(), "--job-version", "5/earlier"}})
	if strings.Contains(res.stderr.String(), "does not match binary") {
		t.Fatalf("daemon refused a job from another binary version: exit %d stderr %q", res.code, res.stderr.String())
	}
	// The empty state directory stops the start at the owner check, which is
	// after the job check and before anything listens.
	if res.code == 0 || !strings.Contains(res.stderr.String(), "no durable account owner") {
		t.Fatalf("daemon start = exit %d stderr %q, want the owner check to be reached", res.code, res.stderr.String())
	}
	if len(*holds) != 0 {
		t.Fatalf("an accepted job held %v", *holds)
	}

	_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	opts.resolvedShareGroupMode = connectordaemon.GroupModeSingle
	opts.resolvedSupervision = connectorstate.RuntimeSupervisionNative
	for job, want := range map[string]string{"5/earlier": "5/earlier", "5/test": "5/test", "": "5/test"} {
		got, err := admitDaemonJobVersion(context.Background(), opts, job)
		if err != nil || got != want {
			t.Fatalf("admitDaemonJobVersion(%q) = %q, %v; want %q", job, got, err, want)
		}
	}
	// External supervision never passes a job version; a refused one there
	// fails at once, because that supervisor owns the restart policy.
	opts.resolvedSupervision = connectorstate.RuntimeSupervisionExternal
	if _, err := admitDaemonJobVersion(context.Background(), opts, "4/test"); err == nil || len(*holds) != 0 {
		t.Fatalf("external refusal = %v with holds %v, want an immediate error", err, *holds)
	}
}

func TestShareDaemonJobCarriesTheResolvedShareGroupMode(t *testing.T) {
	_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	opts.resolvedEndpoint = config.DefaultEndpoint
	opts.resolvedShareGroupMode = connectordaemon.GroupModePerShare
	dir := t.TempDir()
	daemon, err := opts.newShareDaemon(filepath.Join(dir, "state"), filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	controller, ok := daemon.(*connectordaemon.JobController)
	if !ok {
		t.Fatalf("production share daemon controller is %T, want the native job controller", controller)
	}
	if controller.ShareGroupMode != connectordaemon.GroupModePerShare {
		t.Fatalf("job controller mode = %q, want the resolved per-share mode", controller.ShareGroupMode)
	}
}
