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
		definition, err := readRuleDefinition(cmd, opts, file)
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
	remove := &cobra.Command{Use: "delete <id>", Short: "Delete a saved rule; existing shares keep their access settings", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if deleteVersion < 0 {
			return exitcode.UsageError(errors.New("--version must be the current local access rules version"))
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
	remove.Flags().Int64Var(&deleteVersion, "version", -1, "current local access rules version")
	command.AddCommand(remove)
	command.AddCommand(&cobra.Command{Use: "sync", Short: "Synchronize saved rules with your linked account", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		snapshot, err := opts.syncRules(cmd.Context())
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}})
	var use string
	var resolveVersion int64
	resolve := &cobra.Command{Use: "resolve <id>", Short: "Choose the local or account edit for one conflicting rule", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if resolveVersion < 0 || !slices.Contains([]string{"local", "account"}, use) {
			return exitcode.UsageError(errors.New("use --use local or account and the current --version"))
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
	resolve.Flags().Int64Var(&resolveVersion, "version", -1, "current local access rules version")
	command.AddCommand(resolve)
	return command
}
func (o *globalOpts) ruleStore() (*connectorstate.AccessRuleStore, error) {
	dir, err := o.resolveShareStateDir("")
	if err != nil {
		return nil, err
	}
	return connectorstate.OpenAccessRules(dir, o.resolvedEndpoint)
}
func readRuleDefinition(cmd *cobra.Command, opts *globalOpts, file string) (accessrules.Definition, error) {
	if file != "-" {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("use --file - or --rule-file - and pipe one access rule as JSON"))
	}
	body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
	if err != nil {
		return accessrules.Definition{}, err
	}
	if len(body) > accessrules.MaxBytes {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("access rule JSON is too large"))
	}
	definition, err := accessrules.DecodeDefinition(body)
	if err != nil {
		return accessrules.Definition{}, exitcode.UsageError(err)
	}
	return definition, nil
}
func (o *globalOpts) selectedRule(cmd *cobra.Command, selector, file string) (accessrules.Definition, error) {
	if cmd.Flags().Changed(ruleFileFlag) {
		return readRuleDefinition(cmd, o, file)
	}
	if strings.TrimSpace(selector) == "" {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("--rule requires an access rule ID or exact name"))
	}
	store, err := o.ruleStore()
	if err != nil {
		return accessrules.Definition{}, ruleCommandError(err)
	}
	snapshot, err := store.Snapshot(cmd.Context())
	if err != nil {
		return accessrules.Definition{}, ruleCommandError(err)
	}
	var matches []accessrules.Definition
	for _, items := range [][]accessrules.Definition{snapshot.BuiltIns, snapshot.Items} {
		for _, d := range items {
			if d.ID == selector {
				return d, nil
			}
			if d.Name == selector {
				matches = append(matches, d)
			}
		}
	}
	if len(matches) != 1 {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("access rule was not found or its name is ambiguous; use an ID from qurl rules list"))
	}
	return matches[0], nil
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
	snapshot, err := store.Sync(ctx, client.AccessRules())
	if err != nil && snapshot.SchemaVersion != 1 {
		return accessrules.Snapshot{}, err
	}
	if err == nil || errors.Is(err, accessrules.ErrUnlinked) || errors.Is(err, accessrules.ErrConflict) {
		return snapshot, nil
	}
	if slices.Contains([]int{exitcode.Auth, exitcode.Unavailable, exitcode.RateLimited}, exitcode.FromError(err)) {
		return snapshot, nil
	}
	return accessrules.Snapshot{}, err
}

type accessRuleCommandError struct{ cause error }

func (e *accessRuleCommandError) Error() string { return e.UserMessage() }
func (e *accessRuleCommandError) Unwrap() error { return e.cause }
func (e *accessRuleCommandError) UserMessage() string {
	if errors.Is(e.cause, accessrules.ErrConflict) {
		return "Access rules changed. Reload the list before retrying."
	}
	if errors.Is(e.cause, accessrules.ErrAccountMismatch) {
		return "These access rules belong to another account or deployment. Use that profile."
	}
	return "Could not complete this access rules operation. Your saved rules are preserved."
}
func ruleCommandError(err error) error { return &accessRuleCommandError{cause: err} }

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

func (o *globalOpts) reconcileRulesAfterLink(ctx context.Context) {
	snapshot, err := o.syncRules(ctx)
	if !o.quiet && (err != nil || (snapshot.Sync != "synced" && snapshot.Sync != "local")) {
		o.printer().Warnf("Access rules will synchronize when the account is available. Local changes are preserved.")
	}
}

func ruleCreateCmd(opts *globalOpts) *cobra.Command {
	var mode, domain, contract, emailsFile string
	cmd := &cobra.Command{Use: "create <name>", Short: "Create a named access rule", Example: "  qurl rules create Team --mode keys\n  qurl rules create Colleagues --mode email_domain --email-domain example.com", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return ruleCommandError(err)
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		definition := accessrules.Definition{ID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), Name: args[0], Mode: mode, EmailDomain: domain, ContractID: contract}
		if cmd.Flags().Changed("emails-file") {
			if emailsFile != "-" {
				return exitcode.UsageError(errors.New("use --emails-file - to read a JSON array of additional email addresses"))
			}
			body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
			if err != nil {
				return err
			}
			if accessrules.StrictDecode(body, &definition.AllowedEmails) != nil || definition.AllowedEmails == nil {
				return exitcode.UsageError(errors.New("additional email addresses must be a JSON array"))
			}
		}
		if (cmd.Flags().Changed(consumerContract) && mode != consumerContract) || (cmd.Flags().Changed("email-domain") && mode != "email_domain") || (cmd.Flags().Changed("emails-file") && mode != "email_domain") || definition.Validate(false) != nil {
			return exitcode.UsageError(errors.New("choose a supported --mode and its required --email-domain or --contract settings"))
		}
		store, err := opts.ruleStore()
		if err != nil {
			return ruleCommandError(err)
		}
		return saveCreatedRule(cmd.Context(), opts, store, &definition)
	}}
	cmd.Flags().StringVar(&mode, modeFlag, "", "public, keys, invite_only, email_capture, email_domain, review or contract")
	cmd.Flags().StringVar(&domain, "email-domain", "", "lowercase email domain for domain access")
	cmd.Flags().StringVar(&contract, consumerContract, "", "integration reference for access approved by your service")
	cmd.Flags().StringVar(&emailsFile, "emails-file", "", "use - to read optional additional email addresses as a JSON array")
	return cmd
}

func saveCreatedRule(ctx context.Context, opts *globalOpts, store *connectorstate.AccessRuleStore, definition *accessrules.Definition) error {
	for range 3 {
		snapshot, err := store.Snapshot(ctx)
		if err != nil {
			return ruleCommandError(err)
		}
		snapshot, err = store.Save(ctx, snapshot.LocalVersion, definition)
		if errors.Is(err, accessrules.ErrConflict) {
			continue
		}
		if err != nil {
			return ruleCommandError(err)
		}
		return opts.printer().AccessRules(&snapshot)
	}
	return ruleCommandError(accessrules.ErrConflict)
}
