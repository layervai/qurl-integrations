package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const (
	modeFlag     = "mode"
	accessFlag   = "access"
	ruleFlag     = "rule"
	ruleFileFlag = "rule-file"
)

func rulesCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "rules", Short: "Manage saved access rules for future shares", Long: "Use ready-made access rules or save your own. Rules work locally before account linking. Changing or deleting a saved rule leaves existing shares unchanged.", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.AddCommand(ruleCreateCmd(opts))
	command.AddCommand(&cobra.Command{Use: "capabilities", Short: "Report supported access rules without signing in", Args: noArgs, RunE: func(_ *cobra.Command, _ []string) error { return opts.printer().AccessRuleCapabilities() }})
	command.AddCommand(&cobra.Command{Use: "list", Short: "List ready-made and saved access rules", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		snapshot, err := store.Snapshot(cmd.Context())
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}})
	var file string
	var saveVersion int64
	save := &cobra.Command{Use: "save", Short: "Save one custom rule from JSON on standard input", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if saveVersion < 0 || saveVersion >= accessrules.MaxVersion {
			return exitcode.UsageError(errors.New("--version must be the current local access rules version"))
		}
		definition, err := readRuleDefinition(cmd, opts, file, "--file")
		if err != nil {
			return err
		}
		if definition.Validate(false) != nil {
			return exitcode.UsageError(errors.New("ready-made rules cannot be edited; save a custom rule with its own UUID"))
		}
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		snapshot, err := store.Save(cmd.Context(), saveVersion, &definition)
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}}
	save.Flags().StringVar(&file, "file", "", "use - to read the access rule JSON from standard input")
	save.Flags().Int64Var(&saveVersion, "version", -1, "current local access rules version")
	command.AddCommand(save)
	var deleteVersion int64
	remove := &cobra.Command{Use: "delete <id-or-name>", Short: "Delete a saved rule; existing shares keep their access settings", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkOptionalRuleVersion(cmd, deleteVersion); err != nil {
			return err
		}
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		snapshot, err := store.Delete(cmd.Context(), deleteVersion, args[0])
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}}
	remove.Flags().Int64Var(&deleteVersion, "version", -1, "fail unless access rules are still at this version")
	command.AddCommand(remove)
	command.AddCommand(&cobra.Command{Use: "sync", Short: "Synchronize saved rules with your linked account", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return opts.runRulesSync(cmd.Context())
	}})
	var use string
	var resolveVersion int64
	resolve := &cobra.Command{Use: "resolve <id-or-name>", Short: "Choose the local or account edit for one conflicting rule", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !slices.Contains([]string{"local", "account"}, use) {
			return exitcode.UsageError(errors.New("use --use local or --use account"))
		}
		if err := checkOptionalRuleVersion(cmd, resolveVersion); err != nil {
			return err
		}
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		snapshot, err := store.Resolve(cmd.Context(), resolveVersion, args[0], use)
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}}
	resolve.Flags().StringVar(&use, "use", "", "local or account")
	resolve.Flags().Int64Var(&resolveVersion, "version", -1, "fail unless access rules are still at this version")
	command.AddCommand(resolve)
	return command
}

// checkOptionalRuleVersion allows an omitted --version, which acts on the
// current rules, but never a negative one.
func checkOptionalRuleVersion(cmd *cobra.Command, version int64) error {
	if cmd.Flags().Changed("version") && version < 0 {
		return exitcode.UsageError(errors.New("--version must be the current local access rules version"))
	}
	return nil
}

func (o *globalOpts) ruleStore() (*connectorstate.AccessRuleStore, error) {
	dir, err := o.resolveShareStateDir("")
	if err != nil {
		return nil, err
	}
	return connectorstate.OpenAccessRules(dir, o.resolvedEndpoint)
}
func readRuleDefinition(cmd *cobra.Command, opts *globalOpts, file, flag string) (accessrules.Definition, error) {
	if file != "-" {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("use " + flag + " - and pipe one access rule as JSON"))
	}
	body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
	if err != nil {
		return accessrules.Definition{}, err
	}
	if len(body) > accessrules.MaxBytes {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("access rule JSON is too large"))
	}
	var definition accessrules.Definition
	if connectorstate.DecodeStrictJSON(body, &definition) != nil || definition.Validate(strings.HasPrefix(definition.ID, "builtin.")) != nil {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("invalid access rule definition"))
	}
	return definition, nil
}
func (o *globalOpts) selectedRule(cmd *cobra.Command, selector, file string) (accessrules.Definition, error) {
	if cmd.Flags().Changed(ruleFileFlag) {
		return readRuleDefinition(cmd, o, file, "--"+ruleFileFlag)
	}
	if strings.TrimSpace(selector) == "" {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("--rule requires an access rule ID or exact name"))
	}
	// Built-in IDs need no saved state, so a damaged rules file cannot block them.
	if rule, err := accessrules.Find(selector, accessrules.BuiltIns()); err == nil && rule.ID == selector {
		return rule, nil
	}
	store, err := o.ruleStore()
	if err != nil {
		return accessrules.Definition{}, ruleCommandError(err)
	}
	snapshot, err := store.Snapshot(cmd.Context())
	if err != nil {
		return accessrules.Definition{}, ruleCommandError(err)
	}
	rule, err := accessrules.Find(selector, snapshot.BuiltIns, snapshot.Items)
	if err != nil {
		return accessrules.Definition{}, ruleCommandError(err)
	}
	return rule, nil
}

