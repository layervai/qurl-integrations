package main

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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
xxxx-xxxx-xxxx-xxxx, and takes that person's access away, for at most 256
people in one command. Every device id is checked against the list before any
access is taken away. One that is not on the list is an error and removes
nothing, so a mistyped id is never mistaken for access taken away. Then each
person is removed with a change of their own, before any change to public
keys. If one of those changes fails after others were made, the error says
exactly which device ids were removed and which were not.

A list holds at most 256 devices. --add and --remove each take at most 256
public keys in one command, which is checked before anything is sent. The
limit on the list that results is the service's: it refuses a change that
would leave more than 256 devices, and the list stays as it was.

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
			// The flag that replaced the whole list is answered first, with
			// the message that says it is gone: a command line from earlier
			// documentation must get that, whatever else is wrong with it.
			if len(replaced) > 0 {
				return exitcode.UsageError(errors.New(msgGrantsReplaceRemoved))
			}
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
			case changes:
				resource, err = changeGrants(cmd.Context(), client, opts.printer(), assessment.Input, add, removeKeys, removePeople)
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

// changeGrants makes the changes --add and --remove named and returns the
// resource as it reads afterwards. Access is taken away first: the approved
// people are removed before the public keys are changed, so that if a later
// step fails, nobody was added while a removal the publisher asked for is
// still waiting.
func changeGrants(ctx context.Context, client qurlapi.Client, printer *output.Printer, id string, add, removeKeys, removePeople []string) (*qurlapi.ResourceSummary, error) {
	var resource *qurlapi.ResourceSummary
	var err error
	if len(removePeople) > 0 {
		resource, err = client.RemoveAllowedPasskeys(ctx, id, removePeople)
		if err != nil {
			return nil, reportRemovalFailure(printer, err, len(add)+len(removeKeys) > 0)
		}
	}
	if len(add)+len(removeKeys) > 0 {
		resource, err = client.EditDeviceGrants(ctx, id, add, removeKeys)
	}
	return resource, err
}

// reportRemovalFailure finishes a `qurl grants --remove` whose removal of
// approved people failed, and returns the error the command exits with.
//
// When the failure says what happened to each device id, that is also written
// as a document in JSON mode, so a script learns which people lost access all
// the same. keysWaiting says that the command also named public keys. That
// change is made after the removals, so it was not made, and the outcome
// says so: a publisher must not be left guessing which half of the command
// ran.
func reportRemovalFailure(printer *output.Printer, err error, keysWaiting bool) error {
	var outcome *qurlapi.PasskeyRemovalError
	if !errors.As(err, &outcome) {
		return err
	}
	outcome.KeysNotChanged = keysWaiting
	if printErr := printer.RemovalOutcome(outcome); printErr != nil {
		return errors.Join(err, printErr)
	}
	return err
}

// maxRemovedPeople is the most device ids one command takes. Each is its own
// request, sent one after another, so the bound is the one a list of public
// keys has.
const maxRemovedPeople = 256

// splitRemovals sorts the values of --remove into public keys and device ids.
// A device id is recognized by its form, in either case, and is returned in
// the lowercase form the service uses. Everything else must be a public key.
// A value given twice, more values of either kind than one command takes,
// and a value that is neither, are each refused with their own message.
func splitRemovals(values []string) (keys, deviceIDs []string, err error) {
	for _, value := range values {
		deviceID := strings.ToLower(value)
		if !qurlapi.ValidDeviceID(deviceID) {
			if slices.Contains(keys, value) {
				return nil, nil, errors.New(msgGrantsRemoveTwice)
			}
			keys = append(keys, value)
			continue
		}
		if slices.Contains(deviceIDs, deviceID) {
			return nil, nil, errors.New(msgGrantsRemoveTwice)
		}
		deviceIDs = append(deviceIDs, deviceID)
	}
	if len(deviceIDs) > maxRemovedPeople {
		return nil, nil, errors.New(msgGrantsRemoveTooManyPeople)
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
