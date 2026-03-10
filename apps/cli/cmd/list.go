package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func listCmd(opts *globalOpts) *cobra.Command {
	var (
		limit  int
		cursor string
		status string
		query  string
		sort   string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List QURLs",
		Example: `  qurl list
  qurl list --status active --limit 50
  qurl list --sort created_at:desc
  qurl list --query "dashboard"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := c.List(cmd.Context(), client.ListInput{
				Limit:  limit,
				Cursor: cursor,
				Status: status,
				Query:  query,
				Sort:   sort,
			})
			if err != nil {
				return fmt.Errorf("list QURLs: %w", err)
			}

			return opts.formatter().FormatList(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().IntVarP(&limit, "limit", "l", 20, "Maximum number of QURLs to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from a previous list response")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (active, expired, revoked, consumed)")
	cmd.Flags().StringVar(&query, "query", "", "Search description and target URL")
	cmd.Flags().StringVar(&sort, "sort", "", "Sort field:direction (e.g., created_at:desc)")

	return cmd
}
