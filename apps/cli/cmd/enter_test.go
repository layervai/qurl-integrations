package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qv2"
)

// enterForbiddenWords is the customer-surface jargon contract for `qurl enter`:
// none of these may appear (case-insensitive) in Short / Long / Example, any
// NON-hidden flag usage string, or any returned error's user-facing text. The
// entries are pre-lowercased so substring checks against a lowercased haystack
// actually match (a verbatim-cased needle like "DER" would never hit a lowercased
// haystack and silently pass). Shared by the help and error jargon tests.
var enterForbiddenWords = []string{
	"qv2",
	"relay",
	"resolve",
	"trust",
	"issuer",
	"signature",
	"admission",
	"proof-of-possession",
	"proof of possession",
	"knock",
	"fail closed",
	"fails closed",
	"errnotconfigured",
	"at_",
	"access token",
	"device key",
	"spki",
	"der",
	"base64",
	"allowlist",
	"nhp",
	"serverid",
	"cell",
}

// runEnterErr executes `qurl enter` with the given args and returns the error.
// `enter` is a pure client-side command (no qURL API call), so unlike runCmdErr
// it needs no mock server or --endpoint.
func runEnterErr(t *testing.T, args ...string) error {
	t.Helper()
	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"enter"}, args...))
	return cmd.Execute()
}

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
// while the underlying admission path is undeployed. This pins the "wired but not
// yet live" contract — the sentinel is still reachable via Unwrap, but the message
// surfaced to the customer is the friendly, jargon-free string.
func TestEnterCommand_NoTrustConfig_FailsClosed(t *testing.T) {
	err := runEnterErr(t, "https://qurl.link/#qv2.a.b.c")
	if err == nil {
		t.Fatal("expected fail-closed error with no trust config")
	}
	// The sentinel remains reachable through enterError.Unwrap.
	if !errors.Is(err, qurl.ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured (via Unwrap), got: %v", err)
	}
	// ...but the customer-facing text must be the friendly string, not raw jargon.
	if err.Error() != enterMsgNotConfigured {
		t.Fatalf("expected friendly not-configured message, got: %q", err.Error())
	}
	lower := strings.ToLower(err.Error())
	for _, bad := range []string{"errnotconfigured", "trust", "relay"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("friendly error leaked jargon %q: %q", bad, err.Error())
		}
	}
}

// TestEnterCommand_InvalidIssuerKey exercises the CLI's --issuer-key parsing: a
// value missing the "<kid>=<der>" shape is rejected before any qurl-go call.
func TestEnterCommand_InvalidIssuerKey(t *testing.T) {
	err := runEnterErr(t, "https://qurl.link/#qv2.a.b.c",
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
	err := runEnterErr(t, "https://qurl.link/#qv2.a.b.c",
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
	err := runEnterErr(t, "https://qurl.link/#qv2.a.b.c",
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

// TestStaticTrustConfig_RelayOnly_RequiresIssuerKey: supplying --relay without any
// --issuer-key is rejected (a relay allowlist with no trust anchors can verify
// nothing).
func TestStaticTrustConfig_RelayOnly_RequiresIssuerKey(t *testing.T) {
	_, err := staticTrustConfig(nil, []string{"relay.qurl.link"})
	if err == nil {
		t.Fatal("expected error for relay without issuer key")
	}
	if !strings.Contains(err.Error(), "at least one --issuer-key is required") {
		t.Fatalf("expected issuer-key-required error, got: %v", err)
	}
}

// TestStaticTrustConfig_DuplicateKid: two --issuer-key values sharing a kid are
// rejected rather than silently overwriting one anchor with the other.
func TestStaticTrustConfig_DuplicateKid(t *testing.T) {
	k1 := testIssuerKeyArg(t, "k1")
	k1Again := testIssuerKeyArg(t, "k1") // same kid, different key bytes
	_, err := staticTrustConfig([]string{k1, k1Again}, []string{"relay.qurl.link"})
	if err == nil {
		t.Fatal("expected error for duplicate kid")
	}
	if !strings.Contains(err.Error(), "duplicate --issuer-key for kid") {
		t.Fatalf("expected duplicate-kid error, got: %v", err)
	}
}

// TestStaticTrustConfig_EmptyRelay: an empty/whitespace --relay entry is rejected
// with a clear error rather than being silently dropped by the allowlist builder.
func TestStaticTrustConfig_EmptyRelay(t *testing.T) {
	_, err := staticTrustConfig([]string{testIssuerKeyArg(t, "k1")}, []string{"   "})
	if err == nil {
		t.Fatal("expected error for empty --relay entry")
	}
	if !strings.Contains(err.Error(), "invalid --relay") {
		t.Fatalf("expected invalid-relay error, got: %v", err)
	}
}

// findEnterCmd locates the `enter` subcommand off the real root command so the
// test sees the exact help copy and flag wiring (including MarkHidden) a customer
// would get.
func findEnterCmd(t *testing.T) *cobra.Command {
	t.Helper()
	root := rootCmd("test")
	for _, c := range root.Commands() {
		if c.Name() == "enter" {
			return c
		}
	}
	t.Fatal("enter subcommand not found on root command")
	return nil
}

// TestEnterCommand_NoJargonInHelp asserts the customer-visible help surface
// (Short, Long, Example, and every NON-hidden flag's usage) contains none of the
// forbidden jargon words. Hidden flags (--issuer-key/--relay) are excluded, which
// also proves MarkHidden took effect — if it hadn't, their usage strings carry
// "trust"/"issuer"/"relay"/"base64"/"DER" and this test would fail.
func TestEnterCommand_NoJargonInHelp(t *testing.T) {
	cmd := findEnterCmd(t)

	var parts []string
	parts = append(parts, cmd.Short, cmd.Long, cmd.Example)
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		parts = append(parts, f.Usage)
	})

	haystack := strings.ToLower(strings.Join(parts, "\n"))
	for _, bad := range enterForbiddenWords {
		if strings.Contains(haystack, bad) {
			t.Errorf("customer help surface leaked jargon %q\nhelp text:\n%s", bad, haystack)
		}
	}
}

// TestEnterCommand_NoJargonInErrors runs the no-flags (customer) path against a
// qv2-shaped link and a clearly-malformed input, and asserts each surfaced error
// is one of the friendly messages and contains none of the forbidden jargon words.
func TestEnterCommand_NoJargonInErrors(t *testing.T) {
	friendly := map[string]bool{
		enterMsgNotConfigured: true,
		enterMsgOverloaded:    true,
		enterMsgGeneric:       true,
	}

	for _, input := range []string{
		"https://qurl.link/#qv2.a.b.c",
		"not-a-real-qurl-link",
	} {
		err := runEnterErr(t, input)
		if err == nil {
			t.Fatalf("expected an error for input %q", input)
		}
		msg := err.Error()
		if !friendly[msg] {
			t.Errorf("input %q: error %q is not one of the friendly messages", input, msg)
		}
		lower := strings.ToLower(msg)
		for _, bad := range enterForbiddenWords {
			if strings.Contains(lower, bad) {
				t.Errorf("input %q: error leaked jargon %q: %q", input, bad, msg)
			}
		}
	}
}
