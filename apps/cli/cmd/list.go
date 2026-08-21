package main

import (
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func listCmd(opts *globalOpts) *cobra.Command {
	var (
		limit   int
		cursor  string
		status  string
		resType string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your published resources",
		Long: `List the resources published under your account, one row per resource.

The text table shortens each CRID from the middle so rows stay readable;
JSON output and --quiet always carry the full CRID. Pages continue with
--cursor when there are more results.`,
		Example: `  qurl list --status active
  qurl list --quiet | xargs -n1 qurl resolve --quiet`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient()
			if err != nil {
				return err
			}
			page, err := client.List(cmd.Context(), qurlapi.ListOptions{
				Limit:  limit,
				Cursor: cursor,
				Status: status,
				Type:   resType,
			})
			if err != nil {
				return err
			}
			return opts.printer().List(page)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 0, "maximum resources per page, 1-100 (default: service decides)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "continue from a previous page's cursor")
	cmd.Flags().StringVar(&status, "status", "", "only resources with this status, e.g. active")
	cmd.Flags().StringVar(&resType, "type", "", "only resources of this kind: url or tunnel")

	return cmd
}
