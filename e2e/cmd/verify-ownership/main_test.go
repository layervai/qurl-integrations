package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"

	"github.com/layervai/qurl-go/qurl"

	"strings"
	"testing"
)

func TestVerifiedPublicOwnership(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	cell, _ := ecdh.X25519().GenerateKey(rand.Reader)
	signer, _ := qurl.NewLocalSigner(priv, "test-issuer")
	config := publicConfig{Issuers: map[string]string{"test-issuer": base64.RawURLEncoding.EncodeToString(der)}, CellKey: base64.StdEncoding.EncodeToString(cell.PublicKey().Bytes())}
	link, err := qurl.CreatePortalWithParams(context.Background(), signer, qurl.CreateParams{CellPublicKey: cell.PublicKey().Bytes(), ResourcePublicKey: der, RelayURL: "https://relay.example.com", JTI: "test-owned-qurl", IssuedAt: 1781910000, NotBefore: 1781910000, Expiry: 1781910300})
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifiedPublicIdentity(link, config)
	if err != nil {
		t.Fatal(err)
	}
	if got["cell_id"] != "" || got["cell_public_key_b64"] != config.CellKey || got["resource_public_key_b64"] != base64.RawURLEncoding.EncodeToString(der) {
		t.Fatal("public binding mismatch")
	}
	agent, err := base64.StdEncoding.DecodeString(got["agent_public_key"])
	if err != nil || len(agent) != 32 {
		t.Fatal("invalid native agent key")
	}
	if _, err := verifiedPublicIdentity(link+"tampered", config); err == nil {
		t.Fatal("tampered signature accepted")
	}
	mismatch := config
	mismatch.CellKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := verifiedPublicIdentity(link, mismatch); err == nil {
		t.Fatal("signed cell mismatch accepted")
	}
	if got["signed_jti"] != "test-owned-qurl" || got["signed_expiry_unix"] != "1781910300" {
		t.Fatal("signed claim identity missing")
	}
	if len(got) != 6 {
		t.Fatal("unexpected receipt fields")
	}
}

func TestPublicConfigShape(t *testing.T) {
	html := "const QURL_LINK_CONFIG = {\n        issuerTrustStore: {\"issuer\":\"public\"},\n        serverStaticPubB64: \"" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\",\n      };"
	if _, err := parsePublicConfig(html); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", html + html, strings.ReplaceAll(html, "issuerTrustStore:", "missing:"), strings.ReplaceAll(html, "{\"issuer\":\"public\"}", "null")} {
		if _, err := parsePublicConfig(bad); err == nil {
			t.Fatal("malformed public config accepted")
		}
	}
}

func TestIssuerPreflightRejectsInvalidKeys(t *testing.T) {
	for _, key := range []string{"not+raw/url=", base64.RawURLEncoding.EncodeToString([]byte("not DER"))} {
		if _, err := issuerTrust(publicConfig{Issuers: map[string]string{"issuer": key}}); err == nil {
			t.Fatal("invalid issuer passed preflight")
		}
	}
}
