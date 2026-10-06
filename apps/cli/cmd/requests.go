package main

import (
	"errors"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// requestCodePattern is a request code as a person writes it: six digits,
// with one optional space or dash in the middle. The digit class matches the
// ASCII digits only.
var requestCodePattern = regexp.MustCompile(`^(\d{3})[ -]?(\d{3})$`)

// parseRequestCode returns the six digits of the code in the operands after
// the CRID. The code is one operand, or two when a shell split "123 456".
// Anything else can never be a code, so it is refused before any request.
func parseRequestCode(operands []string) (string, error) {
	groups := requestCodePattern.FindStringSubmatch(strings.TrimSpace(strings.Join(operands, " ")))
	if groups == nil {
		return "", exitcode.InvalidInputError(msgRequestCodeInvalid, nil)
	}
	return groups[1] + groups[2], nil
}

// resourceAddress is the address of a resource on the link site, empty when
// this install does not know that site for its deployment.
func (o *globalOpts) resourceAddress(resourceCRID string) string {
	if o.linkSite == nil {
		return ""
	}
	return consume.ResourceAddress(o.linkSite(), resourceCRID)
}

// codeArgs accepts the CRID and the code, the code as one operand or as its
// two halves.
func codeArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.RangeArgs(2, 3)(cmd, args); err != nil {
		return exitcode.UsageError(err)
	}
	return nil
}

func requestsCmd(opts *globalOpts) *cobra.Command {
	var on, off, yes bool
	cmd := &cobra.Command{
		Use:   "requests [<CRID>]",
		Short: "List access requests, or turn them on or off for a private resource",
		Long: `List the people who asked for access to your private resources, or turn access
requests on or off for one resource.

With no argument, the command lists the pending requests of all of your
resources. With a CRID, it lists the requests for that resource. Each request
has the six-digit code that only the person who asked was shown, the name they
typed, the id of their device, and when they asked.

Approve a code only when the person gave it to you themselves. The code is
what ties a request to a person. The name is typed by whoever asked and proves
nothing: anyone can type any name. Approve with "qurl approve <CRID> <code>"
and refuse with "qurl deny <CRID> <code>".

--on lets people ask for access to a private resource that is already
published, and prints what to send them. --off stops new requests. To see who
was approved, and to take one person's access away, use "qurl grants <CRID>".`,
		Example: `  qurl requests
  qurl requests ` + exampleCRID + `
  qurl requests ` + exampleCRID + ` --on
  qurl requests ` + exampleCRID + ` --off`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return exitcode.UsageError(err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case on && off:
				return exitcode.UsageError(errors.New(msgRequestsOnAndOff))
			case (on || off) && len(args) == 0:
				return exitcode.UsageError(errors.New(msgRequestsSettingNeedsCRID))
			}
			printer := opts.printer()
			if len(args) == 0 {
				client, err := opts.newClient(cmd.Context())
				if err != nil {
					return err
				}
				requests, err := client.AccessRequests(cmd.Context(), "")
				if err != nil {
					return err
				}
				return printer.AccessRequests(requests, true)
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			if on || off {
				resource, err := client.SetAccessRequests(cmd.Context(), assessment.Input, on)
				if err != nil {
					return err
				}
				return printer.AccessRequestsSetting(resource, opts.resourceAddress(resource.CRID))
			}
			requests, err := client.AccessRequests(cmd.Context(), assessment.Input)
			if err != nil {
				return err
			}
			return printer.AccessRequests(requests, false)
		},
	}
	cmd.Flags().BoolVar(&on, "on", false, "let people ask for access to this private resource")
	cmd.Flags().BoolVar(&off, "off", false, "stop new requests for access to this resource")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	return cmd
}

func approveCmd(opts *globalOpts) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "approve <CRID> <code>",
		Short: "Approve one person's request for access to a private resource",
		Long: `Approve the access request that has this code, so that the person who asked
can open the resource.

The code is the six digits the person was shown when they asked. Approve it
only when that person gave it to you themselves: the code is what ties the
approval to them. The name on a request is typed by whoever asked and can be
typed by anyone, so never approve because of a name alone. To see the pending
requests, run "qurl requests <CRID>".

Write the code as 123456, 123 456 or 123-456. The command prints who now has
access and the command that takes that access away again.`,
		Example: `  qurl approve ` + exampleCRID + ` 123456
  qurl approve ` + exampleCRID + ` 123 456`,
		Args: codeArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := parseRequestCode(args[1:])
			if err != nil {
				return err
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			printer := opts.printer()
			if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			person, err := client.ApproveAccessRequest(cmd.Context(), assessment.Input, code)
			if err != nil {
				return err
			}
			return printer.Approved(assessment.Input, person)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	return cmd
}

func denyCmd(opts *globalOpts) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "deny <CRID> <code>",
		Short: "Refuse one person's request for access to a private resource",
		Long: `Refuse the access request that has this code. The request is removed and the
person gets no access.

Write the code as 123456, 123 456 or 123-456. To see the pending requests, run
"qurl requests <CRID>". To take away access you already approved, use
"qurl grants <CRID> --remove <device id>" instead.`,
		Example: `  qurl deny ` + exampleCRID + ` 123456`,
		Args:    codeArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := parseRequestCode(args[1:])
			if err != nil {
				return err
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			printer := opts.printer()
			if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := client.DenyAccessRequest(cmd.Context(), assessment.Input, code); err != nil {
				return err
			}
			return printer.Denied(assessment.Input, code)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	return cmd
}
