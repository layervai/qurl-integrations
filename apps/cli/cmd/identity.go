package main

import (
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func identityCmd(opts *globalOpts) *cobra.Command {
	root := &cobra.Command{
		Use: "identity", Short: "Require signed consumer identity at your app origin",
		Long: `qURL can send a short-lived signed JWT in X-QURL-Identity on each authorized
request. Enable it only after your app verifies the header on every request
and refuses direct origin requests. Requires a private URL or Connector
resource with consumer access, HTTPS for URL origins, and an environment
that offers signed identity. You can enable it for your own resource.

Disabling stops new assertions. An app that still requires the header will
refuse requests. Issued assertions remain valid until their short expiry.
Your app still controls its own permissions.`,
		Example: "  qurl identity enable <CRID>\n  qurl identity status <CRID>\n  qurl identity disable <CRID>",
	}
	shorts := map[string]string{"enable": "Require signed identity for a resource", "disable": "Stop sending signed identity for a resource", "status": "Show whether a resource requires signed identity"}
	for _, action := range []string{"enable", "disable", "status"} {
		root.AddCommand(&cobra.Command{
			Use: action + " <CRID>", Short: shorts[action], Long: root.Long, Args: exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				client, err := opts.newClient(cmd.Context())
				if err != nil {
					return err
				}
				if action == "status" {
					resource, err := client.Resource(cmd.Context(), args[0])
					if err != nil {
						return err
					}
					if resource.SignedIdentity == nil {
						return qurlapi.ErrSignedIdentityUnsupported
					}
					return opts.printer().SignedIdentity(resource)
				}
				resource, err := client.SetSignedIdentity(cmd.Context(), args[0], action == "enable")
				if err != nil {
					return err
				}
				return opts.printer().SignedIdentity(resource)
			},
		})
	}
	return root
}
