package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/layervai/qurl-go/qurl"

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
	// emailsFileFlag reads a rule's additional email addresses.
	emailsFileFlag = "emails-file"
)

func rulesCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "rules", Short: "Manage access rules for future shares", Long: "Share with a default access rule or save your own. Rules work locally before account linking. Changing or deleting a saved rule leaves existing shares unchanged.", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.AddCommand(ruleCreateCmd(opts))
	command.AddCommand(&cobra.Command{Use: "capabilities", Short: "Report supported access rules as JSON without signing in", Args: noArgs, RunE: func(_ *cobra.Command, _ []string) error { return opts.printer().AccessRuleCapabilities() }})
	command.AddCommand(&cobra.Command{Use: "list", Short: "List the default access rules and your saved rules", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return opts.printRuleStore(func(store *connectorstate.AccessRuleStore) (accessrules.Snapshot, error) {
			return store.Snapshot(cmd.Context())
		})
	}})
	command.AddCommand(ruleSaveCmd(opts), ruleDeleteCmd(opts))
	command.AddCommand(&cobra.Command{Use: "sync", Short: "Synchronize saved rules with your linked account; read the JSON sync field, since a deferred sync exits 0", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return opts.runRulesSync(cmd.Context())
	}})
	command.AddCommand(ruleResolveCmd(opts))
	return command
}

func ruleSaveCmd(opts *globalOpts) *cobra.Command {
	var file string
	var version int64
	cmd := &cobra.Command{Use: "save", Short: "Save one custom rule from JSON on standard input", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if version < 0 || version >= accessrules.MaxVersion {
			return exitcode.UsageError(errors.New("--version must be the current local access rules version"))
		}
		definition, err := readRuleDefinition(cmd, opts, file, "--file")
		if err != nil {
			return err
		}
		if accessrules.IsPresetID(definition.ID) {
			return exitcode.UsageError(errors.New("ready-made rules cannot be edited; save a custom rule with its own UUID"))
		}
		return opts.printRuleStore(func(store *connectorstate.AccessRuleStore) (accessrules.Snapshot, error) {
			return store.Save(cmd.Context(), version, &definition)
		})
	}}
	cmd.Flags().StringVar(&file, "file", "", "use - to read the access rule JSON from standard input")
	cmd.Flags().Int64Var(&version, "version", -1, "current local access rules version")
	return cmd
}

func ruleDeleteCmd(opts *globalOpts) *cobra.Command {
	var version int64
	cmd := &cobra.Command{Use: "delete <id-or-name>", Short: "Delete a saved rule; existing shares keep their access settings", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkOptionalRuleVersion(cmd, version); err != nil {
			return err
		}
		return opts.printRuleStore(func(store *connectorstate.AccessRuleStore) (accessrules.Snapshot, error) {
			return store.Delete(cmd.Context(), version, args[0])
		})
	}}
	cmd.Flags().Int64Var(&version, "version", -1, "fail unless access rules are still at this version")
	return cmd
}

func ruleResolveCmd(opts *globalOpts) *cobra.Command {
	var use string
	var version int64
	cmd := &cobra.Command{Use: "resolve <id-or-name>", Short: "Choose the local or account edit for one conflicting rule", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !slices.Contains([]string{accessrules.UseLocal, accessrules.UseAccount}, use) {
			return exitcode.UsageError(errors.New("use --use local or --use account"))
		}
		if err := checkOptionalRuleVersion(cmd, version); err != nil {
			return err
		}
		return opts.printRuleStore(func(store *connectorstate.AccessRuleStore) (accessrules.Snapshot, error) {
			return store.Resolve(cmd.Context(), version, args[0], use)
		})
	}}
	cmd.Flags().StringVar(&use, "use", "", "local or account")
	cmd.Flags().Int64Var(&version, "version", -1, "fail unless access rules are still at this version")
	return cmd
}

// checkOptionalRuleVersion allows an omitted --version, which acts on the
// current rules, but never a negative one.
func checkOptionalRuleVersion(cmd *cobra.Command, version int64) error {
	if cmd.Flags().Changed("version") && version < 0 {
		return exitcode.UsageError(errors.New("--version must be the current local access rules version"))
	}
	return nil
}

