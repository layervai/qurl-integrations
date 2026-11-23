package internal

import (
	"errors"
	"testing"
)

func TestConnectorURLContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, endpoint, api string
		reason              ConnectorURLFailure
	}{
		{name: "https", endpoint: " https://example.test/// ", api: "https://example.test/v1"},
		{name: "localhost", endpoint: "http://LOCALHOST:8080", api: "http://LOCALHOST:8080/v1"},
		{name: "ipv4", endpoint: "http://127.0.0.1", api: "http://127.0.0.1/v1"},
		{name: "ipv6", endpoint: "http://[::1]:8080", api: "http://[::1]:8080/v1"},
		{name: "missing", reason: ConnectorURLMissing},
		{name: "slashes", endpoint: "////", reason: ConnectorURLOrigin},
		{name: "relative", endpoint: "example.test", reason: ConnectorURLInvalid},
		{name: "authority", endpoint: "https:///", reason: ConnectorURLInvalid},
		{name: "credentials", endpoint: "https://user:secret@example.test", reason: ConnectorURLCredentials},
		{name: "query", endpoint: "https://example.test?q=x", reason: ConnectorURLQuery},
		{name: "fragment", endpoint: "https://example.test#x", reason: ConnectorURLFragment},
		{name: "empty query", endpoint: "https://example.test?", reason: ConnectorURLInvalid},
		{name: "empty fragment", endpoint: "https://example.test#", reason: ConnectorURLInvalid},
		{name: "version", endpoint: "https://example.test/V1/", reason: ConnectorURLVersion},
		{name: "escaped version", endpoint: "https://example.test/%76%31", reason: ConnectorURLVersion},
		{name: "path", endpoint: "https://example.test/private/v1", reason: ConnectorURLPath},
		{name: "remote http", endpoint: "http://example.test", reason: ConnectorURLInvalid},
		{name: "loopback impostor", endpoint: "http://localhost.evil.test", reason: ConnectorURLInvalid},
		{name: "scheme", endpoint: "ftp://example.test", reason: ConnectorURLInvalid},
		{name: "bad port", endpoint: "https://example.test:bad", reason: ConnectorURLInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			endpoint, api, err := ParseConnectorEndpoint(tc.endpoint)
			if tc.reason != connectorURLValid {
				if !errors.Is(err, tc.reason) || endpoint != "" || api != "" {
					t.Fatalf("got %q, %q, %v; want reason %d", endpoint, api, err, tc.reason)
				}
				return
			}
			if err != nil || api != tc.api || endpoint+"/v1" != api {
				t.Fatalf("got %q, %q, %v; want %q", endpoint, api, err, tc.api)
			}
			if err := ValidateConnectorAPIURL(api); err != nil {
				t.Fatalf("validated origin produced invalid API URL: %v", err)
			}
		})
	}
}

func TestConnectorRenderedURLContract(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"https://example.test/v1", "https://example.test/v1///", "https://example.test/%76%31", "https://example.test/v1?", "http://[::1]:8080/v1"} {
		if err := ValidateConnectorAPIURL(raw); err != nil {
			t.Errorf("%q: %v", raw, err)
		}
	}
	for _, raw := range []string{"example.test/v1", "https://example.test", "https://example.test/V1", "https://example.test/v1#", "https://example.test/v1#x", "https://example.test/v1?q=x", "https://user:secret@example.test/v1", "http://example.test/v1", "ftp://example.test/v1", "https://example.test/private/v1"} {
		if err := ValidateConnectorAPIURL(raw); !errors.Is(err, errConnectorAPIURLInvalid) {
			t.Errorf("%q: got %v, want original invalid sentinel", raw, err)
		}
	}
	if err := ValidateConnectorAPIURL(" "); !errors.Is(err, errConnectorAPIURLMissing) {
		t.Errorf("missing: got %v, want original missing sentinel", err)
	}
}
