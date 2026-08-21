package nethost

import "testing"

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "localhost", want: true},
		{host: "127.0.0.1", want: true},
		{host: "127.5.6.7", want: true},
		{host: "::1", want: true},
		// The trim and lower are why the internal inspect and tunnel call sites could
		// drop their own normalization; a copy without them answered false here.
		{host: "  LocalHost  ", want: true},
		{host: "LOCALHOST", want: true},
		{host: "  127.0.0.1 ", want: true},
		// url.URL.Hostname can hand back a host carrying Unicode whitespace from a URL
		// that parsed cleanly; TrimSpace strips it, which the doc comment claims.
		{host: "localhost\u00a0", want: true},
		{host: "\u00a0localhost", want: true},
		{host: "", want: false},
		{host: "slack.test", want: false},
		{host: "localhost.evil.test", want: false},
		{host: "localhost\u00a0.evil.test", want: false},
		{host: "8.8.8.8", want: false},
		{host: "0.0.0.0", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := IsLoopback(tc.host); got != tc.want {
				t.Fatalf("IsLoopback(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
