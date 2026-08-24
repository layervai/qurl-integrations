//go:build linux && !android

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

type privateFixture struct {
	Schema int    `json:"schema"`
	Value  string `json:"value"`
}

func TestCanonicalPrivateJSONRoundTripAndMetadata(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directories require owner traversal.
		t.Fatal(err)
	}
	path := filepath.Join(directory, "report.json")
	want := privateFixture{Schema: 1, Value: "sandbox"}
	if err := writeCanonicalPrivateJSON(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("report metadata = %#v %#v", info, stat)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != `{"schema":1,"value":"sandbox"}`+"\n" { //nolint:gosec // Test reads its own fixed temporary path.
		t.Fatalf("report bytes = %q %v", raw, err)
	}
	var got privateFixture
	if err := readCanonicalPrivateJSON(path, &got); err != nil || got != want {
		t.Fatalf("round trip = %#v %v", got, err)
	}
	if err := writeCanonicalPrivateJSON(path, want); err != nil {
		t.Fatalf("exact report replay = %v", err)
	}
	if err := writeCanonicalPrivateJSON(path, privateFixture{Schema: 1, Value: "drift"}); err == nil {
		t.Fatal("conflicting report was overwritten")
	}
}

func TestReadCanonicalPrivateJSONRejectsAuthorityMutations(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directories require owner traversal.
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		raw  string
		mode os.FileMode
	}{
		{"group-readable", `{"schema":1,"value":"sandbox"}` + "\n", 0o440},
		{"missing LF", `{"schema":1,"value":"sandbox"}`, 0o600},
		{"double LF", `{"schema":1,"value":"sandbox"}` + "\n\n", 0o600},
		{"CRLF", `{"schema":1,"value":"sandbox"}` + "\r\n", 0o600},
		{"noncanonical whitespace", `{ "schema":1,"value":"sandbox"}` + "\n", 0o600},
		{"duplicate", `{"schema":1,"schema":1,"value":"sandbox"}` + "\n", 0o600},
		{"unknown", `{"schema":1,"value":"sandbox","extra":true}` + "\n", 0o600},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			path := filepath.Join(directory, mutation.name+".json")
			if err := os.WriteFile(path, []byte(mutation.raw), mutation.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mutation.mode); err != nil {
				t.Fatal(err)
			}
			var value privateFixture
			if err := readCanonicalPrivateJSON(path, &value); err == nil {
				t.Fatal("mutated private JSON accepted")
			}
		})
	}
}

func TestRunFailsBeforeAuthorityOnParserAndOutputDrift(t *testing.T) {
	if err := run(context.Background(), nil); err == nil {
		t.Fatal("empty command accepted")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directories require owner traversal.
		t.Fatal(err)
	}
	input := filepath.Join(directory, "input.json")
	apiKey := filepath.Join(directory, "api-key")
	report := filepath.Join(directory, "report.json")
	if err := os.WriteFile(input, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("occupied\n"), 0o440); err != nil { //nolint:gosec // Fixture proves group-readable input is rejected.
		t.Fatal(err)
	}
	if err := os.Chmod(report, 0o440); err != nil { //nolint:gosec // Fixture proves group-readable output is rejected.
		t.Fatal(err)
	}
	if err := os.WriteFile(apiKey, []byte("lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"provision", "--authority-socket", filepath.Join(directory, "missing.sock"),
		"--invocation-token", string(bytes.Repeat([]byte{'a'}, 64)), "--input-file", input, "--api-key-file", apiKey, "--report-file", report})
	if err == nil || err.Error() != "existing report is not owner-only" {
		t.Fatalf("occupied report error = %v", err)
	}
}

