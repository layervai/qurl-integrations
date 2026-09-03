package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
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

// TestDaemonRunRejectsAJobVersionForAnotherMode pins that the resident
// daemon's job version is mode-bearing: a job installed for one mode cannot
// start the daemon in another, so a definition change is always a restart in
// the new mode rather than a silent divergence.
func TestDaemonRunRejectsAJobVersionForAnotherMode(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "3/test", "--share-group-mode", "per-share"}})
	if res.code != 1 || !strings.Contains(res.stderr.String(), `does not match binary "3/test/per-share"`) {
		t.Fatalf("mode-mismatched job version = exit %d stderr %q", res.code, res.stderr.String())
	}
	single := runCLI(t, &runOpts{args: []string{"daemon", "run", "--job-version", "3/test/per-share", "--share-group-mode", "single"}})
	if single.code != 1 || !strings.Contains(single.stderr.String(), `does not match binary "3/test"`) {
		t.Fatalf("single-mode daemon accepted a per-share job version: exit %d stderr %q", single.code, single.stderr.String())
	}
}

func TestShareDaemonJobCarriesTheResolvedShareGroupMode(t *testing.T) {
	_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	opts.resolvedEndpoint = config.DefaultEndpoint
	opts.resolvedShareGroupMode = connectordaemon.GroupModePerShare
	dir := t.TempDir()
	controller, ok := opts.newShareDaemon(filepath.Join(dir, "state"), filepath.Join(dir, "logs")).(*connectordaemon.JobController)
	if !ok {
		t.Fatalf("production share daemon controller is %T, want the native job controller", controller)
	}
	if controller.ShareGroupMode != connectordaemon.GroupModePerShare {
		t.Fatalf("job controller mode = %q, want the resolved per-share mode", controller.ShareGroupMode)
	}
}
