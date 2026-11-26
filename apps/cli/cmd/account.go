package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func accountCmd(opts *globalOpts) *cobra.Command {
	var headless bool
	command := &cobra.Command{
		Use:     "account",
		Args:    noArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		Short:   "Manage optional account access",
		Long:    "Publish without an account. Link this device for account recovery, access from other devices, or paid CRID contracts.",
		Example: "  qurl account setup\n  qurl account recover",
	}
	command.AddCommand(&cobra.Command{
		Use:     "setup",
		Short:   "Link this device to an account in your browser",
		Long:    "Sign in with a verified account to recover access and use other devices. Existing resources and links stay unchanged.",
		Example: "  qurl account setup",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			id := opts.registeredIdentity
			if id == nil {
				id, err = client.Me(cmd.Context())
				if err != nil {
					return err
				}
			}
			if id == nil || id.OwnerID == "" {
				return fmt.Errorf("%w: qURL account identity response is empty", qurl.ErrInvalidAPIResponse)
			}
			if !opts.quiet {
				opts.printer().Notef("%s", msgAccountSetup)
			}
			opts.warnInsecureEndpoint()
			token, err := signInAccount(cmd.Context(), opts, headless)
			if err != nil {
				return err
			}
			accountID, err := client.LinkAccount(cmd.Context(), token, id.OwnerID)
			if err != nil {
				return err
			}
			opts.reconcileRulesAfterLink(cmd.Context())
			return opts.printer().Account(id.OwnerID, accountID, "linked", msgAccountLinked)
		},
	})
	command.PersistentFlags().BoolVar(&headless, "headless", false, fmt.Sprintf("print the sign-in URL; forward %s to this server with SSH", qurlapi.AccountCallbackAddress))
	command.AddCommand(accountRecoverCmd(opts, &headless))
	return command
}

func accountRecoverCmd(opts *globalOpts, headless *bool) *cobra.Command {
	var selectedOwner string
	recoverCmd := &cobra.Command{
		Use:     "recover",
		Short:   "Recover account resources on a new device",
		Long:    "Sign in to restore management access on a new device. Use QURL_CONNECTOR_STATE_DIR to choose an unused directory and keep existing state intact. This does not restore files or running apps from another host.",
		Example: "  QURL_CONNECTOR_STATE_DIR=~/.qurl-recovered qurl account recover\n  qurl account recover --owner device:...",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.requireRuntimeSupervisionIfNamespace(); err != nil {
				return err
			}
			if !opts.quiet {
				opts.printer().Notef("%s", msgAccountContinue)
			}
			opts.warnInsecureEndpoint()
			token, err := signInAccount(cmd.Context(), opts, *headless)
			if err != nil {
				return err
			}
			owners, err := qurlapi.AccountOwners(cmd.Context(), opts.accountConfig(token, ""))
			if err != nil {
				return err
			}
			owner, err := selectAccountOwner(owners, selectedOwner)
			if err != nil {
				return err
			}
			account, err := qurlapi.New(opts.accountConfig(token, owner))
			if err != nil {
				return err
			}
			identity, err := account.Me(cmd.Context())
			if err != nil {
				return err
			}
			client, deviceIdentity, err := opts.openRegisteredClient(cmd.Context(), account, "", identity)
			if errors.Is(err, auth.ErrDeviceAccountConflict) {
				return auth.ErrAccountRecoveryState
			}
			if err != nil {
				return err
			}
			if client == nil || deviceIdentity == nil {
				return errors.New("registered-device enrollment returned no client identity")
			}
			opts.registeredClient, opts.registeredIdentity = client, deviceIdentity
			opts.reconcileRulesAfterLink(cmd.Context())
			return opts.printer().Account(deviceIdentity.OwnerID, deviceIdentity.AccountID, "recovered", msgAccountRecovered)
		},
	}
	recoverCmd.Flags().StringVar(&selectedOwner, "owner", "", "resource owner to recover when the account has several devices")
	return recoverCmd
}

func selectAccountOwner(owners []string, requested string) (string, error) {
	if requested == "" && len(owners) == 1 {
		requested = owners[0]
	}
	if requested == "" {
		for _, owner := range owners {
			// TODO(upstream-contract): qurl-service identifies device owners with the device: prefix.
			if strings.HasPrefix(owner, "device:") {
				if requested != "" {
					return "", exitcode.UsageError(fmt.Errorf(msgAccountChooseOwner, strings.Join(owners, ", ")))
				}
				requested = owner
			}
		}
	}
	if requested == "" {
		return "", exitcode.UsageError(fmt.Errorf(msgAccountChooseOwner, strings.Join(owners, ", ")))
	}
	if !slices.Contains(owners, requested) {
		return "", exitcode.UsageError(errors.New(msgAccountOwnerDenied))
	}
	return requested, nil
}

// Headless setup keeps the same PKCE and loopback callback as browser setup.
func signInAccount(ctx context.Context, opts *globalOpts, headless bool) (string, error) {
	open := opts.openBrowser
	if headless {
		open = func(_ context.Context, link string) error {
			_, err := fmt.Fprintf(opts.streams.Err, "Forward %s to this server with SSH, then open this URL on your computer:\n%s\n", qurlapi.AccountCallbackAddress, link)
			return err
		}
	}
	return opts.signInAccount(ctx, opts.accountConfig("", ""), open)
}
