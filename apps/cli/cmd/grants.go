package main

import (
	"errors"
	"slices"
	"strings"

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
		Short: "Show or change the devices and people allowed to open a private resource",
		Long: `Show the devices and the people allowed to open a private resource, or change
those lists as the resource owner.

With no flag, the command prints both lists: the public keys of the devices
you allowed, and the people you approved after they asked for access, each
with the name they typed, the id of their device, and when you approved them.
It also says whether people can still ask.

--add allows a device and --remove takes one off the list. Each can be
repeated, and both can be used in one command, which is applied as one change.
A public key that is already on the list, or already off it, is left as it
is, so the command can be repeated safely. --clear takes every public key off
the list. It does not remove approved people.

--remove also takes the device id of an approved person, in the form
xxxx-xxxx-xxxx-xxxx, and takes that person's access away. Each device id is
its own change, made before any change to public keys. A device id that is
not on the list is an error and removes nothing, so a mistyped id is never
mistaken for access taken away.

Run "qurl whoami -o json" on a recipient's device to find its public key. To
set the first list when you publish, use "qurl publish --allow-device-key".
People are approved with "qurl approve", after "qurl requests" shows who asked.

Both lists apply to a private resource and have no effect on a public one.
Changing them does not change the resource's privacy. Taking a device or a
person off a list stops new link requests from them; links that were already
issued keep their own expiry.`,
		Example: `  qurl grants ` + exampleCRID + `
  qurl grants ` + exampleCRID + ` --add <public-key>
  qurl grants ` + exampleCRID + ` --add <public-key> --remove <other-public-key>
  qurl grants ` + exampleCRID + ` --remove <device id>
  qurl grants ` + exampleCRID + ` --clear`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			removeKeys, removePeople, err := splitRemovals(remove)
			if err != nil {
				return exitcode.UsageError(err)
			}
			if err := validateGrantFlags(add, removeKeys, replaced, clearGrants, len(removePeople) > 0); err != nil {
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
			var resource *qurlapi.ResourceSummary
			switch {
			case clearGrants:
				// Clearing is the one change that replaces the complete list,
				// with an empty one.
				resource, err = client.SetDeviceGrants(cmd.Context(), assessment.Input, nil)
			case len(add)+len(removeKeys)+len(removePeople) > 0:
				// Access is taken away first. If a later step fails, nobody
				// was added while a removal the publisher asked for is still
				// waiting.
				if len(removePeople) > 0 {
					resource, err = client.RemoveAllowedPasskeys(cmd.Context(), assessment.Input, removePeople)
				}
				if err == nil && len(add)+len(removeKeys) > 0 {
					resource, err = client.EditDeviceGrants(cmd.Context(), assessment.Input, add, removeKeys)
				}
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
			return printer.Grants(resource)
		},
	}
	cmd.Flags().StringArrayVar(&add, "add", nil, "public key of a device to allow (repeatable)")
	cmd.Flags().StringArrayVar(&remove, "remove", nil, "public key of a device, or device id of an approved person, to take off the list (repeatable)")
	cmd.Flags().BoolVar(&clearGrants, "clear", false, "take every public key off the list; approved people stay")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	// --allow-device-key replaced the complete list here. It is still parsed,
	// hidden, so that a command from earlier documentation gets a usage error
	// that names --add instead of "unknown flag", and never replaces a list.
	cmd.Flags().StringArrayVar(&replaced, "allow-device-key", nil, "replaced by --add and --remove")
	if err := cmd.Flags().MarkHidden("allow-device-key"); err != nil {
		panic(err) // unreachable: the flag is defined on the line above
	}
	return cmd
}

// splitRemovals sorts the values of --remove into public keys and device ids.
// A device id is recognized by its form, in either case, and is returned in
// the lowercase form the service uses. Everything else must be a public key.
// A value that is neither, and a value given twice, is refused.
func splitRemovals(values []string) (keys, deviceIDs []string, err error) {
	for _, value := range values {
		deviceID := strings.ToLower(value)
		if !qurlapi.ValidDeviceID(deviceID) {
			keys = append(keys, value)
			continue
		}
		if slices.Contains(deviceIDs, deviceID) {
			return nil, nil, errors.New(msgGrantsRemoveInvalid)
		}
		deviceIDs = append(deviceIDs, deviceID)
	}
	if err := validateDeviceKeys("--remove", keys); err != nil {
		if len(keys) > 256 {
			return nil, nil, err
		}
		return nil, nil, errors.New(msgGrantsRemoveInvalid)
	}
	return keys, deviceIDs, nil
}

// validateGrantFlags refuses a command line that cannot be one change to the
// lists, before a credential is read or a request is sent. No flag at all is
// valid: it reads the lists. removeKeys are the public keys given to
// --remove, already checked by splitRemovals, and removePeople says that
// --remove was also given a device id.
func validateGrantFlags(add, removeKeys, replaced []string, clearGrants, removePeople bool) error {
	if len(replaced) > 0 {
		return errors.New(msgGrantsReplaceRemoved)
	}
	if clearGrants && (len(add)+len(removeKeys) > 0 || removePeople) {
		return errors.New(msgGrantsClearWithEdit)
	}
	if err := validateDeviceKeys("--add", add); err != nil {
		return err
	}
	for _, key := range add {
		if slices.Contains(removeKeys, key) {
			return errors.New(msgGrantsAddAndRemove)
		}
	}
	return nil
}
