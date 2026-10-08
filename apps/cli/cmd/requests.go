package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
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

// codeHalf is one of the two groups of three digits a shell makes of a code
// typed as "123 456".
var codeHalf = regexp.MustCompile(`^\d{3}$`)

// codeArgs accepts the CRID and one more operand, or the CRID and a code
// that a shell split into its two halves. A third operand that is not the
// second half of a code is one operand too many, and is reported as that
// instead of as a code that cannot be one.
func codeArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 3 && codeHalf.MatchString(args[1]) && codeHalf.MatchString(args[2]) {
		return nil
	}
	if err := cobra.ExactArgs(2)(cmd, args); err != nil {
		return exitcode.UsageError(err)
	}
	return nil
}

// isHTTPMethod reports whether operand is one of the methods `qurl request`
// takes as its first operand, in any case. No CRID is one of these words.
func isHTTPMethod(operand string) bool {
	switch strings.ToUpper(operand) {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
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
has the name the person typed, the id of their device, when they asked, and
when the request expires.

The listing never shows a request's six-digit code, in any output mode. The
code is on the screen of the person who asked, and it is the only proof of who
is asking: the name is typed by whoever asked, and anyone can type any name.
To let a person in, ask them for their code and run
"qurl approve <CRID> <code>". To refuse a request, run
"qurl deny <CRID> <device id>" with the device id from the listing.

The listing of all your resources is bounded. When there may be more requests
than it shows, it says so; list one resource to see all of its requests.

--on lets people ask for access to a private resource that is already
published, and prints what to send them. --off stops new requests. To see who
was approved, and to take one person's access away, use "qurl grants <CRID>".

"qurl request", without the s, is another command: it makes one request for an
app that supervises qURL.`,
		Example: `  qurl requests
  qurl requests ` + exampleCRID + `
  qurl requests ` + exampleCRID + ` --on
  qurl requests ` + exampleCRID + ` --off`,
		Args: func(cmd *cobra.Command, args []string) error {
			err := cobra.MaximumNArgs(1)(cmd, args)
			switch {
			case err == nil:
				return nil
			case isHTTPMethod(args[0]):
				// A method and a path are the operands of `qurl request`.
				return usageErrorWithHint(err, hintMeantRequest)
			default:
				return exitcode.UsageError(err)
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case on && off:
				return exitcode.UsageError(errors.New(msgRequestsOnAndOff))
			case (on || off) && len(args) == 0:
				return exitcode.UsageError(errors.New(msgRequestsSettingNeedsCRID))
			case on || off:
				return setAccessRequests(cmd.Context(), opts, args[0], on, yes)
			case len(args) == 1:
				return listAccessRequests(cmd.Context(), opts, args[0])
			}
			return listAccessRequests(cmd.Context(), opts, "")
		},
	}
	cmd.Flags().BoolVar(&on, "on", false, "let people ask for access to this private resource")
	cmd.Flags().BoolVar(&off, "off", false, "stop new requests for access to this resource")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow --on or --off for a test CRID on a production endpoint; listing never needs it")
	return cmd
}

// listAccessRequests prints the pending requests of one resource, or of all
// of the owner's resources when operand is empty.
//
// A listing acts on nothing, so like `qurl grants` with no flag it needs no
// confirmation for a test CRID on a production endpoint. The operand must
// still be a CRID.
func listAccessRequests(ctx context.Context, opts *globalOpts, operand string) error {
	id := ""
	if operand != "" {
		assessment, err := cridux.Assess(operand)
		if err != nil {
			return err
		}
		if err := requireCRID(assessment); err != nil {
			return err
		}
		id = assessment.Input
	}
	client, err := opts.newClient(ctx)
	if err != nil {
		return err
	}
	requests, err := client.AccessRequests(ctx, id)
	if err != nil {
		return err
	}
	return opts.printer().AccessRequests(requests, id == "")
}

// setAccessRequests turns access requests on or off for one resource. It is
// a change, so a test CRID on a production endpoint needs --yes.
func setAccessRequests(ctx context.Context, opts *globalOpts, operand string, on, yes bool) error {
	assessment, err := cridux.Assess(operand)
	if err != nil {
		return err
	}
	printer := opts.printer()
	if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), yes); err != nil {
		return err
	}
	client, err := opts.newClient(ctx)
	if err != nil {
		return err
	}
	resource, err := client.SetAccessRequests(ctx, assessment.Input, on)
	if err != nil {
		return err
	}
	return printer.AccessRequestsSetting(resource, opts.resourceAddress(resource.CRID))
}

func approveCmd(opts *globalOpts) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "approve <CRID> <code>",
		Short: "Approve one person's request for access to a private resource",
		Long: `Approve the access request that has this code, so that the person who asked
can open the resource.

The code is the six digits the person was shown when they asked. They give it
to you; no qURL command shows it. Approve it only when that person gave it to
you themselves: the code is what ties the approval to them. The name on a
request is typed by whoever asked and can be typed by anyone, so never approve
because of a name alone. To see who is waiting, run "qurl requests <CRID>".

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
		Use:   "deny <CRID> <device id>",
		Short: "Refuse one person's request for access to a private resource",
		Long: `Refuse a pending access request. The request is removed and the person gets
no access.

Name the request by the device id it came from, in the form
xxxx-xxxx-xxxx-xxxx, as "qurl requests <CRID>" shows it. If the person gave
you their six-digit code and you want to refuse it, the code is accepted in
the same place: write it as 123456, 123 456 or 123-456.

A request that is not approved gives no access and expires by itself. To take
away access you already approved, use "qurl grants <CRID> --remove <device id>"
instead.`,
		Example: `  qurl deny ` + exampleCRID + ` abcd-efgh-2345-mnop
  qurl deny ` + exampleCRID + ` 123456`,
		Args: codeArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := parseDeniedRequest(args[1:])
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
			if err := client.DenyAccessRequest(cmd.Context(), assessment.Input, request); err != nil {
				return err
			}
			return printer.Denied(assessment.Input, request)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "allow a test CRID on a production endpoint")
	return cmd
}

// parseDeniedRequest returns what names the request to refuse: a device id,
// in the lowercase form the service uses, or the six digits of a code. A
// device id is recognized by its form, in either case. Anything that is
// neither is refused before any request.
func parseDeniedRequest(operands []string) (string, error) {
	if len(operands) == 1 {
		if deviceID := strings.ToLower(strings.TrimSpace(operands[0])); qurlapi.ValidDeviceID(deviceID) {
			return deviceID, nil
		}
	}
	code, err := parseRequestCode(operands)
	if err != nil {
		return "", exitcode.InvalidInputError(msgDeniedRequestInvalid, nil)
	}
	return code, nil
}
