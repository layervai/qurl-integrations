package consume

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

// All tests here are offline on purpose: an AccessOpener refuses before any
// network I/O on every path this file exercises — configuration faults, and
// links that fail their local check under a configured deployment. The one
// networked path (a valid link under valid settings) is covered by the cmd
// harness seam hermetically and by the clisandbox journey live.

func TestNeedsAccessGrant(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		link string
		want bool
	}{
		"fragment credential":          {"https://qurl.link/#qv2.claims.secret.sig", true},
		"fragment credential w/ path":  {"https://qurl.link/portal#qv2.a.b.c", true},
		"plain URL":                    {"https://example.com/file.bin", false},
		"page anchor":                  {"https://example.com/doc#section-2", false},
		"bare qv2 anchor, no parts":    {"https://qurl.link/#qv2", false},
		"credential-like query":        {"https://qurl.link/?f=qv2.a.b.c", false},
		"unparseable":                  {"\x00https://qurl.link/#qv2.a.b.c", false},
		"empty":                        {"", false},
		"scheme-relative no fragment":  {"//qurl.link/x", false},
		"fragment prefix must be qv2.": {"https://qurl.link/#qv3.a.b.c", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := NeedsAccessGrant(tc.link); got != tc.want {
				t.Errorf("NeedsAccessGrant(%q) = %v, want %v", tc.link, got, tc.want)
			}
		})
	}
}

// portalLink is a shape-valid fragment-credential link for offline tests;
// its parts are not real credentials, so a configured opener discards it at
// the local check — before any network I/O.
const portalLink = "https://qurl.link/#qv2.claims.secret.sig"

// testDeploymentJSON writes a syntactically valid deployment settings file
// carrying one freshly generated key under kid, returning its path.
func testDeploymentJSON(t *testing.T, kid string) string {
	t.Helper()
	signer, err := qurl.GenerateLocalSigner(kid)
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	der, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatalf("public key DER: %v", err)
	}
	return writeDeployment(t, `{
  "issuers": [{"kid": "`+kid+`", "spki_der_b64": "`+base64.RawURLEncoding.EncodeToString(der)+`"}],
  "cells": [],
  "relay_allowlist": ["qurl.link"]
}`)
}

func writeDeployment(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write deployment fixture: %v", err)
	}
	return path
}

// envMap builds a LookupEnv over a fixed map, so no test reads the process
// environment.
func envMap(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// TestOpenWithoutSettingsRefusesConfigured pins the fail-closed default:
// with no QURL_DEPLOYMENT in the injected environment and the SDK's shipped
// deployment empty, Open refuses with the configuration sentinel before any
// network I/O. The process variable is cleared so a developer's real
// QURL_DEPLOYMENT can never leak into the hermetic run through the SDK's
// own fallback.
func TestOpenWithoutSettingsRefusesConfigured(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	opener := &AccessOpener{LookupEnv: envMap(nil)}
	_, err := opener.Open(context.Background(), portalLink)
	if !errors.Is(err, ErrAccessNotConfigured) {
		t.Fatalf("err = %v, want ErrAccessNotConfigured", err)
	}
}

// TestOpenClassifiesSettingsFaults pins the configuration family: an
// unreadable, malformed, or incomplete settings file refuses with
// ErrAccessNotConfigured, never a raw SDK error.
func TestOpenClassifiesSettingsFaults(t *testing.T) {
	t.Parallel()
	signer, err := qurl.GenerateLocalSigner("kid-settings-faults")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	der, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatalf("public key DER: %v", err)
	}
	goodKey := base64.RawURLEncoding.EncodeToString(der)
	cases := map[string]string{
		"missing file": filepath.Join(t.TempDir(), "absent.json"),
		"not JSON":     writeDeployment(t, "not json"),
		"unknown field": writeDeployment(t,
			`{"issuers": [], "cells": [], "relay_allowlist": [], "surprise": true}`),
		"no issuers": writeDeployment(t,
			`{"issuers": [], "cells": [], "relay_allowlist": ["qurl.link"]}`),
		"bad key encoding": writeDeployment(t,
			`{"issuers": [{"kid": "k1", "spki_der_b64": "!!!"}], "cells": [], "relay_allowlist": ["qurl.link"]}`),
		"unusable key": writeDeployment(t,
			`{"issuers": [{"kid": "k1", "spki_der_b64": "aGk"}], "cells": [], "relay_allowlist": ["qurl.link"]}`),
		"no transport": writeDeployment(t,
			`{"issuers": [{"kid": "k1", "spki_der_b64": "`+goodKey+`"}], "cells": [], "relay_allowlist": []}`),
		"blank-only endpoints": writeDeployment(t,
			`{"issuers": [{"kid": "k1", "spki_der_b64": "`+goodKey+`"}], "cells": [], "relay_allowlist": ["", "  "]}`),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opener := &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: path})}
			_, err := opener.Open(context.Background(), portalLink)
			if !errors.Is(err, ErrAccessNotConfigured) {
				t.Fatalf("%s: err = %v, want ErrAccessNotConfigured", name, err)
			}
		})
	}
}

// TestOpenDiscardsLinkFailingLocalCheck pins the verification family: under
// valid settings, a link whose credential parts don't decode is discarded
// with the fail-closed verification sentinel — offline, nothing fetched.
func TestOpenDiscardsLinkFailingLocalCheck(t *testing.T) {
	path := testDeploymentJSON(t, "kid-local-check")
	opener := &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: path})}
	for name, link := range map[string]string{
		"undecodable parts": portalLink,
		"wrong part count":  "https://qurl.link/#qv2.onlyone",
	} {
		if _, err := opener.Open(context.Background(), link); !errors.Is(err, ErrLinkVerification) {
			t.Errorf("%s: err = %v, want ErrLinkVerification", name, err)
		}
	}
}

// TestClassifyAccessError pins the full SDK-fault mapping, including the
// pass-through default for faults outside the access taxonomy.
func TestClassifyAccessError(t *testing.T) {
	t.Parallel()
	passthrough := errors.New("a transport fault")
	cases := map[string]struct {
		in   error
		want error
	}{
		"already classified":  {ErrAccessNotConfigured, ErrAccessNotConfigured},
		"sdk not configured":  {qurl.ErrNotConfigured, ErrAccessNotConfigured},
		"unknown kid":         {qurl.ErrUnknownKID, ErrAccessSettingsMismatch},
		"disallowed endpoint": {qurl.ErrRelayURL, ErrAccessSettingsMismatch},
		"bad signature":       {qurl.ErrSignature, ErrLinkVerification},
		"strict parse":        {qurl.ErrStrictParse, ErrLinkVerification},
		"bad shape":           {qurl.ErrFragment, ErrLinkVerification},
		"bad encoding":        {qurl.ErrEncoding, ErrLinkVerification},
		"bad key length":      {qurl.ErrKeyLength, ErrLinkVerification},
		"platform deny":       {&qurl.ServerDenyError{ErrCode: "7"}, ErrAccessDenied},
		"platform busy":       {qurl.ErrServerOverloaded, ErrAccessBusy},
		"context canceled":    {context.Canceled, context.Canceled},
		"other":               {passthrough, passthrough},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := classifyAccessError(tc.in)
			if !errors.Is(got, tc.want) {
				t.Errorf("classifyAccessError(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
