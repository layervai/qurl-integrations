package auth

import "testing"

func TestIsAllowedOriginURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{"https://auth.example.com/authorize", true},
		{"https://auth.layerv.ai", true},
		{"http://127.0.0.1:8080/callback", true},
		{"http://127.0.0.1/callback", true},
		{"http://localhost:9090/callback", true},
		{"http://localhost/callback", true},

		// Reject: not https and not loopback
		{"http://evil.com", false},
		{"http://auth.example.com", false},
		// Reject: scheme confusion / injection
		{"javascript:alert(1)", false},
		{"file:///etc/passwd", false},
		// Reject: loopback-lookalike in hostname
		{"http://127.0.0.1.evil.example/oauth/token", false},
		{"http://localhost.attacker.tld/oauth/token", false},
		// Reject: userinfo in https URL
		{"https://user@evil.com/", false},
		// Reject: empty / invalid
		{"", false},
		{"not-a-url", false},
	}

	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := IsAllowedOriginURL(tc.url); got != tc.want {
				t.Errorf("IsAllowedOriginURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
