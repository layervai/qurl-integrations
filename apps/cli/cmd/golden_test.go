package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/clitest"
)

// goldenCase drives one command through the three output variants. Variants:
// tty (color, terminals attached), plain (piped), json (-o json, piped).
type goldenCase struct {
	name     string
	args     func(srv *apitest.Server) []string
	prepare  func(srv *apitest.Server)
	env      func(srv *apitest.Server) map[string]string
	variants []string
	wantCode int
	// stdin is piped input (login's key); empty means an empty pipe.
	stdin string
	// keyring builds the injected keyring stand-in per variant run (a fresh
	// one each run, so a mutating command cannot bleed into the next
	// variant); nil means the harness default (empty, available).
	keyring func() *fakeKeyring
	// chdirTemp runs the variant in a fresh temp working directory, so
	// cases whose output embeds a relative --file path stay deterministic
	// and leave nothing behind in the repo tree.
	chdirTemp bool
	// setup seeds the (possibly temp) working directory before the run.
	setup func(t *testing.T)
	// stdoutGolden/stderrGolden select which streams are golden-compared;
	// a stream not selected must be byte-empty.
	stdoutGolden bool
	stderrGolden bool
}

func goldenVariants() []string { return []string{"tty", "plain", "json"} }

// TestGoldens pins the rendered bytes of every implemented command across
// TTY, plain, and JSON projections, for success and error anatomies alike.
func TestGoldens(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	otherCRID := apitest.DeriveCRID(t, []byte("a-different-resource-key"), apitest.VersionTest)

	cases := []goldenCase{
		{
			name:         "publish",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// Publishing a URL that already has an active resource: the
			// text document says so itself, so stderr stays empty.
			name:         "publish_existing",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			variants:     []string{"tty", "plain"},
			stdoutGolden: true,
		},
		{
			// The JSON document only gains found_existing: true; the replay
			// note is a stderr status line.
			name:         "publish_existing",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			variants:     []string{"json"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			name:         "resolve",
			args:         func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "list",
			args:         func(*apitest.Server) []string { return []string{"list"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "delete",
			args:         func(srv *apitest.Server) []string { return []string{"delete", srv.Key.CRID, "--yes"} },
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "delete",
			args:         func(srv *apitest.Server) []string { return []string{"delete", srv.Key.CRID, "--yes"} },
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			name: "error_notfound",
			args: func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/resolve",
					apitest.HandlerNotFound404(t, "resource_not_found"))
			},
			variants:     []string{"tty", "plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_revoked",
			args: func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/resolve", apitest.HandlerRevoked400(t))
			},
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_retired",
			args: func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/resolve", apitest.HandlerTombstoned410(t))
			},
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_dark503",
			args: func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/resolve", apitest.HandlerDark503(t))
			},
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			name: "error_verify_mismatch",
			args: func(srv *apitest.Server) []string { return []string{"resolve", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.SetResolveCRID(otherCRID)
			},
			variants:     []string{"plain"},
			wantCode:     12,
			stderrGolden: true,
		},
		{
			name: "error_nokey",
			args: func(*apitest.Server) []string { return []string{"list"} },
			env: func(srv *apitest.Server) map[string]string {
				return map[string]string{"QURL_ENDPOINT": srv.URL}
			},
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			name:         "whoami",
			args:         func(*apitest.Server) []string { return []string{"whoami"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "login",
			args:         func(*apitest.Server) []string { return []string{"login"} },
			stdin:        testAPIKey + "\n",
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "login",
			args:         func(*apitest.Server) []string { return []string{"login"} },
			stdin:        testAPIKey + "\n",
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// The keyring-unavailable save: the key lands in the credential
			// file and the warning says so.
			name:         "login_fallback",
			args:         func(*apitest.Server) []string { return []string{"login"} },
			stdin:        testAPIKey + "\n",
			keyring:      func() *fakeKeyring { return &fakeKeyring{unavailable: true} },
			variants:     []string{"plain"},
			stderrGolden: true,
		},
		{
			name:         "logout",
			args:         func(*apitest.Server) []string { return []string{"logout"} },
			keyring:      func() *fakeKeyring { return &fakeKeyring{key: testAPIKeyStored} },
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "logout",
			args:         func(*apitest.Server) []string { return []string{"logout"} },
			keyring:      func() *fakeKeyring { return &fakeKeyring{key: testAPIKeyStored} },
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// Idempotent logout with nothing stored anywhere: exit 0, a note.
			name:         "logout_none",
			args:         func(*apitest.Server) []string { return []string{"logout"} },
			variants:     []string{"plain"},
			stderrGolden: true,
		},
		{
			// login with a key the platform does not recognize: exit 4.
			name: "error_login_invalid",
			args: func(*apitest.Server) []string { return []string{"login"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
			},
			stdin:        testAPIKey + "\n",
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			// login with an expired key: exit 4 and the new-key remedy.
			name: "error_login_expired",
			args: func(*apitest.Server) []string { return []string{"login"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyExpired401(t))
			},
			stdin:        testAPIKey + "\n",
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			// A frozen account is an account-standing condition (exit 6 with
			// the standing message), not a generic forbidden.
			name: "error_frozen",
			args: func(*apitest.Server) []string { return []string{"whoami"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAccountFrozen403(t))
			},
			variants:     []string{"plain"},
			wantCode:     6,
			stderrGolden: true,
		},
		{
			name: "error_scope",
			args: func(*apitest.Server) []string { return []string{"whoami"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerInsufficientScope403(t))
			},
			variants:     []string{"plain"},
			wantCode:     6,
			stderrGolden: true,
		},
		{
			name: "error_ratelimited",
			args: func(*apitest.Server) []string { return []string{"list"} },
			prepare: func(srv *apitest.Server) {
				srv.ScriptRepeat(http.MethodGet, "/v1/resources", 3, apitest.Handler429(t, 2))
			},
			variants:     []string{"plain"},
			wantCode:     9,
			stderrGolden: true,
		},
		{
			// Browser mode: the verified link plus expiry on stdout, the
			// launch note on stderr. TTY-only by contract — the piped
			// variant of a bare get is error_get_piped below.
			name:         "get_browser",
			args:         func(srv *apitest.Server) []string { return []string{"get", srv.Key.CRID} },
			variants:     []string{"tty"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// Download mode: the confirmation goes to stderr; stdout stays
			// data-free. The relative --file path keeps the message
			// deterministic under chdirTemp.
			name: "get_file",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetResolveQURL(srv.URL + apitest.DownloadPath)
			},
			chdirTemp:    true,
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name: "get_file",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetResolveQURL(srv.URL + apitest.DownloadPath)
			},
			chdirTemp:    true,
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// The §16.2 refusal: piped stdout with no --file.
			name:         "error_get_piped",
			args:         func(srv *apitest.Server) []string { return []string{"get", srv.Key.CRID} },
			variants:     []string{"plain"},
			wantCode:     2,
			stderrGolden: true,
		},
		{
			// Overwrite refusal (exit 7, the Conflict row) fires before any
			// request; the golden pins the --force remedy wording.
			name: "error_get_exists",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			chdirTemp: true,
			setup: func(t *testing.T) {
				if err := os.WriteFile("out.bin", []byte("already here"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			variants:     []string{"plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// Expiry that outlives the single automatic refresh: two 410s
			// from the link host, exit 5.
			name: "error_get_expired",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetResolveQURL(srv.URL + apitest.DownloadPath)
				srv.ScriptRepeat(http.MethodGet, apitest.DownloadPath, 2,
					func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
			},
			chdirTemp:    true,
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
	}

	// Anchor the golden tree before any case changes the working directory.
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatalf("resolve golden dir: %v", err)
	}

	for _, tc := range cases {
		for _, variant := range tc.variants {
			t.Run(tc.name+"_"+variant, func(t *testing.T) {
				if tc.chdirTemp {
					t.Chdir(t.TempDir())
				}
				if tc.setup != nil {
					tc.setup(t)
				}
				srv := apitest.NewServerWithKey(t, apitest.FixedResourceKey(t))
				if tc.prepare != nil {
					tc.prepare(srv)
				}

				args := tc.args(srv)
				env := map[string]string{"QURL_API_KEY": testAPIKey}
				if tc.env != nil {
					env = tc.env(srv)
				}
				o := &runOpts{
					args: append([]string{"--endpoint", srv.URL}, args...),
					env:  env,
					tty:  variant == "tty",
				}
				if tc.stdin != "" {
					o.stdin = strings.NewReader(tc.stdin)
				}
				if tc.keyring != nil {
					o.keyring = tc.keyring()
				}
				if variant == "json" {
					o.args = append(o.args, "-o", "json")
				}
				res := runCLI(t, o)

				if res.code != tc.wantCode {
					t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
						res.code, tc.wantCode, res.stdout.String(), res.stderr.String())
				}
				if tc.stdoutGolden {
					clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".golden"), res.stdout.Bytes())
				} else {
					mustEmptyStdout(t, res)
				}
				if tc.stderrGolden {
					clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".stderr.golden"), res.stderr.Bytes())
				} else if res.stderr.Len() != 0 {
					t.Fatalf("stderr must be empty for %s_%s, got %q", tc.name, variant, res.stderr.String())
				}
			})
		}
	}
}

// TestGoldenFilesAreLFOnly is the CRLF sentinel: no golden file may contain
// a carriage return, on any platform, ever. The repo .gitattributes protects
// checkout; this protects authoring.
func TestGoldenFilesAreLFOnly(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden files found; the sentinel would be vacuous")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, entry.Name())))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if i := bytes.IndexByte(data, '\r'); i >= 0 {
			t.Errorf("%s contains a carriage return at byte %d; goldens are LF-only", entry.Name(), i)
		}
	}
}
