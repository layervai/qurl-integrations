package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const fixtureDeviceKey = "dGVzdC1kZXZpY2UtcHVibGljLWtleQ=="

func fixtureIdentity() *qurlapi.Identity {
	return &qurlapi.Identity{
		OwnerID:  "own_output_test",
		AuthType: "api_key",
		Key: &qurlapi.KeyIdentity{
			KeyID:  "key_outputtest01",
			Kind:   "api_key",
			Scopes: []string{"qurl:read", "qurl:write"},
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
		if err := p.WhoAmI(fixtureIdentity(), fixtureDeviceKey, ""); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"own_output_test", "key_outputtest01", "qurl:read, qurl:write", "never", "Device public key:", fixtureDeviceKey} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("text projection missing %q:\n%s", want, out.String())
			}
		}
		if errBuf.Len() != 0 {
			t.Errorf("whoami text is data; stderr must stay empty, got %q", errBuf.String())
		}
		// The long label re-aligns every row; pin the exact layout.
		want := "Owner:              own_output_test\n" +
			"Auth:               api_key\n" +
			"Key:                key_outputtest01\n" +
			"Kind:               api_key\n" +
			"Scopes:             qurl:read, qurl:write\n" +
			"Expires:            never\n" +
			"Device public key:  " + fixtureDeviceKey + "\n"
		if out.String() != want {
			t.Errorf("text projection =\n%s\nwant\n%s", out.String(), want)
		}
	})

	t.Run("expiring key", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		id := fixtureIdentity()
		expiry := fixedClock().Add(48 * time.Hour)
		id.Key.ExpiresAt = &expiry
		if err := p.WhoAmI(id, fixtureDeviceKey, ""); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "(in 2d)") {
			t.Errorf("expiring key must render the remaining time:\n%s", out.String())
		}
	})

	t.Run("keyless identity", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.WhoAmI(&qurlapi.Identity{OwnerID: "own_jwt", AuthType: "jwt"}, "", ""); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Key:") || strings.Contains(out.String(), "Device public key:") {
			t.Errorf("keyless identity must omit the key block and device public key:\n%s", out.String())
		}
	})

	t.Run("quiet", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)
		if err := p.WhoAmI(fixtureIdentity(), fixtureDeviceKey, ""); err != nil {
			t.Fatal(err)
		}
		if out.String() != "own_output_test\n" {
			t.Errorf("quiet = %q, want the bare owner id", out.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
		if err := p.WhoAmI(fixtureIdentity(), fixtureDeviceKey, ""); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			OwnerID  string `json:"owner_id"`
			AuthType string `json:"auth_type"`
			APIKey   *struct {
				KeyID  string   `json:"key_id"`
				Scopes []string `json:"scopes"`
			} `json:"api_key"`
			DevicePublicKeyB64 string `json:"device_public_key_b64"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.OwnerID != "own_output_test" || doc.APIKey == nil || doc.APIKey.KeyID != "key_outputtest01" ||
			doc.DevicePublicKeyB64 != fixtureDeviceKey {
			t.Errorf("json projection = %+v", doc)
		}
		if strings.Contains(out.String(), "key_prefix") {
			t.Errorf("whoami must not echo any part of the API key secret:\n%s", out.String())
		}
	})
}

// TestWhoAmIRendersDeviceKey pins that the predicate whoami uses to skip the
// state read agrees with what WhoAmI actually renders, for every projection.
func TestWhoAmIRendersDeviceKey(t *testing.T) {
	for _, tc := range []struct {
		format Format
		quiet  bool
		want   bool
	}{
		{FormatText, false, true},
		{FormatText, true, false},
		{FormatJSON, false, true},
		{FormatJSON, true, true},
	} {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, tc.format, tc.quiet, false, false)
		if got := p.WhoAmIRendersDeviceKey(); got != tc.want {
			t.Errorf("format %v quiet %v: WhoAmIRendersDeviceKey = %v, want %v", tc.format, tc.quiet, got, tc.want)
		}
		if err := p.WhoAmI(fixtureIdentity(), fixtureDeviceKey, ""); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), fixtureDeviceKey); got != tc.want {
			t.Errorf("format %v quiet %v: WhoAmI rendered key = %v, but WhoAmIRendersDeviceKey = %v", tc.format, tc.quiet, got, tc.want)
		}
	}
}

// TestWhoAmIJSONOmitsAnAbsentDevicePublicKey pins the omitempty half of the
// contract: without native device state there is no device_public_key_b64, so
// a supervisor cannot persist "" as if it were a key.
func TestWhoAmIJSONOmitsAnAbsentDevicePublicKey(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
	if err := p.WhoAmI(&qurlapi.Identity{OwnerID: "own_jwt", AuthType: "jwt"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "device_public_key_b64") {
		t.Fatalf("whoami JSON = %s, want no device_public_key_b64 without native device state", out.String())
	}
}

// TestLoginProjections pins login's split streams: the human confirmation is
// stderr; JSON and --quiet are stdout.
func TestLoginProjections(t *testing.T) {
	t.Run("text confirms device enrollment and key disposal", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.Login(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Errorf("login text is a status message; stdout must stay empty, got %q", out.String())
		}
		for _, want := range []string{"Enrolled this device for own_output_test.", "Enrollment credential:", "consumed, not stored"} {
			if !strings.Contains(errBuf.String(), want) {
				t.Errorf("confirmation missing %q:\n%s", want, errBuf.String())
			}
		}
	})

	t.Run("quiet prints the owner id", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)
		if err := p.Login(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		if out.String() != "own_output_test\n" || errBuf.Len() != 0 {
			t.Errorf("quiet = stdout %q stderr %q", out.String(), errBuf.String())
		}
	})

	t.Run("json confirms device enrollment", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
		if err := p.Login(fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			DeviceKeyID    string `json:"device_key_id"`
			DeviceEnrolled bool   `json:"device_enrolled"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if !doc.DeviceEnrolled {
			t.Error("device_enrolled = false, want true")
		}
		if doc.DeviceKeyID != "key_outputtest01" {
			t.Errorf("device_key_id = %q, want the enrolled device key id", doc.DeviceKeyID)
		}
	})
}

