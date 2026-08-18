package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
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
			name: "error_ratelimited",
			args: func(*apitest.Server) []string { return []string{"list"} },
			prepare: func(srv *apitest.Server) {
				srv.ScriptRepeat(http.MethodGet, "/v1/resources", 3, apitest.Handler429(t, 2))
			},
			variants:     []string{"plain"},
			wantCode:     9,
			stderrGolden: true,
		},
	}

	for _, tc := range cases {
		for _, variant := range tc.variants {
			t.Run(tc.name+"_"+variant, func(t *testing.T) {
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
				if variant == "json" {
					o.args = append(o.args, "-o", "json")
				}
				res := runCLI(t, o)

				if res.code != tc.wantCode {
					t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
						res.code, tc.wantCode, res.stdout.String(), res.stderr.String())
				}
				if tc.stdoutGolden {
					clitest.Golden(t, tc.name+"."+variant+".golden", res.stdout.Bytes())
				} else {
					mustEmptyStdout(t, res)
				}
				if tc.stderrGolden {
					clitest.Golden(t, tc.name+"."+variant+".stderr.golden", res.stderr.Bytes())
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
