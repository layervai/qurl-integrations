package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func grantsCmd(opts *globalOpts) *cobra.Command {
	var keys []string
	var clearGrants, yes bool
	cmd := &cobra.Command{
		Use: "grants <CRID>", Short: "Replace the devices allowed to request links",
		Long: "Replace the complete device grant list as the resource owner. Repeat --allow-device-key for each allowed device, or use --clear to remove all device grants. This does not change resource privacy or revoke existing links. Public resources remain public.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clearGrants == (len(keys) > 0) {
				return exitcode.UsageError(errors.New("choose --allow-device-key or --clear"))
			}
			if err := validateAllowedDeviceKeys(keys); err != nil {
				return exitcode.UsageError(err)
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			if err := applyCRIDGuards(opts.printer(), assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			resource, err := client.SetDeviceGrants(cmd.Context(), assessment.Input, keys)
			if err != nil {
				return err
			}
			return opts.printer().ResourceStatus(resource)
		},
	}
	cmd.Flags().StringArrayVar(&keys, "allow-device-key", nil, "allowed recipient public key (repeatable; replaces the complete list)")
	cmd.Flags().BoolVar(&clearGrants, "clear", false, "remove all device grants")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	return cmd
}
