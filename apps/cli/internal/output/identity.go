package output

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Identity renderings for whoami and login. The identity is who the
// credential is — owner, auth type, and the key's non-secret identity. There
// is deliberately no plan or usage data here; the platform's identity echo is
// authentication state only. whoami also renders the device public key, which
// comes from local agent state and is passed in separately.

type identityKeyJSON struct {
	KeyID     string     `json:"key_id"`
	Kind      string     `json:"kind"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// KeyStorage is the key provider protecting local device state: its id for
// JSON and a description for people. The zero value omits both.
type KeyStorage struct {
	Provider    string
	Description string
}

type whoamiJSON struct {
	OwnerID            string           `json:"owner_id"`
	AuthType           string           `json:"auth_type"`
	APIKey             *identityKeyJSON `json:"api_key,omitempty"`
	DevicePublicKeyB64 string           `json:"device_public_key_b64,omitempty"`
	// KeyStorage names the key provider protecting the local device state. It
	// is the connector's provider id, drawn from a closed set; encoding/json
	// escapes any control bytes, so it needs no text-side sanitizing here. It
	// is omitted when no local state was found.
	KeyStorage string `json:"key_storage,omitempty"`
}

type loginJSON struct {
	OwnerID  string `json:"owner_id"`
	AuthType string `json:"auth_type"`
	// DeviceKeyID is the enrolled device credential's public identifier, the
	// value a supervising app records next to the owner id. It is omitted
	// rather than empty when /v1/me reports no key object, so a supervisor
	// cannot persist "" as if it were an id.
	DeviceKeyID    string `json:"device_key_id,omitempty"`
	DeviceEnrolled bool   `json:"device_enrolled"`
}

func identityKey(id *qurlapi.Identity) *identityKeyJSON {
	if id.Key == nil {
		return nil
	}
	return &identityKeyJSON{
		KeyID:     id.Key.KeyID,
		Kind:      id.Key.Kind,
		Scopes:    id.Key.Scopes,
		ExpiresAt: id.Key.ExpiresAt,
	}
}

// WhoAmIRendersDeviceKey reports whether WhoAmI's projection for this printer
// includes the device public key. It mirrors WhoAmI's switch: JSON wins over
// --quiet, and plain --quiet prints only the owner id. Callers use it to skip a
// state read whose result would be discarded.
func (p *Printer) WhoAmIRendersDeviceKey() bool {
	return p.format == FormatJSON || !p.quiet
}

// WhoAmI renders the identity behind the configured credential. Identity is
// data (scripts pipe it), so every projection goes to stdout; --quiet prints
// just the owner id.
//
// devicePublicKeyB64 is this machine's registered-device public key from local
// agent state, never from /v1/me, and keyStorage describes the key provider
// protecting that state. An empty value omits its row and JSON field.
func (p *Printer) WhoAmI(id *qurlapi.Identity, devicePublicKeyB64 string, keyStorage KeyStorage) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(whoamiJSON{
			OwnerID: id.OwnerID, AuthType: id.AuthType, APIKey: identityKey(id), DevicePublicKeyB64: devicePublicKeyB64,
			KeyStorage: keyStorage.Provider,
		})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, id.OwnerID)
		return err
	default:
		return p.whoamiText(id, devicePublicKeyB64, keyStorage.Description)
	}
}

func (p *Printer) whoamiText(id *qurlapi.Identity, devicePublicKeyB64, keyStorage string) error {
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("%s\t%s\n", p.bold("Owner:"), id.OwnerID)
	ew.printf("%s\t%s\n", p.bold("Auth:"), id.AuthType)
	if k := id.Key; k != nil {
		ew.printf("%s\t%s\n", p.bold("Key:"), k.KeyID)
		ew.printf("%s\t%s\n", p.bold("Kind:"), k.Kind)
		ew.printf("%s\t%s\n", p.bold("Scopes:"), strings.Join(k.Scopes, ", "))
		ew.printf("%s\t%s\n", p.bold("Expires:"), p.keyExpiry(k.ExpiresAt))
	}
	if devicePublicKeyB64 != "" {
		ew.printf("%s\t%s\n", p.bold("Device public key:"), devicePublicKeyB64)
	}
	if keyStorage != "" {
		ew.printf("%s\t%s\n", p.bold("Key storage:"), keyStorage)
	}
	return ew.flush(tw)
}

func (p *Printer) keyExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return p.formatExpiry(*t)
}

// Login renders a successful registered-device enrollment.
// The confirmation is a status message for humans and goes to stderr; JSON
// emits the device identity document on stdout; --quiet prints the owner id
// so scripts get the same primary value whoami would give them.
func (p *Printer) Login(id *qurlapi.Identity) error {
	switch {
	case p.format == FormatJSON:
		doc := loginJSON{OwnerID: id.OwnerID, AuthType: id.AuthType, DeviceEnrolled: true}
		if id.Key != nil {
			doc.DeviceKeyID = id.Key.KeyID
		}
		return p.writeJSON(doc)
	case p.quiet:
		_, err := fmt.Fprintln(p.out, id.OwnerID)
		return err
	default:
		return p.loginText(id)
	}
}

func (p *Printer) loginText(id *qurlapi.Identity) error {
	ew := &errWriter{w: p.err}
	ew.printf("%s\n\n", fmt.Sprintf(msgDeviceEnrolled, p.bold(id.OwnerID)))
	if ew.err != nil {
		return ew.err
	}
	tw := tabwriter.NewWriter(p.err, 0, 0, 2, ' ', 0)
	twe := &errWriter{w: tw}
	twe.printf("  %s\t%s\n", p.bold("Auth:"), id.AuthType)
	twe.printf("  %s\t%s\n", p.bold("Enrollment credential:"), "consumed, not stored")
	return twe.flush(tw)
}

// Account reports an optional account operation using the normal output contract.
// Human guidance stays on stderr; JSON and quiet output carry the resource owner.
func (p *Printer) Account(ownerID, status, message string) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(map[string]string{"owner_id": ownerID, "status": status})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, ownerID)
		return err
	default:
		_, err := fmt.Fprintln(p.err, message)
		return err
	}
}