// printRuleStore runs one store operation and prints the resulting snapshot.
func (o *globalOpts) printRuleStore(do func(*connectorstate.AccessRuleStore) (accessrules.Snapshot, error)) error {
	store, err := o.ruleStore()
	if err != nil {
		return ruleCommandError(err)
	}
	snapshot, err := do(store)
	if err != nil {
		return ruleCommandError(err)
	}
	return o.printer().AccessRules(&snapshot)
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
	// readRequestBody is the CLI's bounded stdin reader; the method only names the body in its errors.
	body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
	if err != nil {
		return accessrules.Definition{}, err
	}
	if len(body) > accessrules.MaxBytes {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("access rule JSON is too large"))
	}
	var definition accessrules.Definition
	if connectorstate.DecodeStrictJSON(body, &definition) != nil {
		return accessrules.Definition{}, exitcode.UsageError(errors.New("invalid access rule definition"))
	}
	if err := definition.Validate(accessrules.IsPresetID(definition.ID)); err != nil {
		return accessrules.Definition{}, exitcode.UsageError(fmt.Errorf("invalid access rule definition: %w", err))
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
	// Presets resolve before saved rules and need no saved state, so a damaged
	// rules file cannot block them. NameTaken keeps local saves off their names,
	// but a rule synced from another client can share one; that name is then
	// ambiguous and the rule's ID or the preset's ID picks.
	preset, err := accessrules.Find(selector, []accessrules.Definition{accessrules.OnlyFirstPerson()}, accessrules.BuiltIns())
	if err == nil {
		if selector != preset.ID && o.savedRuleNamed(cmd, selector) {
			return accessrules.Definition{}, ruleCommandError(accessrules.ErrAmbiguousRule)
		}
		return preset, nil
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

// savedRuleNamed reports whether a readable saved rule is named exactly name,
// matching Find. An unreadable rules file leaves the preset chosen, so it
// cannot block presets.
func (o *globalOpts) savedRuleNamed(cmd *cobra.Command, name string) bool {
	// No profile directory means nothing saved; do not create one here.
	dir, err := o.resolveShareStateDir("")
	if err != nil {
		return false
	}
	if _, err = os.Stat(dir); err != nil {
		return false
	}
	store, err := o.ruleStore()
	var snapshot accessrules.Snapshot
	if err == nil {
		snapshot, err = store.Snapshot(cmd.Context())
	}
	if err != nil {
		if log := o.verboseLogger(); log != nil {
			log("saved access rules are unreadable; using the default rule named %q: %v", name, err)
		}
		return false
	}
	return slices.ContainsFunc(snapshot.Items, func(d accessrules.Definition) bool { return d.Name == name })
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
		// Through Sync, so a client that cannot reach the account records
		// the journal as offline like any other deferred sync.
		return store.Sync(ctx, unreachableRules{err})
	}
	return store.Sync(ctx, client.AccessRules())
}

// unreachableRules is the account library when its client could not open.
type unreachableRules struct{ err error }

func (u unreachableRules) Load(context.Context) (accessrules.AccountLibrary, error) {
	return accessrules.AccountLibrary{}, u.err
}

func (u unreachableRules) Replace(context.Context, int64, []accessrules.Definition) (accessrules.AccountLibrary, error) {
	return accessrules.AccountLibrary{}, u.err
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
	// A zero snapshot means the journal was never read, so nothing stays pending.
	if journalRead := snapshot.SchemaVersion != 0; err != nil && !journalRead {
		return "", false
	}
	switch {
	case errors.Is(err, accessrules.ErrUnlinked):
		return msgRulesUnlinked, true
	// ErrRuleConflict wraps ErrConflict, but Sync reports rule conflicts in the
	// snapshot, never as this error.
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
		if file.LinkLimits {
			return fmt.Sprintf(msgLinkLimitsFileDamaged, file.Path)
		}
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
	for _, name := range []string{"public", "private", "allow-device-key", "allow-requests", accessFlag, consumerEmailDomainFlag, consumerContract, modeFlag, stripeAccountFlag, stripePriceFlag, paymentAccessSecondsFlag} {
		if cmd.Flags().Lookup(name) != nil && cmd.Flags().Changed(name) {
			return exitcode.UsageError(errors.New("an access rule cannot be combined with other access settings"))
		}
	}
	return nil
}

// reconcileRulesAfterLink never fails the login; it warns when rules stay pending.
func (o *globalOpts) reconcileRulesAfterLink(ctx context.Context) {
	// Bounded so a slow service cannot hold up a login that already succeeded.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot, err := o.syncRules(ctx)
	notice, deferred := ruleSyncNotice(&snapshot, err)
	if !deferred {
		if notice = ruleErrorMessage(err); notice == "" {
			notice = msgRulesNotSynced
		}
	}
	if exitcode.FromError(err) == exitcode.Auth {
		// "Sign in again" would be wrong right after signing in.
		notice = msgRulesLoginNotSynced
	}
	if snapshot.SchemaVersion == 0 {
		// Sync stopped before reading the journal; read it here so a device
		// with nothing saved stays quiet.
		// A fresh budget: the sync may have spent the shared one.
		readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		if store, storeErr := o.ruleStore(); storeErr == nil {
			if local, readErr := store.Snapshot(readCtx); readErr == nil {
				snapshot = local
			}
		}
		cancelRead()
	}
	// A never-linked journal with no saved rules has nothing waiting, so any
	// sync failure stays quiet; a linked or unreadable journal still warns.
	if snapshot.Sync == accessrules.SyncLocal && len(snapshot.Items) == 0 && len(snapshot.Conflicts) == 0 {
		notice = ""
	}
	if notice != "" && !o.quiet {
		o.printer().Warnf("%s", notice)
	}
}

// modeChoices lists --mode values in the order every client offers them.
var modeChoices = output.AccessModeChoices()

// approvalsLast maps --approvals-last choices to approval_expires_in; never is empty.
var approvalsLast = map[string]string{"1d": "1d", "7d": "7d", "30d": "30d", "90d": "90d", "never": ""}

// ruleSettingFlags are the rule settings typed on the command line.
type ruleSettingFlags struct {
	expiresIn, sessionLength, approvals string
	oneBrowser                          bool
}

func (f *ruleSettingFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.expiresIn, "expires-in", "", "public rules: each shared link expires after this long, such as 1h or 7d (1m to 30d)")
	cmd.Flags().BoolVar(&f.oneBrowser, "once", false, "public rules: one-time links; only the first person to open each link gets in, anyone else sees that it no longer works (alias --one-browser)")
	cmd.Flags().StringVar(&f.sessionLength, "stays-open-for", "", "public rules: how long they can use it after opening the link, such as 30m (1s to 24h) (alias --session-length)")
	cmd.Flags().StringVar(&f.approvals, "approvals-last", "", "review and keys rules: 1d, 7d, 30d, 90d or never (default: never)")
	// The earlier names stay accepted, and named in help, for released clients.
	// Wrap, not replace, any normalizer the flag set already has.
	previous := cmd.Flags().GetNormalizeFunc()
	cmd.Flags().SetNormalizeFunc(func(set *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "one-browser":
			name = "once"
		case "session-length":
			name = "stays-open-for"
		}
		return previous(set, name)
	})
}

