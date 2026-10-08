package main

import (
	"errors"
	"slices"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func grantsCmd(opts *globalOpts) *cobra.Command {
	var add, remove, replaced []string
	var clearGrants, yes bool
	cmd := &cobra.Command{
		Use:   "grants <CRID>",
		Short: "Show or change the devices allowed to open a private resource",
		Long: `Show the devices allowed to open a private resource, or change that list as
the resource owner.

With no flag, the command prints the current list. --add allows a device and
--remove takes one off the list. Each can be repeated, and both can be used in
one command, which is applied as one change. A public key that is already on
the list, or already off it, is left as it is, so the command can be repeated
safely. --clear takes every device off the list.

A list holds at most 256 devices. --add and --remove each take at most 256
public keys in one command, which is checked before anything is sent. The
limit on the list that results is the service's: it refuses a change that
would leave more than 256 devices, and the list stays as it was.

Run "qurl whoami -o json" on a recipient's device to find its public key. To
set the first list when you publish, use "qurl publish --allow-device-key".

Device grants apply to a private resource and have no effect on a public one.
Changing them does not change the resource's privacy. Taking a device off the
list stops new link requests from it; links that were already issued keep
their own expiry.`,
		Example: `  qurl grants ` + exampleCRID + `
  qurl grants ` + exampleCRID + ` --add <public-key>
  qurl grants ` + exampleCRID + ` --add <public-key> --remove <other-public-key>
  qurl grants ` + exampleCRID + ` --clear`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateGrantFlags(add, remove, replaced, clearGrants); err != nil {
				return exitcode.UsageError(err)
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			// Reading the list sends nothing that acts on the resource, so
			// like status and inspect it needs no confirmation for a test
			// CRID on a production endpoint. A change does.
			changes := clearGrants || len(add)+len(remove) > 0
			if !changes {
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
			var resource *qurlapi.ResourceSummary
			switch {
			case clearGrants:
				// Clearing is the one change that replaces the complete list,
				// with an empty one.
				resource, err = client.SetDeviceGrants(cmd.Context(), assessment.Input, nil)
			case len(add)+len(remove) > 0:
				resource, err = client.EditDeviceGrants(cmd.Context(), assessment.Input, add, remove)
			default:
				resource, err = client.Resource(cmd.Context(), assessment.Input)
			}
			if err != nil {
				return err
			}
			printer := opts.printer()
			if resource.Private != nil && !*resource.Private {
				printer.Notef("%s", msgPublicGrantsNoEffect)
			}
			return printer.ResourceStatus(resource)
		},
	}
	cmd.Flags().StringArrayVar(&add, "add", nil, "public key of a device to allow (repeatable)")
	cmd.Flags().StringArrayVar(&remove, "remove", nil, "public key of a device to take off the list (repeatable)")
	cmd.Flags().BoolVar(&clearGrants, "clear", false, "take every device off the list")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a change to a test CRID on a production endpoint; reading the list never needs it")
	// --allow-device-key replaced the complete list here. It is still parsed,
	// hidden, so that a command from earlier documentation gets a usage error
	// that names --add instead of "unknown flag", and never replaces a list.
	cmd.Flags().StringArrayVar(&replaced, "allow-device-key", nil, "replaced by --add and --remove")
	if err := cmd.Flags().MarkHidden("allow-device-key"); err != nil {
		panic(err) // unreachable: the flag is defined on the line above
	}
	return cmd
}

// validateGrantFlags refuses a command line that cannot be one change to the
// list, before a credential is read or a request is sent. No flag at all is
// valid: it reads the list.
func validateGrantFlags(add, remove, replaced []string, clearGrants bool) error {
	if len(replaced) > 0 {
		return errors.New(msgGrantsReplaceRemoved)
	}
	if clearGrants && len(add)+len(remove) > 0 {
		return errors.New(msgGrantsClearWithEdit)
	}
	if err := validateDeviceKeys("--add", add); err != nil {
		return err
	}
	if err := validateDeviceKeys("--remove", remove); err != nil {
		return err
	}
	for _, key := range add {
		if slices.Contains(remove, key) {
			return errors.New(msgGrantsAddAndRemove)
		}
	}
	return nil
}
