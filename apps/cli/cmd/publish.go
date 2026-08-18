package main

import (
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func publishCmd(opts *globalOpts) *cobra.Command {
	var (
		description string
		tags        []string
		alias       string
	)

	cmd := &cobra.Command{
		Use:   "publish <target-url>",
		Short: "Publish a URL as a protected resource and get its CRID",
		Long: `Publish a URL as a qURL protected resource.

The service registers the target and returns its CRID — the resource's
permanent, verifiable ID. Share the CRID anywhere; it contains no secrets.
Anyone authorized can later turn it into a short-lived access link with
"qurl resolve".

Publishing the same URL again does not create a duplicate: while the URL
already has an active resource, publish returns that existing resource
and its CRID, and the output says so when it happens. Delete the resource
first to publish the same URL as a new resource with a fresh CRID. The
CRID is printed last so it is the easiest thing to select and copy, and
--quiet prints only the CRID for scripts.`,
		Example: `  qurl publish https://api.example.com/reports
  qurl publish https://grafana.internal.example.com --description "Team dashboard" --quiet`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := client.Publish(cmd.Context(), args[0], qurlapi.PublishOptions{
				Description: description,
				Tags:        tags,
				Alias:       alias,
			})
			if err != nil {
				return err
			}

			printer := opts.printer()
			if result.CRID == "" {
				printer.Warnf("%s", msgNoCRIDReturned)
			}
			return printer.Publish(result)
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "human-readable description stored with the resource")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag stored with the resource (repeatable)")
	cmd.Flags().StringVar(&alias, "alias", "", "memorable handle stored with the resource")

	return cmd
}
