package main

import (
	"bytes"
	"context"
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
		{name: "token punctuation", value: "AZaz09!#$%&'*+-.^_`|~/!#$%&'*+-.^_`|~", product: "AZaz09!#$%&'*+-.^_`|~/!#$%&'*+-.^_`|~", set: true},
		{name: "64 bytes", value: strings.Repeat("a", 62) + "/1", product: strings.Repeat("a", 62) + "/1", set: true},
		{name: "65 bytes", value: strings.Repeat("a", 63) + "/1", set: true},
		{name: "missing slash", value: "desktop", set: true},
		{name: "empty name", value: "/1", set: true},
		{name: "empty version", value: "desktop/", set: true},
		{name: "extra slash", value: "desktop/1/2", set: true},
		{name: "leading space", value: " desktop/1", set: true},
		{name: "trailing space", value: "desktop/1 ", set: true},
		{name: "multiple products", value: "desktop/1 other/2", set: true},
		{name: "tab", value: "desktop/1\t", set: true},
		{name: "CR", value: "desktop/1\r", set: true},
		{name: "LF", value: "desktop/1\n", set: true},
		{name: "CRLF injection", value: "desktop/1\r\nInjected: yes", set: true},
		{name: "control", value: "desktop/\x00", set: true},
		{name: "DEL", value: "desktop/\x7f", set: true},
		{name: "unicode name", value: "désktop/1", set: true},
		{name: "unicode version", value: "desktop/é", set: true},
		{name: "comment", value: "desktop/1(comment)", set: true},
		{name: "separator", value: "desktop/1:2", set: true},
		{name: "quoted", value: "desktop/\"1\"", set: true},
		{name: "backslash", value: "desktop/1\\2", set: true},
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
