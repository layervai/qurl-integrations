package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	domainGetOperation        = "get"
	domainRegisterOperation   = "add"
	domainDeleteOperation     = "delete"
	domainVerifyOperation     = "verify"
	domainRegenerateOperation = "regenerate-token"
)

func domainsCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "domains", Short: "Manage custom domains with fresh browser account sign-in", Args: noArgs,
		Long: "Manage domains owned by the signed-in account. For automation, select --api-key-auth with an explicit durable account key. Registration requires an enterprise plan. Tokens stay in memory. Writes make one HTTP attempt. DNS records are instructions for you to configure; these commands do not change your DNS provider. Verification runs once and does not wait for propagation. Inspect status and checks before using the domain.", RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.PersistentFlags().BoolVar(&opts.accountAPIKeyAuth, "api-key-auth", false, "Use an explicit QURL_API_KEY_FILE or QURL_API_KEY for account automation")
	var limit int
	var cursor string
	list := &cobra.Command{Use: accountListOperation, Short: "List one page of domain status", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := qurlapi.ValidateAccountPage(limit); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		page, err := client.ListDomains(cmd.Context(), limit, cursor)
		if err != nil {
			return err
		}
		return opts.printer().DomainPage(page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "page size (1–100)")
	list.Flags().StringVar(&cursor, "cursor", "", "opaque cursor from the previous page")
	command.AddCommand(list)
	for _, operation := range []string{domainGetOperation, domainRegisterOperation, domainDeleteOperation, domainVerifyOperation, domainRegenerateOperation} {
		command.AddCommand(domainActionCmd(opts, operation))
	}
	return command
}

func domainActionCmd(opts *globalOpts, operation string) *cobra.Command {
	descriptions := map[string]string{
		domainGetOperation:        "Read current domain status and DNS instructions",
		domainRegisterOperation:   "Register an enterprise custom domain and print DNS instructions",
		domainDeleteOperation:     "Delete a custom domain",
		domainVerifyOperation:     "Check DNS once and print the actual results",
		domainRegenerateOperation: "Replace the DNS verification token and print new instructions",
	}
	command := &cobra.Command{Use: operation + " DOMAIN", Short: descriptions[operation], Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := qurlapi.NormalizeAccountDomain(args[0])
		if err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), operation != domainGetOperation)
		if err != nil {
			return err
		}
		err = runDomainAction(opts, cmd, client, operation, name)
		if err != nil && operation != domainGetOperation {
			return domainWriteFailure(name, err)
		}
		return err
	}}
	if operation == domainRegenerateOperation {
		command.Long = "Immediately invalidates the previous DNS verification token. Update the TXT record with the returned token, then run domains verify. Only pending_verification and failed domains allow this action. If the result is uncertain, use domains get to read the current token before another write."
	}
	if operation == domainVerifyOperation {
		command.Long = "Check TXT, ACME CNAME and traffic routing once. A successful command means the API returned the checks; inspect their values and domain status for readiness. This command does not wait for DNS propagation. If the token has expired, use domains regenerate-token and update the TXT record first."
	}
	return command
}

func runDomainAction(opts *globalOpts, cmd *cobra.Command, client qurlapi.AccountManagement, operation, name string) error {
	switch operation {
	case domainDeleteOperation:
		if err := client.DeleteAccountDomain(cmd.Context(), name); err != nil {
			return err
		}
		return opts.printer().DomainDeleted(name)
	case domainVerifyOperation:
		result, err := client.VerifyAccountDomain(cmd.Context(), name)
		if err != nil {
			return err
		}
		return opts.printer().DomainVerification(result)
	case domainGetOperation, domainRegisterOperation, domainRegenerateOperation:
		var item *qurlapi.AccountDomain
		var err error
		switch operation {
		case domainGetOperation:
			item, err = client.GetAccountDomain(cmd.Context(), name)
		case domainRegisterOperation:
			item, err = client.RegisterAccountDomain(cmd.Context(), name)
		case domainRegenerateOperation:
			item, err = client.RegenerateDomainToken(cmd.Context(), name)
		}
		if err != nil {
			return err
		}
		return opts.printer().Domain(item)
	default:
		return errors.New("unsupported domain action")
	}
}

type domainWriteResultError struct {
	domain string
	cause  error
}

func (e *domainWriteResultError) Error() string { return e.UserMessage() }
func (e *domainWriteResultError) Unwrap() error { return e.cause }
func (e *domainWriteResultError) UserMessage() string {
	return fmt.Sprintf("Domain write outcome is uncertain. Run qurl domains get %s to inspect current state before another write.", e.domain)
}
func domainWriteFailure(name string, cause error) error {
	var apiError *qurlapi.Error
	if errors.As(cause, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 {
		return cause
	}
	return &domainWriteResultError{domain: name, cause: cause}
}
