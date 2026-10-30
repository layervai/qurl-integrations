package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
