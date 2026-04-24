package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func listCmd(opts *globalOpts) *cobra.Command {
	var (
		limit         int
		cursor        string
		status        string
		query         string
		sort          string
		createdAfter  string
		createdBefore string
		expiresBefore string
		expiresAfter  string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List QURLs",
		Example: `  qurl list
  qurl list --status active --limit 50
  qurl list --sort created_at:desc
  qurl list --query "dashboard"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if status != "" {
				switch status {
				case client.StatusActive, client.StatusRevoked:
				default:
					return fmt.Errorf("invalid status %q: must be active or revoked", status)
				}
			}

			if err := errors.Join(
				validateRFC3339("created-after", createdAfter),
				validateRFC3339("created-before", createdBefore),
				validateRFC3339("expires-before", expiresBefore),
				validateRFC3339("expires-after", expiresAfter),
			); err != nil {
				return err
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := c.List(cmd.Context(), &client.ListInput{
				Limit:         limit,
				Cursor:        cursor,
				Status:        status,
				Query:         query,
				Sort:          sort,
				CreatedAfter:  createdAfter,
				CreatedBefore: createdBefore,
				ExpiresBefore: expiresBefore,
				ExpiresAfter:  expiresAfter,
			})
			if err != nil {
				return fmt.Errorf("list QURLs: %w", err)
			}

			return opts.formatter().FormatList(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().IntVarP(&limit, "limit", "l", 20, "Maximum number of QURLs to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from a previous list response")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (active, revoked)")
	cmd.Flags().StringVar(&query, "query", "", "Search label, description, and target URL")
	cmd.Flags().StringVar(&sort, "sort", "", "Sort field:direction (e.g., created_at:desc)")
	cmd.Flags().StringVar(&createdAfter, "created-after", "", "Filter QURLs created after this date (RFC3339)")
	cmd.Flags().StringVar(&createdBefore, "created-before", "", "Filter QURLs created before this date (RFC3339)")
	cmd.Flags().StringVar(&expiresBefore, "expires-before", "", "Filter QURLs expiring before this date (RFC3339)")
	cmd.Flags().StringVar(&expiresAfter, "expires-after", "", "Filter QURLs expiring after this date (RFC3339)")

	return cmd
}
