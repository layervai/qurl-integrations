package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// TestSupervisionResolvesThroughSettingsPrecedence pins the daemon
// supervision mode to the one precedence chain every setting uses: the
// persistent --supervision flag beats QURL_DAEMON_SUPERVISION, which beats
// the profile's daemon_supervision, which beats the built-in native default.
func TestSupervisionResolvesThroughSettingsPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		flag   string
		env    map[string]string
		config string
		want   connectorstate.RuntimeSupervision
	}{
		{"flag wins over everything", "native", map[string]string{"QURL_DAEMON_SUPERVISION": "external"}, "external", connectorstate.RuntimeSupervisionNative},
		{"env wins over config", "", map[string]string{"QURL_DAEMON_SUPERVISION": "external"}, "native", connectorstate.RuntimeSupervisionExternal},
		{"config wins over default", "", nil, "external", connectorstate.RuntimeSupervisionExternal},
		{"default when nothing set", "", nil, "", connectorstate.RuntimeSupervisionNative},
		{"empty env value falls through", "", map[string]string{"QURL_DAEMON_SUPERVISION": ""}, "external", connectorstate.RuntimeSupervisionExternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			if tc.config != "" {
				if err := os.WriteFile(config.Path(configDir), []byte("daemon_supervision: "+tc.config+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}, func(g *globalOpts) {
				g.configDir = configDir
				g.lookupEnv = func(key string) (string, bool) { v, ok := tc.env[key]; return v, ok }
			})
			opts.supervision = tc.flag
			if err := opts.resolveSettings(); err != nil {
				t.Fatal(err)
			}
			if opts.resolvedSupervision != tc.want {
				t.Fatalf("resolved supervision = %q, want %q", opts.resolvedSupervision, tc.want)
			}
		})
	}
}