func (o *globalOpts) syncRules(ctx context.Context) (accessrules.Snapshot, error) {
	store, err := o.ruleStore()
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	var client qurlapi.Client
	var closeStore func() error
	switch {
	case o.registeredClient != nil:
		client = o.registeredClient
	case accountKeyConfigured(o.lookupEnv):
		key, keyErr := o.apiCredential()
		if keyErr != nil {
			return accessrules.Snapshot{}, keyErr
		}
		client, err = qurlapi.New(o.accountConfig(key, ""))
	default:
		dir, dirErr := o.resolveShareStateDir("")
		if dirErr != nil {
			return accessrules.Snapshot{}, dirErr
		}
		if !connectorstate.EnvelopePresent(dir) {
			return store.Snapshot(ctx)
		}
		if err := o.requireRuntimeSupervision(dir); err != nil {
			return accessrules.Snapshot{}, err
		}
		device, openErr := connectorstate.Open(dir)
		if openErr != nil {
			return accessrules.Snapshot{}, openErr
		}
		closeStore = device.Close
		handed, handoffErr := device.Handoff()
		if handoffErr != nil {
			_ = device.Close()
			return accessrules.Snapshot{}, handoffErr
		}
		client, err = o.openRegisteredDeviceClient(ctx, o.resolvedEndpoint, handed)
	}
	if closeStore != nil {
		defer func() { _ = closeStore() }()
	}
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	return store.Sync(ctx, client.AccessRules())
}

// runRulesSync keeps JSON output unchanged and adds one warning in text mode
// when rules stay on this device.
func (o *globalOpts) runRulesSync(ctx context.Context) error {
	snapshot, err := o.syncRules(ctx)
	notice, deferred := ruleSyncNotice(&snapshot, err)
	if !deferred {
		return ruleCommandError(err)
	}
	if notice != "" && !o.quiet && o.resolvedFormat != output.FormatJSON {
		o.printer().Warnf("%s", notice)
	}
	return o.printer().AccessRules(&snapshot)
}

// ruleSyncNotice explains a sync that kept saved rules pending on this device.
// deferred is false when err must fail the command instead.
func ruleSyncNotice(snapshot *accessrules.Snapshot, err error) (notice string, deferred bool) {
	if err != nil && snapshot.SchemaVersion != 1 {
		return "", false
	}
	switch {
	case errors.Is(err, accessrules.ErrUnlinked):
		return msgRulesUnlinked, true
	case errors.Is(err, accessrules.ErrConflict):
		return msgRulesBusy, true
	case err != nil:
		switch exitcode.FromError(err) {
		case exitcode.Auth:
			return msgRulesSignIn, true
		case exitcode.Unavailable, exitcode.RateLimited:
			return msgRulesUnavailable, true
		}
		return "", false
	case snapshot.Sync == accessrules.SyncConflict:
		return msgRulesConflict, true
	case snapshot.Sync == accessrules.SyncLocal:
		return msgRulesLocalOnly, true
	}
	return "", true
}

type accessRuleCommandError struct {
	message string
	cause   error
}

func (e *accessRuleCommandError) Error() string       { return e.message }
func (e *accessRuleCommandError) Unwrap() error       { return e.cause }
func (e *accessRuleCommandError) UserMessage() string { return e.message }

// ruleCommandError names the access-rule cause; other failures keep the
// CLI's standard rendering.
func ruleCommandError(err error) error {
	if message := ruleErrorMessage(err); message != "" {
		return &accessRuleCommandError{message: message, cause: err}
	}
	return err
}

func ruleErrorMessage(err error) string {
	var file *connectorstate.AccessRulesFileError
	switch {
	case errors.As(err, &file):
		return fmt.Sprintf(msgRulesFileDamaged, file.Path)
	case errors.Is(err, accessrules.ErrRuleConflict):
		return msgRulesRuleConflict
	case errors.Is(err, accessrules.ErrConflict):
		return msgRulesChanged
	case errors.Is(err, accessrules.ErrAccountMismatch):
		return msgRulesOtherAccount
	case errors.Is(err, accessrules.ErrRuleNotFound):
		return msgRulesNotFound
	case errors.Is(err, accessrules.ErrAmbiguousRule):
		return msgRulesAmbiguous
	case errors.Is(err, accessrules.ErrDuplicateName):
		return msgRulesDuplicateName
	case errors.Is(err, accessrules.ErrLibraryLimit):
		return msgRulesLimit
	}
	return ""
}

