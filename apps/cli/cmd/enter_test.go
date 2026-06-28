package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qv2"
)

// testIssuerKeyArg generates a fresh P-256 key and returns a "<kid>=<base64-DER>"
// --issuer-key flag value whose DER parses as a valid trust anchor. The key is
// unrelated to any link's signer, so the verify step still fails closed — the
// point is to exercise the CLI's trust-config wiring up to qurl-go's verify gate.
func testIssuerKeyArg(t *testing.T, kid string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal SPKI DER: %v", err)
	}
	return kid + "=" + base64.StdEncoding.EncodeToString(der)
}

// TestEnterCommand_NoTrustConfig_FailsClosed: with no --issuer-key/--relay the
// command takes the one-arg EnterPortal default-provider path, which fails closed
// with ErrNotConfigured while the qv2 admission path is undeployed. This pins the
// "wired but not yet live" contract.
func TestEnterCommand_NoTrustConfig_FailsClosed(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "enter", "https://qurl.link/#qv2.a.b.c")
	if err == nil {
		t.Fatal("expected fail-closed error with no trust config")
	}
	if !errors.Is(err, qurl.ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got: %v", err)
	}
}

// TestEnterCommand_InvalidIssuerKey exercises the CLI's --issuer-key parsing: a
// value missing the "<kid>=<der>" shape is rejected before any qurl-go call.
func TestEnterCommand_InvalidIssuerKey(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "enter", "https://qurl.link/#qv2.a.b.c",
		"--issuer-key", "no-equals-sign", "--relay", "relay.qurl.link")
	if err == nil {
		t.Fatal("expected error for malformed --issuer-key")
	}
	if !strings.Contains(err.Error(), "issuer-key") {
		t.Fatalf("expected issuer-key parse error, got: %v", err)
	}
}

// TestEnterCommand_InvalidIssuerKeyBase64 covers a kid with non-base64 key bytes.
func TestEnterCommand_InvalidIssuerKeyBase64(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "enter", "https://qurl.link/#qv2.a.b.c",
		"--issuer-key", "k1=!!!not-base64!!!", "--relay", "relay.qurl.link")
	if err == nil {
		t.Fatal("expected error for non-base64 --issuer-key")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Fatalf("expected base64 error, got: %v", err)
	}
}

// TestEnterCommand_WithTrustConfig_ReachesVerify: a valid issuer key + relay build
// a Static-provider Config and drive EnterPortalWith. The supplied link is
// malformed, so qurl-go's parser/verify rejects it — proving the trust-config path
// is wired all the way into qurl-go (not short-circuited and not the
// ErrNotConfigured default path).
func TestEnterCommand_WithTrustConfig_ReachesVerify(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "enter", "https://qurl.link/#qv2.a.b.c",
		"--issuer-key", testIssuerKeyArg(t, "k1"), "--relay", "relay.qurl.link")
	if err == nil {
		t.Fatal("expected error parsing/verifying malformed link")
	}
	// We must be PAST the fail-closed default path: with trust config supplied the
	// error is a qurl-go parse/verify failure, never ErrNotConfigured.
	if errors.Is(err, qurl.ErrNotConfigured) {
		t.Fatalf("trust config was not applied; got ErrNotConfigured: %v", err)
	}
}

// TestStaticTrustConfig_IssuerKeyOnly_RelayFailsClosed proves that supplying
// --issuer-key with no --relay yields an empty relay allowlist that rejects every
// relay_url at qv2 validation (fail closed), rather than admitting an attacker-
// chosen relay. We assert on the allowlist directly because a full link's relay
// gate only runs after signature verification.
func TestStaticTrustConfig_IssuerKeyOnly_RelayFailsClosed(t *testing.T) {
	cfg, err := staticTrustConfig([]string{testIssuerKeyArg(t, "k1")}, nil)
	if err != nil {
		t.Fatalf("staticTrustConfig: %v", err)
	}
	if cfg.RelayAllowlist == nil {
		t.Fatal("expected a non-nil (empty) relay allowlist")
	}
	// Any relay_url must be rejected by the empty allowlist.
	if err := qv2.ValidateRelayURL("https://relay.qurl.link/", cfg.RelayAllowlist); !errors.Is(err, qv2.ErrRelayURL) {
		t.Fatalf("expected ErrRelayURL for empty allowlist, got: %v", err)
	}
}