func TestSupervisionRejectsUnknownValuesAtEachSource(t *testing.T) {
	// A flag or environment typo is a usage error; a config-file typo routes to
	// the configuration exit code like every other file setting.
	flag := runCLI(t, &runOpts{args: []string{"--supervision", "desktop", "list"}})
	if flag.code != 2 || !strings.Contains(flag.stderr.String(), "invalid daemon supervision") {
		t.Fatalf("flag typo = exit %d stderr %q, want usage error", flag.code, flag.stderr.String())
	}
	env := runCLI(t, &runOpts{args: []string{"list"}, env: map[string]string{"QURL_API_KEY": testAPIKey, "QURL_DAEMON_SUPERVISION": "desktop"}})
	if env.code != 2 || !strings.Contains(env.stderr.String(), "invalid daemon supervision") {
		t.Fatalf("env typo = exit %d stderr %q, want usage error", env.code, env.stderr.String())
	}
	configDir := t.TempDir()
	if err := os.WriteFile(config.Path(configDir), []byte("daemon_supervision: desktop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := runCLI(t, &runOpts{args: []string{"list"}, configDir: configDir})
	if file.code != 3 || !strings.Contains(file.stderr.String(), "daemon_supervision") {
		t.Fatalf("config typo = exit %d stderr %q, want configuration error", file.code, file.stderr.String())
	}
}

func TestShareDaemonJobCarriesTheResolvedSupervision(t *testing.T) {
	_, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	opts.resolvedEndpoint = config.DefaultEndpoint
	opts.resolvedShareGroupMode = connectordaemon.GroupModeSingle
	opts.resolvedSupervision = connectorstate.RuntimeSupervisionExternal
	dir := t.TempDir()
	daemon, err := opts.newShareDaemon(filepath.Join(dir, "state"), filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	controller, ok := daemon.(*connectordaemon.JobController)
	if !ok {
		t.Fatalf("production share daemon controller is %T, want the native job controller", controller)
	}
	if controller.Supervision != connectorstate.RuntimeSupervisionExternal {
		t.Fatalf("job controller supervision = %q, want the resolved external mode", controller.Supervision)
	}
}

// TestNativeCommandRefusesExternalNamespace pins rule 2 of external
// supervision: a natively supervised qurl never mutates — and in particular
// never installs a background job over — a namespace another supervisor
// owns. Every mutating command fails with the configuration exit code before
// any cloud or daemon call; read-only commands are unaffected.
func TestNativeCommandRefusesExternalNamespace(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	daemon := &recordingShareDaemon{}
	const wantMessage = `runtime supervision is "external", not "native"; run this command with --supervision external`
	for _, args := range [][]string{
		{"publish", "http://127.0.0.1:3000"},
		{"start", srv.Key.CRID},
		{"restart", srv.Key.CRID},
		{"stop", srv.Key.CRID},
		{"delete", srv.Key.CRID, "--yes"},
		{"login"},
	} {
		t.Run(args[0], func(t *testing.T) {
			res := runCLI(t, &runOpts{
				args:        append([]string{"--endpoint", srv.URL}, args...),
				env:         map[string]string{"QURL_API_KEY": testAPIKey},
				stdin:       strings.NewReader(testAPIKey + "\n"),
				shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
				localResource:   resolvedLocalResource(srv, true),
			})
			if res.code != 3 || !strings.Contains(res.stderr.String(), wantMessage) {
				t.Fatalf("%s against an external namespace = exit %d stderr %q, want exit 3 with %q", args[0], res.code, res.stderr.String(), wantMessage)
			}
			mustEmptyStdout(t, res)
		})
	}
	if daemon.ensures != 0 || daemon.reloads != 0 {
		t.Fatalf("native commands reached the daemon controller: %+v", daemon)
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("native commands mutated cloud state before the supervision check: %#v", requests)
	}
	for _, args := range [][]string{{"list"}, {"whoami"}} {
		res := runCLI(t, &runOpts{
			args:        append([]string{"--endpoint", srv.URL}, args...),
			env:         map[string]string{"QURL_API_KEY": testAPIKey},
			shareDaemon: daemon, shareStateDir: stateDir,
		})
		if res.code != 0 {
			t.Fatalf("read-only %s against an external namespace = exit %d stderr %q, want success", args[0], res.code, res.stderr.String())
		}
	}
}

func TestExternalCommandRefusesNativeNamespace(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:        []string{"--endpoint", srv.URL, "--supervision", "external", "publish", "http://127.0.0.1:3000"},
		env:         map[string]string{"QURL_API_KEY": testAPIKey},
		shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, true),
	})
	const wantMessage = `runtime supervision is "native", not "external"; run this command with --supervision native`
	if res.code != 3 || !strings.Contains(res.stderr.String(), wantMessage) {
		t.Fatalf("external publish against a native namespace = exit %d stderr %q, want exit 3 with %q", res.code, res.stderr.String(), wantMessage)
	}
	if daemon.ensures != 0 || len(srv.Requests()) != 0 {
		t.Fatalf("mismatched publish reached the daemon (%+v) or the service (%d requests)", daemon, len(srv.Requests()))
	}
	if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.RuntimeModeFile)); !os.IsNotExist(err) {
		t.Fatalf("a lifecycle command established the external policy marker: %v", err)
	}
}

// TestExternalPublishHandsOffToTheSupervisedDaemon pins that an external
// publish is the ordinary publish once the namespace matches: the same cloud
// calls, the same registry row, and one Ensure on the controller, which under
// external supervision only reloads a running daemon.
func TestExternalPublishHandsOffToTheSupervisedDaemon(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 4, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", 5, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 5, "serving"))
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_DAEMON_SUPERVISION": "external"},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, true),
	})
	if res.code != 0 {
		t.Fatalf("external publish = exit %d stderr %s", res.code, res.stderr.String())
	}
	if daemon.ensures != 1 || daemon.reloads != 0 {
		t.Fatalf("external publish daemon handoff = %+v, want exactly one Ensure", daemon)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "on" || local.ServingEpoch != 5 {
		t.Fatalf("external publish local state = %+v err=%v", local, err)
	}
}

