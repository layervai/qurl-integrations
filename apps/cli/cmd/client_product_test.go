package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestResolveClientProduct(t *testing.T) {
	for _, tc := range []struct {
		name, value, product string
		empty                bool
	}{
		{name: "unset"},
		{name: "empty is unset", empty: true},
		{name: "desktop", value: "qurl-desktop/1.2.3", product: "qurl-desktop/1.2.3"},
		{name: "invalid", value: "desktop/1\r\nInjected: yes"},
		{name: "reserved", value: "qurl-cli/1.0"},
	} {
		for _, verbose := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/verbose=%t", tc.name, verbose), func(t *testing.T) {
				var diagnostics bytes.Buffer
				opts := &globalOpts{
					verbose: verbose,
					streams: &output.Streams{Err: &diagnostics},
					lookupEnv: func(name string) (string, bool) {
						if name != clientProductEnv {
							return "", false
						}
						// The empty case is set-but-empty; "unset" is not set at all.
						return tc.value, tc.value != "" || tc.empty
					},
				}
				opts.resolveClientProduct()
				if opts.resolvedClientProduct != tc.product {
					t.Errorf("resolvedClientProduct = %q, want %q", opts.resolvedClientProduct, tc.product)
				}
				wantDiagnostic := ""
				if verbose && tc.value != "" && tc.product == "" {
					wantDiagnostic = "[debug] ignoring invalid " + clientProductEnv + ": expected " + qurlapi.ClientProductRule + "\n"
				}
				if got := diagnostics.String(); got != wantDiagnostic {
					t.Errorf("diagnostics = %q, want %q", got, wantDiagnostic)
				}
			})
		}
	}
}

// TestClientProductReachesRequestsThroughRoot proves resolveSettings wires the
// environment into the real command tree, with --verbose already parsed.
func TestClientProductReachesRequestsThroughRoot(t *testing.T) {
	for _, tc := range []struct {
		name, value, wantUA string
		wantNote            bool
	}{
		{name: "valid", value: "qurl-desktop/1.2.3", wantUA: "qurl-desktop/1.2.3 qurl-cli/test"},
		{name: "invalid", value: "desktop", wantUA: "qurl-cli/test", wantNote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, "--verbose", "list"},
				env:  map[string]string{"QURL_API_KEY": testAPIKey, clientProductEnv: tc.value},
			})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			reqs := srv.Requests()
			if len(reqs) == 0 {
				t.Fatal("the command sent no request")
			}
			if got := reqs[0].Header.Get("User-Agent"); got != tc.wantUA {
				t.Errorf("User-Agent = %q, want %q", got, tc.wantUA)
			}
			if got := strings.Contains(res.stderr.String(), "ignoring invalid "+clientProductEnv); got != tc.wantNote {
				t.Errorf("invalid-value note = %t, want %t; stderr %q", got, tc.wantNote, res.stderr.String())
			}
		})
	}
}
