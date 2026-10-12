package output

import (
	"fmt"
	"time"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const (
	consumerActionListGrants      = "list_grants"
	consumerActionListInvitations = "list_invitations"
)

// ConsumerManagement renders the decision receipt without claiming pending cleanup succeeded.
func (p *Printer) ConsumerManagement(result *qurlapi.ConsumerManagementResult) error {
	if p.format == FormatJSON {
		return p.writeJSON(consumerManagementDocument(result))
	}
	if p.quiet {
		return p.consumerManagementQuiet(result)
	}
	return p.consumerManagementText(result)
}

func (p *Printer) consumerManagementQuiet(result *qurlapi.ConsumerManagementResult) error {
	if result.Action == consumerActionListInvitations || result.Action == consumerActionListGrants {
		ew := &errWriter{w: p.out}
		for _, invitation := range result.Invitations {
			ew.printf("%s\n", invitation.ID)
		}
		for i := range result.Grants {
			ew.printf("%s\n", result.Grants[i].ID)
		}
		if result.NextCursor != "" {
			p.notefMore(result.NextCursor)
		}
		return ew.flush(nil)
	}
	id := result.CRID
	if result.Invitation != nil {
		id = result.Invitation.ID
	} else if result.Grant != nil {
		id = result.Grant.ID
	}
	_, err := fmt.Fprintln(p.out, id)
	return err
}

func (p *Printer) consumerManagementText(result *qurlapi.ConsumerManagementResult) error {
	if _, err := fmt.Fprintf(p.out, "Resource: %s\nAction: %s\n", result.CRID, result.Action); err != nil {
		return err
	}
	if result.Policy != nil {
		policy := result.Policy.Mode
		if result.Policy.ContractID != "" {
			policy += " " + result.Policy.ContractID
		}
		_, err := fmt.Fprintf(p.out, "Access: private (%s)\nVersion: %d\n", policy, result.Policy.Version)
		return err
	}
	if result.Invitation != nil {
		value := result.Invitation
		ew := &errWriter{w: p.out}
		ew.printf("Invitation: %s\nEmail: %q\nStatus: %s\nVersion: %d\nDelivery: %s\n", value.ID, value.Email, value.Status, value.Version, value.Delivery)
		if value.SessionCleanup != "" {
			ew.printf("Connection cleanup: %s\n", value.SessionCleanup)
		}
		return ew.flush(nil)
	}
	if result.Grant != nil {
		value := result.Grant
		ew := &errWriter{w: p.out}
		ew.printf("Consumer: %s\nStatus: %s\nVersion: %d\n", value.ID, value.Status, value.Version)
		if value.SessionCleanup != "" {
			ew.printf("Connection cleanup: %s\n", value.SessionCleanup)
		}
		return ew.flush(nil)
	}
	for _, invitation := range result.Invitations {
		if _, err := fmt.Fprintf(p.out, "%s  %s  version %d  %q\n", invitation.ID, invitation.Status, invitation.Version, invitation.Email); err != nil {
			return err
		}
	}
	for i := range result.Grants {
		grant := &result.Grants[i]
		contact := ""
		if grant.Email != "" {
			contact = fmt.Sprintf("  %q", grant.Email)
		}
		if _, err := fmt.Fprintf(p.out, "%s  %s  version %d%s\n", grant.ID, grant.Status, grant.Version, contact); err != nil {
			return err
		}
	}
	if result.NextCursor != "" {
		p.notefMore(result.NextCursor)
	}
	if result.NextCursor == "" && result.Action == consumerActionListInvitations && len(result.Invitations) == 0 {
		_, err := fmt.Fprintln(p.out, "No invitations.")
		return err
	}
	if result.NextCursor == "" && result.Action == consumerActionListGrants && len(result.Grants) == 0 {
		_, err := fmt.Fprintln(p.out, "No grants.")
		return err
	}
	return nil
}

// Explicit output fields prevent SDK additions from exposing invitation secrets.
type consumerInvitationJSON struct {
	ID             string     `json:"id"`
	Email          string     `json:"email"`
	Status         string     `json:"status"`
	Delivery       string     `json:"delivery"`
	Version        int64      `json:"version"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	SessionCleanup string     `json:"session_cleanup,omitempty"`
}

type consumerGrantJSON struct {
	ID                  string     `json:"id"`
	Status              string     `json:"status"`
	Version             int64      `json:"version"`
	CredentialKind      string     `json:"credential_kind,omitempty"`
	CredentialID        string     `json:"credential_id,omitempty"`
	CredentialPublicKey string     `json:"credential_public_key_b64,omitempty"`
	Email               string     `json:"email,omitempty"`
	EmailVerifiedAt     *time.Time `json:"email_verified_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	EntitlementID       string     `json:"entitlement_id,omitempty"`
	SessionCleanup      string     `json:"session_cleanup,omitempty"`
}

func invitationDocument(value *qurl.ConsumerInvitation) *consumerInvitationJSON {
	if value == nil {
		return nil
	}
	var expiry *time.Time
	if !value.ExpiresAt.IsZero() {
		expiry = &value.ExpiresAt
	}
	return &consumerInvitationJSON{ID: value.ID, Email: value.Email, Status: value.Status, Delivery: value.Delivery, Version: value.Version, ExpiresAt: expiry, SessionCleanup: value.SessionCleanup}
}

func grantDocument(value *qurl.ConsumerGrant) *consumerGrantJSON {
	if value == nil {
		return nil
	}
	return &consumerGrantJSON{ID: value.ID, Status: value.Status, Version: value.Version, CredentialKind: value.CredentialKind, CredentialID: value.CredentialID, CredentialPublicKey: value.CredentialPublicKey, Email: value.Email, EmailVerifiedAt: value.EmailVerifiedAt, ExpiresAt: value.ExpiresAt, EntitlementID: value.EntitlementID, SessionCleanup: value.SessionCleanup}
}

func consumerManagementDocument(result *qurlapi.ConsumerManagementResult) any {
	out := struct {
		Action      string                     `json:"action"`
		CRID        string                     `json:"crid"`
		Policy      *consumerAccessJSON        `json:"consumer_access,omitempty"`
		Invitation  *consumerInvitationJSON    `json:"invitation,omitempty"`
		Grant       *consumerGrantJSON         `json:"grant,omitempty"`
		Invitations *[]*consumerInvitationJSON `json:"invitations,omitempty"`
		Grants      *[]*consumerGrantJSON      `json:"grants,omitempty"`
		NextCursor  string                     `json:"next_cursor,omitempty"`
	}{Action: result.Action, CRID: result.CRID, Policy: policyDocument(result.Policy), Invitation: invitationDocument(result.Invitation), Grant: grantDocument(result.Grant), NextCursor: result.NextCursor}
	if result.Action == consumerActionListInvitations || len(result.Invitations) > 0 {
		values := make([]*consumerInvitationJSON, 0, len(result.Invitations))
		for i := range result.Invitations {
			values = append(values, invitationDocument(&result.Invitations[i]))
		}
		out.Invitations = &values
	}
	if result.Action == consumerActionListGrants || len(result.Grants) > 0 {
		values := make([]*consumerGrantJSON, 0, len(result.Grants))
		for i := range result.Grants {
			values = append(values, grantDocument(&result.Grants[i]))
		}
		out.Grants = &values
	}
	return out
}

type consumerAccessJSON struct {
	Mode       string `json:"mode"`
	Version    int64  `json:"version"`
	ContractID string `json:"contract_id,omitempty"`
}

func policyDocument(policy *qurl.ConsumerAccessPolicy) *consumerAccessJSON {
	if policy == nil {
		return nil
	}
	return &consumerAccessJSON{Mode: policy.Mode, Version: policy.Version, ContractID: policy.ContractID}
}