// TestExternalLifecycleCommandsRollBackWhenTheDaemonIsAbsent pins the
// supervisor's recovery contract through the real command tree: when the
// externally supervised daemon is not running, publish, start, and restart
// exit with the Unavailable code, name the supervisor's start command, and
// turn their own cloud change back off (one compensating PUT) so the row and
// the platform agree the share is off until the supervisor starts the daemon
// and retries.
func TestExternalLifecycleCommandsRollBackWhenTheDaemonIsAbsent(t *testing.T) {
	cases := []struct {
		name      string
		args      func(srv *apitest.Server) []string
		seedState string // "" seeds no row (a fresh publish)
		script    func(t *testing.T, srv *apitest.Server, path string)
		wantPuts  int
	}{
		{
			name: "publish", args: func(srv *apitest.Server) []string { return []string{"publish", "http://127.0.0.1:3000"} },
			script: func(t *testing.T, srv *apitest.Server, path string) {
				srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 4, "stopped"))
				srv.Script(http.MethodPut, path,
					sharingResponse(t, srv, "on", 5, "connecting"),
					sharingResponse(t, srv, "off", 5, "stopped"),
				)
			},
			wantPuts: 2,
		},
		{
			name: "start", args: func(srv *apitest.Server) []string { return []string{"start", srv.Key.CRID} },
			seedState: "off",
			script: func(t *testing.T, srv *apitest.Server, path string) {
				srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 4, "stopped"))
				srv.Script(http.MethodPut, path,
					sharingResponse(t, srv, "on", 5, "connecting"),
					sharingResponse(t, srv, "off", 5, "stopped"),
				)
			},
			wantPuts: 2,
		},
		{
			name: "restart", args: func(srv *apitest.Server) []string { return []string{"restart", srv.Key.CRID} },
			seedState: "on",
			script: func(t *testing.T, srv *apitest.Server, path string) {
				srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 4, "serving"))
				srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", 5, "connecting"))
				srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", 5, "stopped"))
			},
			wantPuts: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := connectorStateTestDir(t)
			if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
				t.Fatal(err)
			}
			registry, err := openOwnedTestShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.seedState != "" {
				seed := localShareFixture(srv)
				seed.DesiredState = tc.seedState
				if err := registry.Put(context.Background(), &seed); err != nil {
					t.Fatal(err)
				}
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			tc.script(t, srv, path)
			daemon := &recordingShareDaemon{ensureErr: connectordaemon.ErrExternalDaemonNotRunning}
			res := runCLI(t, &runOpts{
				args:          append([]string{"--endpoint", srv.URL}, tc.args(srv)...),
				env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_DAEMON_SUPERVISION": "external"},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
				localResource:   resolvedLocalResource(srv, true),
			})
			if res.code != exitcode.Unavailable || !strings.Contains(res.stderr.String(), "qurl daemon run --supervision external") {
				t.Fatalf("%s without a daemon = exit %d stderr %q, want exit %d naming the supervisor's start command", tc.name, res.code, res.stderr.String(), exitcode.Unavailable)
			}
			mustEmptyStdout(t, res)
			puts := 0
			for _, request := range srv.Requests() {
				if request.Method == http.MethodPut && strings.HasSuffix(request.Path, "/sharing") {
					puts++
				}
			}
			if puts != tc.wantPuts {
				t.Fatalf("%s made %d PUT /sharing requests, want %d (the change and its compensation): %+v", tc.name, puts, tc.wantPuts, srv.Requests())
			}
			if daemon.ensures != 1 || daemon.reloads != 0 {
				t.Fatalf("%s daemon handoff = %+v, want exactly one Ensure", tc.name, daemon)
			}
			local, err := registry.Get(context.Background(), srv.Key.CRID)
			if err != nil || local.DesiredState != "off" || local.ServingEpoch != 5 {
				t.Fatalf("%s local row after the rollback = %+v err=%v, want off at the compensated epoch", tc.name, local, err)
			}
		})
	}
}
