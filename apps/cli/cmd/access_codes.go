package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func accessCodesCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "access-codes", Short: "Create, list, revoke or redeem access codes", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }, Long: "Account management requires a system-tier account that owns the resource. Sign in with the browser by default, or select --api-key-auth with an explicit durable account key. Redemption is public and sends no account credential. Codes are secrets; never pass them as command arguments. Writes and redemption make one HTTP attempt."}
	command.PersistentFlags().BoolVar(&opts.accountAPIKeyAuth, "api-key-auth", false, "Use an explicit QURL_API_KEY_FILE or QURL_API_KEY for account management")
	var limit int
	var cursor string
	list := &cobra.Command{Use: accountListOperation, Short: "List one page of access code metadata", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := qurlapi.ValidateAccountPage(limit); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		page, err := client.ListAccessCodes(cmd.Context(), limit, cursor)
		if err != nil {
			return err
		}
		return opts.printer().AccessCodePage(page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "page size (1–100)")
	list.Flags().StringVar(&cursor, "cursor", "", "opaque cursor from the previous page")
	remove := &cobra.Command{Use: "delete ACCESS_CODE_ID", Short: "Revoke an access code by its nonsecret acd_ ID", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateAccessCodeID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), true)
		if err != nil {
			return err
		}
		if err = client.DeleteAccessCode(cmd.Context(), args[0]); err != nil {
			return accessCodeWriteFailure(err)
		}
		return opts.printer().AccessCodeDeleted(args[0])
	}}
	command.AddCommand(list, accessCodeCreateCmd(opts), remove, accessCodeRedeemCmd(opts))
	return command
}

func accessCodeCreateCmd(opts *globalOpts) *cobra.Command {
	var input qurlapi.AccessCodeInput
	var expiry string
	command := &cobra.Command{Use: "create RESOURCE", Short: "Create a code and print its secret once", Args: exactArgs(1), Long: "Create a code for an active resource owned by the system-tier account. RESOURCE is a public resource key or CRID. Save stdout securely: the secret is shown once. If creation or output fails, list codes and revoke the known unused record before creating another. The CLI does not retry or automatically revoke a code.", RunE: func(cmd *cobra.Command, args []string) error {
		input.ResourceID = args[0]
		if cmd.Flags().Changed("expires-at") {
			parsed, err := time.Parse(time.RFC3339, expiry)
			if err != nil {
				return exitcode.UsageError(errors.New("expires-at must be an RFC3339 timestamp"))
			}
			parsed = parsed.Truncate(time.Second)
			input.ExpiresAt = &parsed
		}
		if err := qurlapi.ValidateAccessCodeInput(input, time.Now()); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), true)
		if err != nil {
			return err
		}
		created, err := client.CreateAccessCode(cmd.Context(), input)
		if err != nil {
			return accessCodeWriteFailure(err)
		}
		if err = opts.printer().CreatedAccessCode(created); err != nil {
			return &accessCodeResultError{cause: err, message: fmt.Sprintf("Code creation succeeded but output failed. The secret cannot be read again. Revoke the known unused record with qurl access-codes delete %s before creating another.", created.ID)}
		}
		return nil
	}}
	command.Flags().StringVar(&input.Name, "name", "", "optional name (at most 100 characters)")
	command.Flags().IntVar(&input.MaxUses, "max-uses", 0, "maximum redemptions; 0 means unlimited")
	command.Flags().StringVar(&expiry, "expires-at", "", "optional future RFC3339 expiry")
	return command
}

func accessCodeRedeemCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{Use: "redeem", Short: "Read a secret code from stdin or a hidden prompt and print its link", Args: noArgs, Long: "Read a secret code without putting it in argv. Redemption sends no account credential and makes one attempt. To meet the service's input timing rule, the command waits until three seconds have passed since input began. It prints the returned link without opening a browser. An uncertain result may have consumed a use; do not retry automatically.", RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.accountAPIKeyAuth {
			return exitcode.UsageError(errors.New("redemption is anonymous; omit --api-key-auth"))
		}
		started := time.Now()
		code, err := readSecret(opts, "Access code (input hidden): ", "no access code provided")
		if err != nil {
			return err
		}
		if err = qurlapi.ValidateAccessCode(code); err != nil {
			return exitcode.UsageError(err)
		}
		timer := time.NewTimer(max(0, 3*time.Second-time.Since(started)))
		defer timer.Stop()
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-timer.C:
		}
		opts.warnInsecureEndpoint()
		link, err := qurlapi.RedeemAccessCode(cmd.Context(), opts.accountConfig("", ""), code, time.Since(started))
		if err != nil {
			return accessCodeRedeemFailure(err)
		}
		if err = opts.printer().RedeemedAccessCode(link); err != nil {
			return accessCodeRedeemFailure(err)
		}
		return nil
	}}
}

type accessCodeResultError struct {
	cause   error
	message string
}

func (e *accessCodeResultError) Error() string       { return e.UserMessage() }
func (e *accessCodeResultError) Unwrap() error       { return e.cause }
func (e *accessCodeResultError) UserMessage() string { return e.message }

func accessCodeWriteFailure(cause error) error {
	var apiError *qurlapi.Error
	if errors.As(cause, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 {
		return cause
	}
	return &accessCodeResultError{cause: cause, message: "Access code write outcome is uncertain. Run qurl access-codes list to inspect current records. The secret cannot be recovered; revoke a known unused record by its acd_ ID before creating another. Do not retry automatically."}
}
func accessCodeRedeemFailure(cause error) error {
	var apiError *qurlapi.Error
	if errors.As(cause, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 {
		return cause
	}
	return &accessCodeResultError{cause: cause, message: "Redemption outcome is uncertain and may have consumed a use. Do not retry automatically. If no link reached stdout, ask the code owner for help."}
}
