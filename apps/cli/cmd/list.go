package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func listCmd(apiKey, endpoint, format *string) *cobra.Command {
	var (
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active QURLs",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			result, err := c.List(cmd.Context(), client.ListInput{
				Limit:  limit,
				Cursor: cursor,
			})
			if err != nil {
				return fmt.Errorf("list QURLs: %w", err)
			}

			f := getFormatter(format)
			return f.FormatList(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().IntVarP(&limit, "limit", "l", 20, "Maximum number of QURLs to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from a previous list response")

	return cmd
}
