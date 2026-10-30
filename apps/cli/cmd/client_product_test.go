package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestClientProductUserAgent(t *testing.T) {
	for _, tc := range []struct {
		name, value, product string
		set                  bool
	}{
		{name: "unset"},
		{name: "empty is unset", set: true},
		{name: "desktop", value: "qurl-desktop/1.2.3", product: "qurl-desktop/1.2.3", set: true},
		{name: "invalid", value: "desktop/1\r\nInjected: yes", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := "qurl-cli/test"
			if tc.product != "" {
				want = tc.product + " " + want
			}
			for _, registered := range []bool{false, true} {
				for _, verbose := range []bool{false, true} {
					var diagnostics bytes.Buffer
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if got := r.Header.Get("User-Agent"); got != want {
							t.Errorf("User-Agent = %q, want %q", got, want)
						}
						_, _ = w.Write([]byte(`{"data":{"owner_id":"owner-test"}}`))
					}))
					opts := &globalOpts{
						version: "test", resolvedEndpoint: srv.URL, httpClient: srv.Client(), verbose: verbose,
						streams:   &output.Streams{Err: &diagnostics},
						lookupEnv: func(name string) (string, bool) { return tc.value, name == "QURL_CLIENT_PRODUCT" && tc.set },
					}
					opts.resolveClientProduct()
					var client qurlapi.Client
					var err error
					if registered {
						client, err = opts.openRegisteredDeviceClient(context.Background(), srv.URL, &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)})
					} else {
						client, err = opts.apiClient("test-api-key")
					}
					if err != nil {
						srv.Close()
						t.Fatal(err)
					}
					wantDiagnostic := ""
					if verbose && tc.set && tc.value != "" && tc.product == "" {
						wantDiagnostic = "[debug] ignoring invalid QURL_CLIENT_PRODUCT: expected name/version with token characters, at most 64 bytes\n"
					}
					if got := diagnostics.String(); got != wantDiagnostic {
						t.Errorf("registered=%t verbose=%t: diagnostics = %q, want %q", registered, verbose, got, wantDiagnostic)
					}
					_, err = client.Me(context.Background())
					srv.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

type userAgentRecorder struct{ got string }

func (r *userAgentRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req.Header.Get("User-Agent")
	return nil, errors.New("recorded")
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
			rec := &userAgentRecorder{}
			res := runCLI(t, &runOpts{
				args:   []string{"--endpoint", "http://127.0.0.1:1", "--verbose", "list"},
				env:    map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CLIENT_PRODUCT": tc.value},
				egress: &egressGuard{report: t.Errorf, next: rec},
			})
			if rec.got != tc.wantUA {
				t.Errorf("User-Agent = %q, want %q", rec.got, tc.wantUA)
			}
			if got := strings.Contains(res.stderr.String(), "ignoring invalid QURL_CLIENT_PRODUCT"); got != tc.wantNote {
				t.Errorf("invalid-value note = %t, want %t; stderr %q", got, tc.wantNote, res.stderr.String())
			}
		})
	}
}
