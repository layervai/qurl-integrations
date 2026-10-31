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

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func consumerAccessCmd(opts *globalOpts) *cobra.Command {
	var email, mode, contract, cursor, kind, credentialID, key, entitlement, expiry, ruleSelector, ruleFile string
	var version int64
	var yes bool
	allowed := map[string][]string{
		consumerInvite: {"email"}, consumerListInvitations: {consumerCursor}, consumerListGrants: {consumerCursor},
		consumerResend: {consumerVersion}, consumerRevokeInvitation: {consumerVersion}, consumerDeny: {consumerVersion}, consumerRevokeGrant: {consumerVersion},
		consumerConfigure: {modeFlag, consumerContract, consumerVersion, ruleFlag, ruleFileFlag},
		consumerApprove:   {consumerVersion, "credential-kind", "credential-id", "credential-key", "entitlement", "expires-at"},
	}
	cmd := &cobra.Command{
		Use: "access <action> <CRID> [<decision-id>]", Short: "Manage private invitations and consumer access",
		Long: `Manage private CRID access as its publisher.
Actions: invite, list_invitations, list_grants, resend, revoke_invitation,
approve, deny, revoke_grant, configure.
Read the version from a listing before a decision. Use --version with that
version. The first policy on an existing private resource uses --version 0.
A key opt-in uses decision-id new and --version 0. An approval or key opt-in
lasts as long as the share's access rule says (Approvals last), unless
--expires-at sets its end. Contract approval
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
			if err := validateRuleSelectionFlags(cmd); err != nil {
				return err
			}
			if err := opts.applyConsumerRule(cmd, &input, ruleSelector, ruleFile); err != nil {
				return err
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
			result, err := opts.executeWithShareRule(cmd.Context(), client.ConsumerManagement(), assessment.Input, &input)
			if err != nil {
				return err
			}
			return opts.printer().ConsumerManagement(result)
		},
	}
	cmd.Flags().StringVar(&ruleSelector, ruleFlag, "", "saved private access rule ID or exact name")
	cmd.Flags().StringVar(&ruleFile, ruleFileFlag, "", "use - to apply the private rule snapshot from standard input")
	cmd.Flags().StringVar(&email, "email", "", "email address for an invitation")
	cmd.Flags().StringVar(&mode, modeFlag, "", "private access policy")
	cmd.Flags().StringVar(&contract, consumerContract, "", "publisher contract identifier")
	cmd.Flags().StringVar(&cursor, consumerCursor, "", "next page cursor from a listing")
	cmd.Flags().Int64Var(&version, consumerVersion, -1, "current policy or decision version")
	cmd.Flags().StringVar(&kind, "credential-kind", "", "passkey or device for a key opt-in")
	cmd.Flags().StringVar(&credentialID, "credential-id", "", "credential id for a key opt-in")
	cmd.Flags().StringVar(&key, "credential-key", "", "public credential key for a key opt-in")
	cmd.Flags().StringVar(&entitlement, "entitlement", "", "trusted publisher entitlement receipt")
	cmd.Flags().StringVar(&expiry, "expires-at", "", "when the approval ends, as an RFC 3339 time (default: the share's access rule; required with --entitlement)")
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
	consumerNewKey           = "new"
)

// TODO(upstream-contract): Keep decision identifiers and the new sentinel in sync with qurl-service.
var consumerDecisionID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type consumerAccessInput struct {
	action, decision, email, mode, contract, cursor, kind, credentialID, key, entitlement, expiry string
	version                                                                                       int64
	expiresAt                                                                                     *time.Time
	policy                                                                                        *qurl.ConsumerAccessPolicy
	rule                                                                                          *accessrules.Definition
}

func validateConsumerActionFlags(cmd *cobra.Command, action string, allowed map[string][]string) error {
	flags, ok := allowed[action]
	if !ok {
		return exitcode.UsageError(errors.New("unknown access action"))
	}
	for _, name := range []string{"email", modeFlag, ruleFlag, ruleFileFlag, consumerContract, consumerCursor, consumerVersion, "credential-kind", "credential-id", "credential-key", "entitlement", "expires-at"} {
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
	newKey := input.action == consumerApprove && input.decision == consumerNewKey
	if needsDecision && !consumerDecisionID.MatchString(input.decision) && !newKey {
		return exitcode.UsageError(errors.New("use a decision id from a listing, or new for a key opt-in"))
	}
	if newKey && input.version != 0 {
		return exitcode.UsageError(errors.New("a new key opt-in requires --version 0"))
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
	// TODO(upstream-contract): Keep consumer input bounds in sync with qurl-service.
	if len(input.email) > 320 || len(input.cursor) > 4096 || len(input.contract) > 128 || len(input.entitlement) > 256 || len(input.credentialID) > 171 || len(input.key) > 256 {
		return exitcode.UsageError(errors.New("access input is too long"))
	}
	if input.expiry != "" {
		parsed, err := time.Parse(time.RFC3339, input.expiry)
		if err != nil {
			return exitcode.UsageError(errors.New("--expires-at must be an RFC 3339 time"))
		}
		// Exact receipt retries can recover a pending notification after expiry.
		input.expiresAt = &parsed
	}
	if input.action == consumerConfigure && input.policy == nil && qurlapi.ValidateConsumerPolicyFields(input.mode, input.contract) != nil {
		return exitcode.UsageError(errors.New("choose a valid --mode and a --contract only for contract mode"))
	}
	if input.action == consumerApprove {
		return validateConsumerApproval(input)
	}
	return nil
}

func validateConsumerApproval(input *consumerAccessInput) error {
	newKey := input.decision == consumerNewKey
	hasCredential := input.kind != "" || input.credentialID != "" || input.key != ""
	if newKey && (!slices.Contains([]string{"passkey", "device"}, input.kind) || input.credentialID == "" || input.key == "") {
		return exitcode.UsageError(errors.New("a key opt-in requires --credential-kind passkey or device, --credential-id and --credential-key"))
	}
	if !newKey && hasCredential {
		return exitcode.UsageError(errors.New("credential flags apply only to decision-id new"))
	}
	if input.entitlement != "" && (input.expiry == "" || newKey) {
		return exitcode.UsageError(errors.New("contract approval requires --entitlement and --expires-at together, without a key opt-in"))
	}
	// Without a receipt, --expires-at is when a review approval or key opt-in lapses.
	// TODO(upstream-contract): qurl-service accepts expires_at up to 365 days ahead.
	if input.entitlement == "" && input.expiresAt != nil {
		if lead := time.Until(*input.expiresAt); lead <= 0 || lead > 365*24*time.Hour {
			return exitcode.UsageError(errors.New("--expires-at must be in the future and within 365 days"))
		}
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
		if input.policy != nil {
			policy = *input.policy
		}
		err = owner.UpdateConsumerAccess(ctx, crid, input.version, policy)
		// TODO(upstream-contract): The SDK must confirm CRID, policy, and version before success.
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

func (o *globalOpts) applyConsumerRule(cmd *cobra.Command, input *consumerAccessInput, selector, file string) error {
	if !cmd.Flags().Changed(ruleFlag) && !cmd.Flags().Changed(ruleFileFlag) {
		return nil
	}
	if input.action != consumerConfigure {
		return exitcode.UsageError(errors.New("access rules apply only to configure"))
	}
	definition, err := o.selectedRule(cmd, selector, file)
	if err != nil {
		return err
	}
	if definition.Mode == accessrules.ModePublic {
		return exitcode.UsageError(errors.New("a public rule cannot change an existing resource's privacy"))
	}
	policy := definition.Policy(input.version + 1)
	input.policy = &policy
	input.rule = &definition
	return nil
}

// executeWithShareRule runs the action with the share's kept rule settings:
// an approval lasts as long as the rule says, and a new policy replaces or
// clears what the share kept.
func (o *globalOpts) executeWithShareRule(ctx context.Context, owner qurlapi.ConsumerManagementClient, crid string, input *consumerAccessInput) (*qurlapi.ConsumerManagementResult, error) {
	if err := o.defaultApprovalExpiry(ctx, crid, input); err != nil {
		return nil, err
	}
	result, err := executeConsumerAction(ctx, owner, crid, input)
	if err != nil || input.action != consumerConfigure {
		return result, err
	}
	if err := o.recordShareRule(ctx, crid, input.rule); err != nil {
		return nil, fmt.Errorf("configured %s, but could not save its access rule settings on this device: %w", crid, err)
	}
	return result, nil
}

// defaultApprovalExpiry gives a review approval or key opt-in the share's
// approval lifetime, unless --expires-at or a contract receipt says otherwise.
func (o *globalOpts) defaultApprovalExpiry(ctx context.Context, crid string, input *consumerAccessInput) error {
	if input.action != consumerApprove || input.expiresAt != nil || input.entitlement != "" {
		return nil
	}
	store, err := o.shareRuleStore()
	if err != nil {
		return ruleCommandError(err)
	}
	kept, ok, err := store.Get(ctx, crid)
	if err != nil {
		return ruleCommandError(err)
	}
	if lifetime, set := kept.ApprovalDuration(); ok && set {
		expiresAt := o.now().UTC().Add(lifetime).Truncate(time.Second)
		input.expiresAt = &expiresAt
	}
	return nil
}