func TestReadExactAPIKeyRequiresOwnerOnlyExactLine(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Private command fixture must be owner-only and searchable.
		t.Fatal(err)
	}
	valid := "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	for name, raw := range map[string]string{
		"valid": valid + "\n", "missing-lf": valid, "double-lf": valid + "\n\n", "crlf": valid + "\r\n",
		"leading-space": " " + valid + "\n", "trailing-space": valid + " \n", "tab": valid + "\t\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readExactAPIKey(path)
			if name == "valid" {
				if err != nil || string(got) != valid {
					t.Fatalf("valid key = %q %v", got, err)
				}
				clear(got)
				return
			}
			if err == nil {
				t.Fatalf("mutated key accepted: %q", got)
			}
		})
	}
}

func TestEnrollmentCredentialIdentityIsExact(t *testing.T) {
	expectedOwner := "sandbox-sharing-owner@clients"
	base := qurlapi.Identity{OwnerID: expectedOwner, AuthType: "api_key", Key: &qurlapi.KeyIdentity{
		KeyID: "key_AbCdEf123456", Kind: "api_key", Scopes: []string{"qurl:agent", "qurl:read", "qurl:write"}, KeyPrefix: "lv_test_abcd"}}
	client := &fakeIdentityAPI{identity: &base}
	authority := fileEnrollmentAuthority{apiKey: "not-returned", keyPrefix: "lv_test_abcd", identityAPI: client}
	receipt, err := authority.VerifyEnrollmentCredential(context.Background(), expectedOwner)
	if err != nil || receipt.OwnerID != expectedOwner || receipt.KeyID != base.Key.KeyID || receipt.KeyPrefix != base.Key.KeyPrefix || client.calls != 1 {
		t.Fatalf("credential receipt = %#v calls=%d err=%v", receipt, client.calls, err)
	}
	mutations := []struct {
		name string
		edit func(*qurlapi.Identity)
	}{
		{"owner", func(identity *qurlapi.Identity) { identity.OwnerID = "other-owner@clients" }},
		{"auth type", func(identity *qurlapi.Identity) { identity.AuthType = "m2m" }},
		{"missing key", func(identity *qurlapi.Identity) { identity.Key = nil }},
		{"kind", func(identity *qurlapi.Identity) { identity.Key.Kind = "device" }},
		{"scope order", func(identity *qurlapi.Identity) {
			identity.Key.Scopes[0], identity.Key.Scopes[1] = identity.Key.Scopes[1], identity.Key.Scopes[0]
		}},
		{"prefix", func(identity *qurlapi.Identity) { identity.Key.KeyPrefix = "lv_test_wxyz" }},
		{"expiry", func(identity *qurlapi.Identity) {
			expiry := time.Unix(2_000_000_000, 0).UTC()
			identity.Key.ExpiresAt = &expiry
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			identity := base
			key := *base.Key
			key.Scopes = append([]string(nil), base.Key.Scopes...)
			identity.Key = &key
			mutation.edit(&identity)
			_, err := (fileEnrollmentAuthority{keyPrefix: "lv_test_abcd", identityAPI: &fakeIdentityAPI{identity: &identity}}).
				VerifyEnrollmentCredential(context.Background(), expectedOwner)
			if err == nil {
				t.Fatal("mutated credential identity accepted")
			}
		})
	}
}

func TestWriteAllHandlesShortAndZeroWrites(t *testing.T) {
	short := &commandShortWriter{maximum: 2}
	if err := writeAll(short, []byte("abcdef")); err != nil || string(short.value) != "abcdef" {
		t.Fatalf("short write = %q %v", short.value, err)
	}
	zero := &commandShortWriter{zero: true}
	if err := writeAll(zero, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write = %v", err)
	}
}

type commandShortWriter struct {
	maximum int
	zero    bool
	value   []byte
}

type fakeIdentityAPI struct {
	identity *qurlapi.Identity
	err      error
	calls    int
}

func (f *fakeIdentityAPI) Me(context.Context) (*qurlapi.Identity, error) {
	f.calls++
	return f.identity, f.err
}

func (w *commandShortWriter) Write(raw []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	count := min(w.maximum, len(raw))
	w.value = append(w.value, raw[:count]...)
	return count, nil
}
