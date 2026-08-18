package output

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
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
	OwnerID  string           `json:"owner_id"`
	AuthType string           `json:"auth_type"`
	APIKey   *identityKeyJSON `json:"api_key,omitempty"`
	// Stored names the backend that took the key: "keyring" or "file".
	Stored string `json:"stored"`
}

type logoutJSON struct {
	// Removed lists the backends a key was removed from ("keyring", "file");
	// empty when nothing was stored. Always emitted so scripts can tell the
	// idempotent no-op from the real removal.
	Removed []string `json:"removed"`
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

// backendLabel maps a storage backend to its prose name; JSON carries the
// machine-stable Backend value itself.
func backendLabel(b auth.Backend) string {
	if b == auth.BackendKeyring {
		return labelKeyring
	}
	return labelCredentialFile
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

// Login renders a successful login: who the key is and where it was stored.
// The confirmation is a status message for humans and goes to stderr; JSON
// emits the identity-plus-storage document on stdout; --quiet prints the
// owner id so scripts get the same primary value whoami would give them.
func (p *Printer) Login(id *qurlapi.Identity, stored auth.Backend) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(loginJSON{OwnerID: id.OwnerID, AuthType: id.AuthType, APIKey: identityKey(id), Stored: string(stored)})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, id.OwnerID)
		return err
	default:
		return p.loginText(id, stored)
	}
}

func (p *Printer) loginText(id *qurlapi.Identity, stored auth.Backend) error {
	ew := &errWriter{w: p.err}
	ew.printf("%s\n\n", fmt.Sprintf(msgLoggedInAs, p.bold(id.OwnerID)))
	if ew.err != nil {
		return ew.err
	}
	tw := tabwriter.NewWriter(p.err, 0, 0, 2, ' ', 0)
	twe := &errWriter{w: tw}
	if k := id.Key; k != nil {
		twe.printf("  %s\t%s\n", p.bold("Key:"), keyLine(k))
		twe.printf("  %s\t%s\n", p.bold("Scopes:"), strings.Join(k.Scopes, ", "))
	}
	twe.printf("  %s\t%s\n", p.bold("Stored:"), backendLabel(stored))
	return twe.flush(tw)
}

// Logout renders a logout outcome. The confirmation goes to stderr; JSON
// reports the removed backends on stdout; --quiet prints nothing (the exit
// code is the outcome). An empty removed set is the idempotent no-op.
func (p *Printer) Logout(removed []auth.Backend) error {
	switch {
	case p.format == FormatJSON:
		names := make([]string, 0, len(removed))
		for _, b := range removed {
			names = append(names, string(b))
		}
		return p.writeJSON(logoutJSON{Removed: names})
	case p.quiet:
		return nil
	case len(removed) == 0:
		_, err := fmt.Fprintf(p.err, "%s\n", msgNothingStored)
		return err
	default:
		labels := make([]string, 0, len(removed))
		for _, b := range removed {
			labels = append(labels, backendLabel(b))
		}
		_, err := fmt.Fprintf(p.err, msgLoggedOut+"\n", strings.Join(labels, " and "))
		return err
	}
}
