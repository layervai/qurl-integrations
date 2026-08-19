package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

func releaseKey(t *testing.T) (keyB64, fingerprint string) {
	t.Helper()
	public, err := curve25519.X25519(bytes.Repeat([]byte{0x42}, curve25519.ScalarSize), curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(public), hub.FingerprintSHA256Hex(public)
}

func TestRunAcceptsMatchingProductionPin(t *testing.T) {
	key, fingerprint := releaseKey(t)
	var stdout, stderr bytes.Buffer
	exitCode := run(
		[]string{"production.sha256"},
		func(name string) (string, bool) {
			if name != hub.EnvServerPublicKey {
				t.Fatalf("lookup env %q", name)
			}
			return key, true
		},
		func(path string) ([]byte, error) {
			if path != "production.sha256" {
				t.Fatalf("read path %q", path)
			}
			return []byte("# raw X25519 key\n" + fingerprint + "\n"), nil
		},
		&stdout,
		&stderr,
	)
	if exitCode != 0 || stderr.Len() != 0 {
		t.Fatalf("run exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if got, want := stdout.String(), "verified production Hub public-key fingerprint "+fingerprint+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), key) {
		t.Fatal("stdout exposed the full Hub public key")
	}
}

func TestRunFailsClosed(t *testing.T) {
	key, fingerprint := releaseKey(t)
	otherKey, otherFingerprint := releaseKeyWithScalar(t, 0x24)
	tests := []struct {
		name        string
		args        []string
		candidate   string
		candidateOK bool
		contents    string
		readErr     error
		wantExit    int
		wantErr     string
		secret      string
	}{
		{name: "missing argument", candidate: key, candidateOK: true, wantExit: 2, wantErr: "usage:"},
		{name: "extra argument", args: []string{"one", "two"}, candidate: key, candidateOK: true, wantExit: 2, wantErr: "usage:"},
		{name: "unset repository variable", args: []string{"pin"}, wantExit: 1, wantErr: "repository variable must be non-empty"},
		{name: "empty repository variable", args: []string{"pin"}, candidateOK: true, wantExit: 1, wantErr: "repository variable must be non-empty"},
		{name: "malformed repository variable", args: []string{"pin"}, candidate: "%%%sensitive-malformed%%%", candidateOK: true, wantExit: 1, wantErr: "canonical padded standard base64", secret: "%%%sensitive-malformed%%%"},
		{name: "fingerprint read error", args: []string{"pin"}, candidate: key, candidateOK: true, readErr: errors.New("not found"), wantExit: 1, wantErr: "read production Hub fingerprint: not found", secret: key},
		{name: "comments-only fingerprint", args: []string{"pin"}, candidate: key, candidateOK: true, contents: "# pending\n", wantExit: 1, wantErr: "no fingerprint line found", secret: key},
		{name: "malformed fingerprint", args: []string{"pin"}, candidate: key, candidateOK: true, contents: "NOT-A-FINGERPRINT\n", wantExit: 1, wantErr: "lowercase hex SHA-256", secret: key},
		{name: "mismatched fingerprint", args: []string{"pin"}, candidate: otherKey, candidateOK: true, contents: fingerprint + "\n", wantExit: 1, wantErr: "fingerprint mismatch", secret: otherKey},
		{name: "matching alternate fixture sanity", args: []string{"pin"}, candidate: otherKey, candidateOK: true, contents: otherFingerprint + "\n", wantExit: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := run(
				tt.args,
				func(string) (string, bool) { return tt.candidate, tt.candidateOK },
				func(string) ([]byte, error) { return []byte(tt.contents), tt.readErr },
				&stdout,
				&stderr,
			)
			if exitCode != tt.wantExit {
				t.Fatalf("run exit = %d, want %d (stderr %q)", exitCode, tt.wantExit, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want containing %q", stderr.String(), tt.wantErr)
			}
			if tt.secret != "" && strings.Contains(stderr.String(), tt.secret) {
				t.Fatalf("stderr exposed the full Hub public key: %q", stderr.String())
			}
			if tt.wantExit != 0 && stdout.Len() != 0 {
				t.Fatalf("failure wrote stdout %q", stdout.String())
			}
		})
	}
}

func releaseKeyWithScalar(t *testing.T, scalarByte byte) (keyB64, fingerprint string) {
	t.Helper()
	public, err := curve25519.X25519(bytes.Repeat([]byte{scalarByte}, curve25519.ScalarSize), curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(public), hub.FingerprintSHA256Hex(public)
}