// apply validates the settings for d.Mode and copies them into d.
func (f *ruleSettingFlags) apply(cmd *cobra.Command, d *accessrules.Definition) error {
	changed := func(names ...string) bool {
		return slices.ContainsFunc(names, func(name string) bool { return cmd.Flags().Changed(name) })
	}
	if changed("expires-in", "once", "stays-open-for") {
		if d.Mode != accessrules.ModePublic {
			return errors.New("--expires-in, --once and --stays-open-for apply only to --mode public")
		}
		if !changed("expires-in") {
			return errors.New("link limits need --expires-in, such as --expires-in 1h")
		}
		if _, ok := accessrules.LinkExpiry.Parse(f.expiresIn); !ok {
			return errors.New("--expires-in must be from 1m to 30d, such as 1h or 7d")
		}
		if changed("stays-open-for") && !accessrules.ValidSessionLength(f.sessionLength) {
			return errors.New("--stays-open-for must be whole seconds from 1s to 24h, such as 30m")
		}
		// Only flags given are stored, so an unset limit stays absent.
		d.LinkLimits = &accessrules.LinkLimits{ExpiresIn: f.expiresIn}
		if changed("once") {
			d.LinkLimits.OneTimeUse = new(f.oneBrowser)
		}
		if changed("stays-open-for") {
			d.LinkLimits.SessionDuration = new(f.sessionLength)
		}
	}
	if changed("approvals-last") {
		if !accessrules.ApprovalsApply(d.Mode) {
			return errors.New("--approvals-last applies only to --mode review or keys")
		}
		value, ok := approvalsLast[f.approvals]
		if !ok {
			return errors.New("--approvals-last must be one of: 1d, 7d, 30d, 90d, never")
		}
		if value != "" {
			d.ApprovalExpiresIn = new(value)
		}
	}
	return nil
}

