package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestResolveClientProduct(t *testing.T) {
	for _, tc := range []struct {
		name, value, product string
	}{
		{name: "empty is unset"},
		{name: "desktop", value: "qurl-desktop/1.2.3", product: "qurl-desktop/1.2.3"},
		{name: "invalid", value: "desktop/1\r\nInjected: yes"},
	} {
		for _, verbose := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/verbose=%t", tc.name, verbose), func(t *testing.T) {
				var diagnostics bytes.Buffer
				opts := &globalOpts{
					verbose: verbose,
					streams: &output.Streams{Err: &diagnostics},
					lookupEnv: func(name string) (string, bool) {
						if name != "QURL_CLIENT_PRODUCT" {
							return "", false
						}
						return tc.value, true
					},
				}
				opts.resolveClientProduct()
				if opts.resolvedClientProduct != tc.product {
					t.Errorf("resolvedClientProduct = %q, want %q", opts.resolvedClientProduct, tc.product)
				}
				wantDiagnostic := ""
				if verbose && tc.value != "" && tc.product == "" {
					wantDiagnostic = "[debug] ignoring invalid QURL_CLIENT_PRODUCT: expected name/version with token characters, at most 64 bytes\n"
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
				env:  map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CLIENT_PRODUCT": tc.value},
			})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			if got := srv.Requests()[0].Header.Get("User-Agent"); got != tc.wantUA {
				t.Errorf("User-Agent = %q, want %q", got, tc.wantUA)
			}
			if got := strings.Contains(res.stderr.String(), "ignoring invalid QURL_CLIENT_PRODUCT"); got != tc.wantNote {
				t.Errorf("invalid-value note = %t, want %t; stderr %q", got, tc.wantNote, res.stderr.String())
			}
		})
	}
}
