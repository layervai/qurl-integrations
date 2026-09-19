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
	command := &cobra.Command{Use: "account", Short: "Manage optional account access"}
	command.AddCommand(&cobra.Command{Use: "setup", Short: "Link this device to an account in your browser", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "This device controls your qURL resources.\n\nLink an account to recover access and manage resources from other devices.\nYour existing links will keep working.\n\nContinue in your browser.")
		token, err := qurlapi.SignInAccount(cmd.Context(), opts.resolvedEndpoint, opts.openBrowser)
		if err != nil {
			return err
		}
		if err := client.LinkAccount(cmd.Context(), token); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Account linked. Your existing links are unchanged.")
		return err
	}})
	command.AddCommand(accountRecoverCmd(opts))
	return command
}

func accountRecoverCmd(opts *globalOpts) *cobra.Command {
	var selectedOwner string
	recoverCmd := &cobra.Command{Use: "recover", Short: "Recover account resources on a new device", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		token, err := qurlapi.SignInAccount(cmd.Context(), opts.resolvedEndpoint, opts.openBrowser)
		if err != nil {
			return err
		}
		owners, err := qurlapi.AccountOwners(cmd.Context(), opts.resolvedEndpoint, token)
		if err != nil {
			return err
		}
		if selectedOwner == "" && len(owners) == 1 {
			selectedOwner = owners[0]
		}
		if selectedOwner == "" {
			devices := []string{}
			for _, owner := range owners {
				if strings.HasPrefix(owner, "device:") {
					devices = append(devices, owner)
				}
			}
			if len(devices) == 1 {
				selectedOwner = devices[0]
			} else {
				return fmt.Errorf("choose resources to recover with --owner; available owners: %s", strings.Join(owners, ", "))
			}
		}
		if !slices.Contains(owners, selectedOwner) {
			return errors.New("this account does not own the selected resources")
		}
		account, err := qurlapi.New(&qurlapi.Config{BaseURL: opts.resolvedEndpoint, APIKey: token, OwnerID: selectedOwner})
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
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Resource access recovered on this device. Existing links are unchanged.")
		return err
	}}
	recoverCmd.Flags().StringVar(&selectedOwner, "owner", "", "resource owner to recover when the account has several devices")
	return recoverCmd
}