func ruleCreateCmd(opts *globalOpts) *cobra.Command {
	var mode, domain, contract, emailsFile, price string
	var accessSeconds int64
	var settings ruleSettingFlags
	cmd := &cobra.Command{Use: "create <name>", Short: "Create a named access rule", Long: `Create a named access rule for future shares.

Who can open it (--mode):
` + output.AccessModeHelp() + `

Public rules with --expires-in share a link that expires instead of the
item's permanent address; qurl share makes later links with the same limits.
Approvals last (review and keys rules) is how long each approval lasts.
A contract rule with --stripe-price is People who pay: sharing with it uses
the Stripe account connected then (qurl payments account).`, Example: `  qurl rules create "Acme team" --mode email_domain --email-domain acme.com
  qurl rules create "Buyers" --mode contract --contract buyers --stripe-price price_123
  qurl rules create "Quick look" --mode public --expires-in 1h --stays-open-for 30m
  qurl rules create "Client review" --mode review --approvals-last 7d
  qurl rules create "Lab devices" --mode keys --approvals-last 30d`, Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (cmd.Flags().Changed(consumerContract) || cmd.Flags().Changed(stripePriceFlag) || cmd.Flags().Changed(paymentAccessSecondsFlag)) && mode != accessrules.ModeContract {
			return exitcode.UsageError(errors.New("--contract, --stripe-price and --payment-access-seconds apply only to --mode contract"))
		}
		if (cmd.Flags().Changed(consumerEmailDomainFlag) || cmd.Flags().Changed(emailsFileFlag)) && mode != accessrules.ModeEmailDomain {
			return exitcode.UsageError(errors.New("--email-domain and --emails-file apply only to --mode email_domain"))
		}
		id, err := newRuleID()
		if err != nil {
			return err
		}
		definition := accessrules.Definition{ID: id, Name: strings.TrimSpace(args[0]), Mode: mode, EmailDomain: strings.ToLower(strings.TrimSpace(domain)), ContractID: contract}
		if cmd.Flags().Changed(emailsFileFlag) {
			emails, err := readAllowedEmails(cmd, opts, emailsFile)
			if err != nil {
				return err
			}
			definition.AllowedEmails = emails
		}
		if err := settings.apply(cmd, &definition); err != nil {
			return exitcode.UsageError(err)
		}
		if cmd.Flags().Changed(stripePriceFlag) {
			definition.Payment = &accessrules.Payment{PriceID: price, AccessSeconds: accessSeconds}
		} else if cmd.Flags().Changed(paymentAccessSecondsFlag) {
			return exitcode.UsageError(errors.New("--payment-access-seconds needs --stripe-price"))
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
	cmd.Flags().StringVar(&mode, modeFlag, "", modeChoices)
	cmd.Flags().StringVar(&domain, consumerEmailDomainFlag, "", "email domain for domain access, such as acme.com")
	cmd.Flags().StringVar(&contract, consumerContract, "", "integration reference for Custom access, where your service decides who gets in")
	cmd.Flags().StringVar(&emailsFile, emailsFileFlag, "", "use - to read optional additional email addresses as a JSON array")
	cmd.Flags().StringVar(&price, stripePriceFlag, "", "contract rules: Stripe price people pay; LayerV collects 3%")
	cmd.Flags().Int64Var(&accessSeconds, paymentAccessSecondsFlag, 0, "contract rules: one-time access duration; zero lasts for the resource lifetime; subscriptions require zero")
	settings.register(cmd)
	return cmd
}

// newRuleID returns a random (version 4, RFC 4122 variant) UUID; no UUID
// package is in the module graph.
func newRuleID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

// readAllowedEmails reads --emails-file - as a JSON array of normalized addresses.
func readAllowedEmails(cmd *cobra.Command, opts *globalOpts, file string) ([]string, error) {
	if file != "-" {
		return nil, exitcode.UsageError(errors.New("use --emails-file - to read a JSON array of additional email addresses"))
	}
	body, err := readRequestBody(cmd.Context(), opts.streams, http.MethodPut)
	if err != nil {
		return nil, err
	}
	if len(body) > accessrules.MaxBytes {
		return nil, exitcode.UsageError(errors.New("the email address list is too large"))
	}
	var emails []string
	if connectorstate.DecodeStrictJSON(body, &emails) != nil || emails == nil {
		return nil, exitcode.UsageError(errors.New("additional email addresses must be a JSON array"))
	}
	for i, email := range emails {
		emails[i] = normalizeEmail(email)
	}
	return emails, nil
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
		return errors.New("--mode must be one of: " + modeChoices)
	case !validWith(func(p *accessrules.Definition) {
		*p = accessrules.Definition{ID: p.ID, Name: p.Name, Mode: accessrules.ModePublic}
	}):
		return errors.New("rule names must be 1 to 120 characters, without control characters")
	case d.Mode == accessrules.ModeEmailDomain && !validWith(func(p *accessrules.Definition) { p.AllowedEmails = nil }):
		return errors.New("--email-domain must be a domain such as acme.com")
	case d.Mode == accessrules.ModeEmailDomain:
		return errors.New("--emails-file must list each email address once, such as name@acme.com")
	// Valid without the payment, so the payment is what is wrong.
	case d.Payment != nil && validWith(func(p *accessrules.Definition) { p.Payment = nil }):
		return errors.New("--stripe-price must be a Stripe price ID such as price_123, and --payment-access-seconds from 0 to 315360000")
	case d.Mode == accessrules.ModeContract:
		return errors.New("--contract must be 1 to 128 letters, digits, hyphens or underscores")
	default:
		return fmt.Errorf("invalid access rule: %w", d.Validate(false))
	}
}

// completeRulePayment fills in a paid rule's Stripe account, read like qurl
// payments account; a saved rule stores only the price.
func completeRulePayment(ctx context.Context, client qurlapi.Client, policy *qurl.ConsumerAccessPolicy) error {
	if policy == nil || policy.Payment == nil || policy.Payment.AccountID != "" {
		return nil
	}
	payments, err := publisherPayments(client)
	if err != nil {
		return err
	}
	account, err := payments.PublisherStripeAccount(ctx)
	if code := exitcode.FromError(err); code == exitcode.Unavailable || code == exitcode.RateLimited {
		return &accessRuleCommandError{message: msgRulesPaymentsUnavailable, cause: err}
	}
	if err != nil {
		return err
	}
	if account == "" {
		return exitcode.InvalidInputError(msgRulesStripeNotConnected, nil)
	}
	return setStripePayment(policy, account, policy.Payment.PriceID, policy.Payment.AccessSeconds)
}

// applyRuleToShare runs once the resource exists. A new public share keeps
// the rule's link limits for later links and gets a temporary link minted with
// them in published.LimitedLink. A share that already existed keeps what it
// was created with, with or without a rule, so publishing it again mints a
// fresh link with its saved limits; published.OtherSettings says when the
// chosen rule differs.
func (o *globalOpts) applyRuleToShare(ctx context.Context, client qurlapi.Client, rule *accessrules.Definition, published *qurlapi.Published) error {
	limits, err := o.ruleLinkLimits(ctx, rule, published)
	if err != nil {
		return err
	}
	return o.mintRuleLink(ctx, client, rule, published, limits)
}

// ruleLinkLimits saves a new share's limits, or reads an existing share's, and
// returns the limits to mint with; nil mints nothing. Local publish calls it
// before serving, so a failed save can still turn the new share back off.
func (o *globalOpts) ruleLinkLimits(ctx context.Context, rule *accessrules.Definition, published *qurlapi.Published) (*accessrules.LinkLimits, error) {
	existing := published.FoundExisting != nil && *published.FoundExisting
	var kept *accessrules.LinkLimits
	var keptErr error
	keptRead := false
	if published.FoundExisting == nil && rule != nil && rule.LinkLimits != nil {
		// An unproven outcome never overwrites limits this share already has.
		kept, keptErr = o.savedLinkLimits(ctx, published.CRID)
		keptRead, existing = true, keptErr != nil || kept != nil
	}
	if rule == nil && !existing {
		return nil, nil //nolint:nilnil // nothing to mint
	}
	id := published.CRID
	var limits *accessrules.LinkLimits
	if rule != nil {
		limits = rule.LinkLimits
	}
	if existing {
		if !keptRead {
			kept, keptErr = o.savedLinkLimits(ctx, id)
		}
		err := keptErr
		if err != nil && rule == nil {
			// A plain republish never asked for rules; it still succeeds.
			o.printer().Warnf(msgRulesLimitsUnread, id, err)
			return nil, nil //nolint:nilnil // nothing to mint
		}
		if err != nil {
			return nil, err
		}
		if rule != nil {
			// Only approvals can differ here (Publish refuses any other policy
			// change); SameDefinition compares them in the service's stored form.
			policy := rule.Policy(1)
			received := policy
			received.ApprovalExpiresIn = ""
			if published.ConsumerAccess != nil {
				received.ApprovalExpiresIn = published.ConsumerAccess.ApprovalExpiresIn
			}
			published.OtherSettings = !accessrules.SameEffectiveLinkLimits(kept, limits) || !received.SameDefinition(policy)
		}
		limits = kept
	} else if limits != nil {
		store, err := o.shareRuleStore()
		if err == nil {
			err = store.Set(ctx, id, limits)
		}
		if err != nil {
			return nil, fmt.Errorf("%s was published, but its link limits could not be saved on this device: %w", id, err)
		}
	}
	return limits, nil
}

// mintRuleLink mints and verifies the link with limits into published.LimitedLink.
func (o *globalOpts) mintRuleLink(ctx context.Context, client qurlapi.Client, rule *accessrules.Definition, published *qurlapi.Published, limits *accessrules.LinkLimits) error {
	if limits == nil {
		return nil
	}
	id := published.CRID
	// The same checks share applies before any link is printed.
	link, err := client.MintLimitedLink(ctx, id, *limits)
	if err = verifyShareLink(id, link, err); err == nil {
		err = o.verifyLink(ctx, link.QURL, id)
	}
	if err != nil {
		// API errors render on their own, so the CRID and next step are said first.
		o.printer().Warnf(msgRulesLinkNotMade, id)
		if rule == nil {
			// A plain republish never asked for a link; it still succeeds.
			return nil
		}
		return err
	}
	published.LimitedLink = link
	return nil
}

// savedLinkLimits is what a share on this device was created with; nil if nothing.
func (o *globalOpts) savedLinkLimits(ctx context.Context, id string) (*accessrules.LinkLimits, error) {
	dir, err := o.resolveShareStateDir("")
	if errors.Is(err, connectorstate.ErrNoDefaultStateDir) {
		// Without a profile directory nothing was saved on this device.
		return nil, nil //nolint:nilnil // no profile means no saved limits
	}
	if err != nil {
		return nil, err
	}
	store, err := connectorstate.OpenShareRules(dir)
	if err != nil {
		return nil, ruleCommandError(err)
	}
	limits, err := store.Get(ctx, id)
	if err != nil {
		return nil, ruleCommandError(err)
	}
	return limits, nil
}

func forgetLinkLimits(ctx context.Context, dir, id string) error {
	store, err := connectorstate.OpenShareRules(dir)
	if err == nil {
		err = store.Set(ctx, id, nil)
	}
	return err
}

func (o *globalOpts) shareRuleStore() (*connectorstate.ShareRuleStore, error) {
	dir, err := o.resolveShareStateDir("")
	if err != nil {
		return nil, err
	}
	return connectorstate.OpenShareRules(dir)
}