// TestLoginJSONOmitsAnAbsentDeviceKeyID pins that a supervisor reading the
// login document cannot mistake an absent key for an empty id: /v1/me without
// a key object drops the field rather than emitting "".
func TestLoginJSONOmitsAnAbsentDeviceKeyID(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
	if err := p.Login(&qurlapi.Identity{OwnerID: "own_output_test", AuthType: "api_key"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "device_key_id") {
		t.Fatalf("login JSON = %s, want no device_key_id when /v1/me reports no key", out.String())
	}
}

// TestWhoAmIReportsKeyStorage pins the key-storage line: named for people in
// text, the raw provider in JSON, and absent from both when there is no local
// state to describe.
func TestWhoAmIReportsKeyStorage(t *testing.T) {
	for provider, wantText := range map[string]string{
		"tpm":       "TPM (sealed to this machine)",
		"file":      "file (owner-only, not encrypted)",
		"local-key": "local-key",
	} {
		t.Run(provider, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).WhoAmI(fixtureIdentity(), fixtureDeviceKey, provider); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Key storage:") || !strings.Contains(out.String(), wantText) {
				t.Fatalf("text projection missing key storage %q:\n%s", wantText, out.String())
			}
			out.Reset()
			if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).WhoAmI(fixtureIdentity(), fixtureDeviceKey, provider); err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc["key_storage"] != provider {
				t.Fatalf("JSON key_storage = %v, want %q", doc["key_storage"], provider)
			}
		})
	}
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).WhoAmI(fixtureIdentity(), fixtureDeviceKey, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "key_storage") {
		t.Fatalf("JSON without local state must omit key_storage:\n%s", out.String())
	}
}
