package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func TestClassifyPublishTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		target    string
		kind      publishTargetKind
		ip        string
		port      int
		canonical string
		wantErr   string
	}{
		{name: "ipv4", target: "http://127.0.0.1:3000", kind: publishTargetLocal, ip: "127.0.0.1", port: 3000, canonical: "http://127.0.0.1:3000"},
		{name: "ipv4 loopback range", target: "http://127.1.2.3:8080/", kind: publishTargetLocal, ip: "127.1.2.3", port: 8080, canonical: "http://127.1.2.3:8080"},
		{name: "localhost normalized", target: "http://LOCALHOST:3000/", kind: publishTargetLocal, ip: "127.0.0.1", port: 3000, canonical: "http://127.0.0.1:3000"},
		{name: "localhost default port", target: "http://localhost", kind: publishTargetLocal, ip: "127.0.0.1", port: 80, canonical: "http://127.0.0.1:80"},
		{name: "ipv6", target: "http://[::1]:3000", kind: publishTargetLocal, ip: "::1", port: 3000, canonical: "http://[::1]:3000"},
		{name: "remote https", target: "https://example.com/path?q=1", kind: publishTargetRemote},
		{name: "private ip stays remote", target: "http://192.168.1.2:3000", kind: publishTargetRemote},
		{name: "local https", target: "https://localhost:3000", wantErr: "cleartext http"},
		{name: "local path", target: "http://localhost:3000/api", wantErr: "without a path"},
		{name: "local query", target: "http://localhost:3000?x=1", wantErr: "without a query"},
		{name: "empty local query", target: "http://localhost:3000?", wantErr: "without a query"},
		{name: "local fragment", target: "http://localhost:3000#x", wantErr: "without a fragment"},
		{name: "empty local fragment", target: "http://localhost:3000#", wantErr: "without a fragment"},
		{name: "credentials", target: "http://me:secret@localhost:3000", wantErr: "credentials"},
		{name: "localhost dot", target: "http://localhost.:3000", wantErr: "not a supported loopback"},
		{name: "localhost subdomain", target: "http://app.localhost:3000", wantErr: "not a supported loopback"},
		{name: "wildcard ipv4", target: "http://0.0.0.0:3000", wantErr: "not a supported loopback"},
		{name: "wildcard ipv6", target: "http://[::]:3000", wantErr: "not a supported loopback"},
		{name: "malformed loopback", target: "http://127.nope:3000", wantErr: "not a supported loopback"},
		{name: "missing scheme", target: "localhost:3000", wantErr: "http or https"},
		{name: "empty ipv4 port", target: "http://localhost:", wantErr: "must not be empty"},
		{name: "empty ipv6 port", target: "http://[::1]:", wantErr: "must not be empty"},
		{name: "bad port", target: "http://localhost:70000", wantErr: "between 1 and 65535"},
		{name: "empty", target: "  ", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := classifyPublishTarget(test.target)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("classifyPublishTarget(%q) error = %v, want containing %q", test.target, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyPublishTarget(%q): %v", test.target, err)
			}
			if got.kind != test.kind || got.localIP != test.ip || got.localPort != test.port || got.canonicalOrigin != test.canonical || got.original != test.target {
				t.Fatalf("classifyPublishTarget(%q) = %#v", test.target, got)
			}
		})
	}
}

func TestGeneratedLocalConnectorID(t *testing.T) {
	t.Parallel()
	first, err := generatedLocalConnectorID("agent-a", "http://127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := generatedLocalConnectorID("agent-a", "http://127.0.0.1:3000")
	otherAgent, _ := generatedLocalConnectorID("agent-b", "http://127.0.0.1:3000")
	otherOrigin, _ := generatedLocalConnectorID("agent-a", "http://127.0.0.1:4000")
	if first != again || first == otherAgent || first == otherOrigin {
		t.Fatalf("derived IDs: first=%q again=%q otherAgent=%q otherOrigin=%q", first, again, otherAgent, otherOrigin)
	}
	if !strings.HasPrefix(first, "local-") || len(first) != len("local-")+16 {
		t.Fatalf("generated ID %q has unexpected shape", first)
	}
	if err := validateConnectorID(first); err != nil {
		t.Fatalf("generated ID invalid: %v", err)
	}
}

func TestLocalEnrollmentIdempotencyKey(t *testing.T) {
	t.Parallel()
	entropy := make([]byte, localEnrollmentEntropyBytes)
	entropy[0] = 1
	otherEntropy := append([]byte(nil), entropy...)
	otherEntropy[0] = 2
	first, err := localEnrollmentIdempotencyKey("agent-a", "local-a234567890123456", entropy)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := localEnrollmentIdempotencyKey("agent-a", "local-a234567890123456", entropy)
	otherID, _ := localEnrollmentIdempotencyKey("agent-a", "local-b234567890123456", entropy)
	otherAttempt, _ := localEnrollmentIdempotencyKey("agent-a", "local-a234567890123456", otherEntropy)
	if first != again || first == otherID || first == otherAttempt || len(first) < 32 || len(first) > 256 {
		t.Fatalf("idempotency keys first=%q again=%q otherID=%q otherAttempt=%q", first, again, otherID, otherAttempt)
	}
	if _, err := localEnrollmentIdempotencyKey("agent-a", "local-a234567890123456", entropy[:len(entropy)-1]); err == nil {
		t.Fatal("short enrollment entropy was accepted")
	}
}

func TestValidateConnectorID(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"abc", "a-b", "local-a234567890123456", "a12"} {
		if err := validateConnectorID(valid); err != nil {
			t.Errorf("validateConnectorID(%q): %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "ab", "A-b", "1ab", "ab-", "a_b", "a" + strings.Repeat("b", 64)} {
		err := validateConnectorID(invalid)
		if err == nil {
			t.Errorf("validateConnectorID(%q) succeeded", invalid)
			continue
		}
		want := fmt.Sprintf("invalid Connector ID %q: use 3-64 lowercase letters, numbers, or hyphens; start with a letter and end with a letter or number", invalid)
		if err.Error() != want {
			t.Errorf("validateConnectorID(%q) = %q, want %q", invalid, err, want)
		}
		if got := exitcode.FromError(err); got != exitcode.Usage {
			t.Errorf("validateConnectorID(%q) exit code = %d, want %d", invalid, got, exitcode.Usage)
		}
	}
}
