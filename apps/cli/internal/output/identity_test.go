package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
)

func fixtureIdentity() *qurlapi.Identity {
	return &qurlapi.Identity{
		OwnerID:  "own_output_test",
		AuthType: "api_key",
		Key: &qurlapi.KeyIdentity{
			KeyID:     "key_outputtest01",
			Kind:      "api_key",
			Scopes:    []string{"qurl:read", "qurl:write"},
			KeyPrefix: "lv_test_outp",
		},
	}
}

// TestWhoAmIProjections pins the whoami stream discipline: every projection
// is stdout-only data; --quiet is the bare owner id; the JSON document is
// the repo-owned shape.
func TestWhoAmIProjections(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.WhoAmI(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"own_output_test", "key_outputtest01 (lv_test_outp)", "qurl:read, qurl:write", "never"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("text projection missing %q:\n%s", want, out.String())
			}
		}
		if errBuf.Len() != 0 {
			t.Errorf("whoami text is data; stderr must stay empty, got %q", errBuf.String())
		}
	})

	t.Run("expiring key", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		id := fixtureIdentity()
		expiry := fixedClock().Add(48 * time.Hour)
		id.Key.ExpiresAt = &expiry
		if err := p.WhoAmI(id); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "(in 2d)") {
			t.Errorf("expiring key must render the remaining time:\n%s", out.String())
		}
	})

	t.Run("keyless identity", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.WhoAmI(&qurlapi.Identity{OwnerID: "own_jwt", AuthType: "jwt"}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Key:") {
			t.Errorf("keyless identity must omit the key block:\n%s", out.String())
		}
	})

	t.Run("quiet", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)
		if err := p.WhoAmI(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		if out.String() != "own_output_test\n" {
			t.Errorf("quiet = %q, want the bare owner id", out.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
		if err := p.WhoAmI(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			OwnerID  string `json:"owner_id"`
			AuthType string `json:"auth_type"`
			APIKey   *struct {
				KeyID     string   `json:"key_id"`
				Scopes    []string `json:"scopes"`
				KeyPrefix string   `json:"key_prefix"`
			} `json:"api_key"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.OwnerID != "own_output_test" || doc.APIKey == nil || doc.APIKey.KeyID != "key_outputtest01" {
			t.Errorf("json projection = %+v", doc)
		}
	})
}

// TestLoginProjections pins login's split streams: the human confirmation is
// stderr; JSON and --quiet are stdout.
func TestLoginProjections(t *testing.T) {
	t.Run("text goes to stderr with the backend label", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.Login(fixtureIdentity(), auth.BackendKeyring); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Errorf("login text is a status message; stdout must stay empty, got %q", out.String())
		}
		for _, want := range []string{"Logged in as own_output_test.", "OS keyring"} {
			if !strings.Contains(errBuf.String(), want) {
				t.Errorf("confirmation missing %q:\n%s", want, errBuf.String())
			}
		}
	})

	t.Run("file backend label", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.Login(fixtureIdentity(), auth.BackendFile); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errBuf.String(), "credential file") {
			t.Errorf("fallback save must name the credential file:\n%s", errBuf.String())
		}
	})

	t.Run("quiet prints the owner id", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)
		if err := p.Login(fixtureIdentity(), auth.BackendKeyring); err != nil {
			t.Fatal(err)
		}
		if out.String() != "own_output_test\n" || errBuf.Len() != 0 {
			t.Errorf("quiet = stdout %q stderr %q", out.String(), errBuf.String())
		}
	})

	t.Run("json carries the machine-stable backend", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
		if err := p.Login(fixtureIdentity(), auth.BackendKeyring); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Stored string `json:"stored"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Stored != "keyring" {
			t.Errorf("stored = %q, want the Backend value", doc.Stored)
		}
	})
}

// TestLogoutProjections pins logout's renderings, including the idempotent
// empty case and the always-emitted removed array in JSON.
func TestLogoutProjections(t *testing.T) {
	t.Run("both backends", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.Logout([]auth.Backend{auth.BackendKeyring, auth.BackendFile}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errBuf.String(), "OS keyring and credential file") {
			t.Errorf("both backends must be listed:\n%s", errBuf.String())
		}
		if out.Len() != 0 {
			t.Errorf("logout text must not touch stdout, got %q", out.String())
		}
	})

	t.Run("nothing stored", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.Logout(nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errBuf.String(), "nothing to remove") {
			t.Errorf("idempotent logout must say so:\n%s", errBuf.String())
		}
	})

	t.Run("quiet is silent", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)
		if err := p.Logout([]auth.Backend{auth.BackendKeyring}); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || errBuf.Len() != 0 {
			t.Errorf("quiet logout must print nothing, got stdout %q stderr %q", out.String(), errBuf.String())
		}
	})

	t.Run("json empty removal is an empty array", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
		if err := p.Logout(nil); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); !strings.Contains(got, `"removed": []`) {
			t.Errorf("empty removal = %s, want an explicit empty array", got)
		}
	})
}