func validateRuleSelectionFlags(cmd *cobra.Command) error {
	selected := cmd.Flags().Changed(ruleFlag) || cmd.Flags().Changed(ruleFileFlag)
	if !selected {
		return nil
	}
	if cmd.Flags().Changed(ruleFlag) && cmd.Flags().Changed(ruleFileFlag) {
		return exitcode.UsageError(errors.New("choose --rule or --rule-file, not both"))
	}
	for _, name := range []string{"public", "private", "allow-device-key", "allow-requests", accessFlag, consumerContract, modeFlag} {
		if cmd.Flags().Lookup(name) != nil && cmd.Flags().Changed(name) {
			return exitcode.UsageError(errors.New("an access rule cannot be combined with other access settings"))
		}
	}
	return nil
}

// reconcileRulesAfterLink never fails the login; it warns when rules stay pending.
func (o *globalOpts) reconcileRulesAfterLink(ctx context.Context) {
	snapshot, err := o.syncRules(ctx)
	notice, deferred := ruleSyncNotice(&snapshot, err)
	if !deferred {
		if notice = ruleErrorMessage(err); notice == "" {
			notice = msgRulesNotSynced
		}
	}
	if notice != "" && !o.quiet {
		o.printer().Warnf("%s", notice)
	}
}

func ruleCreateCmd(opts *globalOpts) *cobra.Command {
	var mode, domain, contract, emailsFile string
	cmd := &cobra.Command{Use: "create <name>", Short: "Create a named access rule", Example: "  qurl rules create Team --mode keys\n  qurl rules create Colleagues --mode email_domain --email-domain example.com", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed(consumerContract) && mode != accessrules.ModeContract {
			return exitcode.UsageError(errors.New("--contract applies only to --mode contract"))
		}
		if (cmd.Flags().Changed("email-domain") || cmd.Flags().Changed("emails-file")) && mode != accessrules.ModeEmailDomain {
			return exitcode.UsageError(errors.New("--email-domain and --emails-file apply only to --mode email_domain"))
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		definition := accessrules.Definition{ID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), Name: strings.TrimSpace(args[0]), Mode: mode, EmailDomain: strings.ToLower(strings.TrimSpace(domain)), ContractID: contract}
		if cmd.Flags().Changed("emails-file") {
			if emailsFile != "-" {
				return exitcode.UsageError(errors.New("use --emails-file - to read a JSON array of additional email addresses"))
			}
			body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
			if err != nil {
				return err
			}
			if connectorstate.DecodeStrictJSON(body, &definition.AllowedEmails) != nil || definition.AllowedEmails == nil {
				return exitcode.UsageError(errors.New("additional email addresses must be a JSON array"))
			}
			for i, email := range definition.AllowedEmails {
				definition.AllowedEmails[i] = normalizeEmail(email)
			}
		}
		if definition.Validate(false) != nil {
			return exitcode.UsageError(ruleInputError(&definition))
		}
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		if _, err := store.Save(cmd.Context(), -1, &definition); err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRule(&definition)
	}}
	cmd.Flags().StringVar(&mode, modeFlag, "", "public, keys, invite_only, email_capture, email_domain, review or contract")
	cmd.Flags().StringVar(&domain, "email-domain", "", "email domain for domain access, such as example.com")
	cmd.Flags().StringVar(&contract, consumerContract, "", "integration reference for Custom access, where your service decides who gets in")
	cmd.Flags().StringVar(&emailsFile, "emails-file", "", "use - to read optional additional email addresses as a JSON array")
	return cmd
}

// normalizeEmail trims an address and lowercases its domain; the part before
// the @ keeps its case.
func normalizeEmail(email string) string {
	email = strings.TrimSpace(email)
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		return email[:at+1] + strings.ToLower(email[at+1:])
	}
	return email
}

// ruleInputError names the first invalid part of a rule typed on the command line.
func ruleInputError(d *accessrules.Definition) error {
	validWith := func(change func(*accessrules.Definition)) bool {
		probe := *d
		change(&probe)
		return probe.Validate(false) == nil
	}
	switch {
	case !slices.Contains(accessrules.Modes(), d.Mode):
		return errors.New("--mode must be one of: " + strings.Join(accessrules.Modes(), ", "))
	case !validWith(func(p *accessrules.Definition) {
		*p = accessrules.Definition{ID: p.ID, Name: p.Name, Mode: accessrules.ModePublic}
	}):
		return errors.New("rule names must be 1 to 120 characters, without control characters")
	case d.Mode == accessrules.ModeEmailDomain && !validWith(func(p *accessrules.Definition) { p.AllowedEmails = nil }):
		return errors.New("--email-domain must be a domain such as example.com")
	case d.Mode == accessrules.ModeEmailDomain:
		return errors.New("--emails-file must list each email address once, such as name@example.com")
	default:
		return errors.New("--contract must be 1 to 128 letters, digits, hyphens or underscores")
	}
}
