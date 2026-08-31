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
// authentication state only.

type identityKeyJSON struct {
	KeyID     string     `json:"key_id"`
	Kind      string     `json:"kind"`
	Scopes    []string   `json:"scopes"`
	KeyPrefix string     `json:"key_prefix,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type whoamiJSON struct {
	OwnerID  string           `json:"owner_id"`
	AuthType string           `json:"auth_type"`
	APIKey   *identityKeyJSON `json:"api_key,omitempty"`
}

type loginJSON struct {
	OwnerID        string `json:"owner_id"`
	AuthType       string `json:"auth_type"`
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
		KeyPrefix: id.Key.KeyPrefix,
		ExpiresAt: id.Key.ExpiresAt,
	}
}

// WhoAmI renders the identity behind the configured credential. Identity is
// data (scripts pipe it), so every projection goes to stdout; --quiet prints
// just the owner id.
func (p *Printer) WhoAmI(id *qurlapi.Identity) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(whoamiJSON{OwnerID: id.OwnerID, AuthType: id.AuthType, APIKey: identityKey(id)})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, id.OwnerID)
		return err
	default:
		return p.whoamiText(id)
	}
}

func (p *Printer) whoamiText(id *qurlapi.Identity) error {
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("%s\t%s\n", p.bold("Owner:"), id.OwnerID)
	ew.printf("%s\t%s\n", p.bold("Auth:"), id.AuthType)
	if k := id.Key; k != nil {
		ew.printf("%s\t%s\n", p.bold("Key:"), keyLine(k))
		ew.printf("%s\t%s\n", p.bold("Kind:"), k.Kind)
		ew.printf("%s\t%s\n", p.bold("Scopes:"), strings.Join(k.Scopes, ", "))
		ew.printf("%s\t%s\n", p.bold("Expires:"), p.keyExpiry(k.ExpiresAt))
	}
	return ew.flush(tw)
}

// keyLine renders the key's identity: the id, plus the non-secret display
// prefix when the platform provided one.
func keyLine(k *qurlapi.KeyIdentity) string {
	if k.KeyPrefix == "" {
		return k.KeyID
	}
	return fmt.Sprintf("%s (%s)", k.KeyID, k.KeyPrefix)
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
		return p.writeJSON(loginJSON{OwnerID: id.OwnerID, AuthType: id.AuthType, DeviceEnrolled: true})
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
	twe.printf("  %s\t%s\n", p.bold("Account key:"), "consumed, not stored")
	return twe.flush(tw)
}
