package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func accountCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "account", Short: "Manage optional account access", Long: "Publish without an account. Link this device only when you need account recovery or access from other devices.", Example: "  qurl account setup\n  qurl account recover"}
	command.AddCommand(&cobra.Command{Use: "setup", Short: "Link this device to an account in your browser", Long: "Sign in with a verified account to recover access and use other devices. Existing resources and links stay unchanged.", Example: "  qurl account setup", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
			return errors.New("qURL account identity response is empty")
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), msgAccountSetup)
		opts.warnInsecureEndpoint()
		token, err := qurlapi.SignInAccount(cmd.Context(), opts.accountConfig("", ""), opts.openBrowser)
		if err != nil {
			return err
		}
		if err := client.LinkAccount(cmd.Context(), token, id.OwnerID); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), msgAccountLinked)
		return err
	}})
	command.AddCommand(accountRecoverCmd(opts))
	return command
}

func accountRecoverCmd(opts *globalOpts) *cobra.Command {
	var selectedOwner string
	recoverCmd := &cobra.Command{Use: "recover", Short: "Recover account resources on a new device", Long: "Sign in to restore management access on a new device. This does not restore files or running apps from another host.", Example: "  qurl account recover\n  qurl account recover --owner device:...", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := opts.requireRuntimeSupervisionIfNamespace(); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), msgAccountContinue)
		opts.warnInsecureEndpoint()
		token, err := qurlapi.SignInAccount(cmd.Context(), opts.accountConfig("", ""), opts.openBrowser)
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
		if err != nil {
			return err
		}
		opts.registeredClient, opts.registeredIdentity = client, deviceIdentity
		_, err = fmt.Fprintln(cmd.OutOrStdout(), msgAccountRecovered)
		return err
	}}
	recoverCmd.Flags().StringVar(&selectedOwner, "owner", "", "resource owner to recover when the account has several devices")
	return recoverCmd
}

func selectAccountOwner(owners []string, requested string) (string, error) {
	if requested == "" && len(owners) == 1 {
		requested = owners[0]
	}
	if requested == "" {
		for _, owner := range owners {
			if strings.HasPrefix(owner, "device:") {
				if requested != "" {
					return "", fmt.Errorf(msgAccountChooseOwner, strings.Join(owners, ", "))
				}
				requested = owner
			}
		}
	}
	if requested == "" {
		return "", fmt.Errorf(msgAccountChooseOwner, strings.Join(owners, ", "))
	}
	if !slices.Contains(owners, requested) {
		return "", errors.New(msgAccountOwnerDenied)
	}
	return requested, nil
}
