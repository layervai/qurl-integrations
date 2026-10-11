package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func consumerAccessCmd(opts *globalOpts) *cobra.Command {
	var email, mode, contract, cursor, kind, credentialID, key, entitlement, expiry string
	var version int64
	var yes bool
	allowed := map[string][]string{
		consumerInvite: {"email"}, consumerListInvitations: {consumerCursor}, consumerListGrants: {consumerCursor},
		consumerResend: {consumerVersion}, consumerRevokeInvitation: {consumerVersion}, consumerDeny: {consumerVersion}, consumerRevokeGrant: {consumerVersion},
		consumerConfigure: {"mode", consumerContract, consumerVersion},
		consumerApprove:   {consumerVersion, "credential-kind", "credential-id", "credential-key", "entitlement", "expires-at"},
	}
	cmd := &cobra.Command{
		Use: "access <action> <CRID> [<decision-id>]", Short: "Manage private invitations and consumer access",
		Long: `Manage private CRID access as its publisher.
Actions: invite, list_invitations, list_grants, resend, revoke_invitation,
approve, deny, revoke_grant, configure.
Read the version from a listing before a decision. Use --version with that
version. The first policy on an existing private resource uses --version 0.
A key opt-in uses decision-id new and --version 0. Contract approval
requires a trusted publisher entitlement receipt and expiry; it does not
verify payment. Mailbox verification proves email control, not legal identity.
If a contract notification is pending, repeat the same receipt, expiry and version.
A removal is complete only when connection cleanup says complete. If pending,
repeat the same removal and version. Invitations send email on your request.
Use --access when you publish to choose the initial policy.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.RangeArgs(2, 3)(cmd, args); err != nil {
				return exitcode.UsageError(err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			input := consumerAccessInput{action: args[0], email: email, mode: mode, contract: contract, cursor: cursor, version: version, kind: kind, credentialID: credentialID, key: key, entitlement: entitlement, expiry: expiry}
			if len(args) == 3 {
				input.decision = args[2]
			}
			if err := validateConsumerActionFlags(cmd, input.action, allowed); err != nil {
				return err
			}
			if err := validateConsumerDecision(&input); err != nil {
				return err
			}
			if err := validateConsumerFields(&input); err != nil {
				return err
			}
			assessment, err := cridux.Assess(args[1])
			if err != nil {
				return err
			}
			if input.action == consumerListInvitations || input.action == consumerListGrants {
				err = requireCRID(assessment)
			} else {
				err = applyCRIDGuards(opts.printer(), assessment, opts.productionEndpoint(), yes)
			}
			if err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			result, err := executeConsumerAction(cmd.Context(), client.ConsumerManagement(), assessment.Input, &input)
			if err != nil {
				return err
			}
			return opts.printer().ConsumerManagement(result)
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email address for an invitation")
	cmd.Flags().StringVar(&mode, "mode", "", "private access policy")
	cmd.Flags().StringVar(&contract, consumerContract, "", "publisher contract identifier")
	cmd.Flags().StringVar(&cursor, consumerCursor, "", "next page cursor from a listing")
	cmd.Flags().Int64Var(&version, consumerVersion, -1, "current policy or decision version")
	cmd.Flags().StringVar(&kind, "credential-kind", "", "passkey or device for a key opt-in")
	cmd.Flags().StringVar(&credentialID, "credential-id", "", "credential id for a key opt-in")
	cmd.Flags().StringVar(&key, "credential-key", "", "public credential key for a key opt-in")
	cmd.Flags().StringVar(&entitlement, "entitlement", "", "trusted publisher entitlement receipt")
	cmd.Flags().StringVar(&expiry, "expires-at", "", "entitlement expiry as an RFC 3339 time")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow changes to a test CRID on a production endpoint")
	return cmd
}

const (
	consumerInvite           = "invite"
	consumerListInvitations  = "list_invitations"
	consumerListGrants       = "list_grants"
	consumerResend           = "resend"
	consumerRevokeInvitation = "revoke_invitation"
	consumerApprove          = "approve"
	consumerDeny             = "deny"
	consumerRevokeGrant      = "revoke_grant"
	consumerConfigure        = "configure"
	consumerVersion          = "version"
	consumerCursor           = "cursor"
	consumerContract         = "contract"
)

var consumerDecisionID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type consumerAccessInput struct {
	action, decision, email, mode, contract, cursor, kind, credentialID, key, entitlement, expiry string
	version                                                                                       int64
	expiresAt                                                                                     *time.Time
}

func validateConsumerActionFlags(cmd *cobra.Command, action string, allowed map[string][]string) error {
	flags, ok := allowed[action]
	if !ok {
		return exitcode.UsageError(errors.New("unknown access action"))
	}
	for _, name := range []string{"email", "mode", consumerContract, consumerCursor, consumerVersion, "credential-kind", "credential-id", "credential-key", "entitlement", "expires-at"} {
		if cmd.Flags().Changed(name) && !slices.Contains(flags, name) {
			return exitcode.UsageError(fmt.Errorf("--%s does not apply to %s", name, action))
		}
	}
	return nil
}

func validateConsumerDecision(input *consumerAccessInput) error {
	needsDecision := slices.Contains([]string{consumerResend, consumerRevokeInvitation, consumerApprove, consumerDeny, consumerRevokeGrant}, input.action)
	if needsDecision != (input.decision != "") {
		return exitcode.UsageError(errors.New("a decision id is required only for decision actions"))
	}
	newKey := input.action == consumerApprove && input.decision == "new"
	if needsDecision && !consumerDecisionID.MatchString(input.decision) && !newKey {
		return exitcode.UsageError(errors.New("use a decision id from a listing, or new for a key opt-in"))
	}
	minimum := int64(1)
	if newKey || input.action == consumerConfigure {
		minimum = 0
	}
	if (needsDecision || input.action == consumerConfigure) && (input.version < minimum || input.version >= 1<<53-1) {
		return exitcode.UsageError(errors.New("--version must be the current decision or policy version"))
	}
	return nil
}

func validateConsumerFields(input *consumerAccessInput) error {
	if input.action == consumerInvite && input.email == "" {
		return exitcode.UsageError(errors.New("--email is required"))
	}
	if len(input.email) > 320 || len(input.cursor) > 4096 || len(input.contract) > 128 || len(input.entitlement) > 256 {
		return exitcode.UsageError(errors.New("access input is too long"))
	}
	if input.expiry != "" {
		parsed, err := time.Parse(time.RFC3339, input.expiry)
		if err != nil {
			return exitcode.UsageError(errors.New("--expires-at must be an RFC 3339 time"))
		}
		input.expiresAt = &parsed
	}
	if input.action == consumerConfigure && (!slices.Contains([]string{"keys", "invite_only", "email_capture", "review", consumerContract}, input.mode) || ((input.mode == consumerContract) != (input.contract != ""))) {
		return exitcode.UsageError(errors.New("choose a valid --mode and a --contract only for contract mode"))
	}
	return nil
}

func executeConsumerAction(ctx context.Context, owner qurlapi.ConsumerManagementClient, crid string, input *consumerAccessInput) (*qurlapi.ConsumerManagementResult, error) {
	result := &qurlapi.ConsumerManagementResult{Action: input.action, CRID: crid}
	var err error
	switch input.action {
	case consumerInvite:
		result.Invitation, err = owner.InviteConsumer(ctx, crid, input.email)
	case consumerResend:
		result.Invitation, err = owner.ResendConsumerInvitation(ctx, crid, input.decision, input.version)
	case consumerRevokeInvitation:
		result.Invitation, err = owner.RevokeConsumerInvitation(ctx, crid, input.decision, input.version)
	case consumerApprove:
		result.Grant, err = owner.ApproveConsumerGrant(ctx, crid, input.decision, qurl.ConsumerGrantApproval{ExpectedVersion: input.version, CredentialKind: input.kind, CredentialID: input.credentialID, CredentialPublicKey: input.key, EntitlementID: input.entitlement, ExpiresAt: input.expiresAt})
	case consumerDeny:
		result.Grant, err = owner.DenyConsumerGrant(ctx, crid, input.decision, input.version)
	case consumerRevokeGrant:
		result.Grant, err = owner.RevokeConsumerGrant(ctx, crid, input.decision, input.version)
	case consumerConfigure:
		policy := qurl.ConsumerAccessPolicy{Mode: input.mode, Version: input.version + 1, ContractID: input.contract}
		err = owner.UpdateConsumerAccess(ctx, crid, input.version, policy)
		result.Policy = &policy
	case consumerListInvitations:
		var page *qurl.ConsumerPage[qurl.ConsumerInvitation]
		page, err = owner.ListConsumerInvitations(ctx, crid, input.cursor)
		if err == nil {
			result.Invitations = page.Data
			result.NextCursor = page.Meta.NextCursor
		}
	case consumerListGrants:
		var page *qurl.ConsumerPage[qurl.ConsumerGrant]
		page, err = owner.ListConsumerGrants(ctx, crid, input.cursor)
		if err == nil {
			result.Grants = page.Data
			result.NextCursor = page.Meta.NextCursor
		}
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}
