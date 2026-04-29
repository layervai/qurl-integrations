package main

import "testing"

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https valid", "https://example.com", false},
		{"http valid", "http://example.com", false},
		{"https with path", "https://example.com/data?q=1", false},
		{"no scheme", "example.com", true},
		{"ftp scheme", "ftp://example.com", true},
		{"empty", "", true},
		{"just scheme", "https://", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateDuration(t *testing.T) {
	tests := []struct {
		name    string
		d       string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"seconds", "30s", false},
		{"minutes", "45m", false},
		{"hours", "24h", false},
		{"days", "7d", false},
		{"invalid suffix", "24x", true},
		{"no number", "h", true},
		{"freeform", "forever", true},
		{"float", "1.5h", true},
		{"negative", "-1h", true},
		{"zero hours", "0h", true},
		{"multi-digit zero", "00h", true},
		{"leading zero", "007d", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDuration(tt.d)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDuration(%q) error = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
		})
	}
}

func TestValidateResourceID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid", "r_abc123", false},
		{"valid long", "r_k8xqp9h2sj9lx7r4a", false},
		{"wrong prefix", "x_abc", true},
		{"no prefix", "abc123", true},
		{"too short", "r_a", true},
		{"just prefix", "r_", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResourceID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateResourceID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestValidateAccessToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"valid", "at_abc123", false},
		{"valid long", "at_k8xqp9h2sj9lx7r4a", false},
		{"wrong prefix", "tok_abc", true},
		{"no prefix", "abc123", true},
		{"too short", "at_ab", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccessToken(tt.token)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateAccessToken(%q) error = %v, wantErr %v", tt.token, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRFC3339(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"valid UTC", "2026-01-01T00:00:00Z", false},
		{"valid with offset", "2026-06-15T12:30:00+05:30", false},
		{"date only", "2026-01-01", true},
		{"invalid format", "Jan 1 2026", true},
		{"garbage", "not-a-date", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRFC3339("test-flag", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRFC3339(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}
