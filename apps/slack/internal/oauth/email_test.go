package oauth

import "testing"

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "lowercases", raw: "Admin+Setup@Example.COM", want: "admin+setup@example.com"},
		{name: "trims", raw: " admin@example.com ", want: "admin@example.com"},
		{name: "empty", wantErr: true},
		{name: "missing at", raw: "admin", wantErr: true},
		{name: "display name rejected", raw: "Admin <admin@example.com>", wantErr: true},
		{name: "angle address rejected", raw: "<admin@example.com>", wantErr: true},
		{name: "pipe rejected", raw: "admin|evil@example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeEmail(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NormalizeEmail(%q) err=%v wantErr=%v", tc.raw, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
