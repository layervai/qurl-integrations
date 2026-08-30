package main

import (
	"context"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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

The text table always prints the full CRID. Local tunnel rows include their
loopback target and durable desired state. Observed tunnel state is shown as
unknown in this paged view; use qurl status <CRID> for an authoritative live
observation. Pages continue with --cursor when there are more results.`,
		Example: `  qurl list --status active
  qurl list --quiet | xargs -n1 qurl resolve --quiet`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient(cmd.Context())
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
			// JSON is the same structured document with or without --quiet.
			// Keep its local tunnel targets stable; only quiet text skips the
			// registry read because it prints identifiers alone.
			if !opts.quiet || opts.resolvedFormat == output.FormatJSON {
				if err := enrichTunnelList(cmd.Context(), opts, page); err != nil {
					return err
				}
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

func enrichTunnelList(ctx context.Context, opts *globalOpts, page *qurlapi.ResourcePage) error {
	tunnelRows := make([]int, 0, len(page.Items))
	for index := range page.Items {
		if page.Items[index].Type == connectorResourceType {
			tunnelRows = append(tunnelRows, index)
		}
	}
	if len(tunnelRows) == 0 {
		return nil
	}
	shares, err := opts.loadLocalShares(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		opts.printer().Warnf("Local sharing state is invalid or inaccessible; local targets were omitted: %v", err)
		return nil
	}
	localTargets := make(map[string]string, len(shares))
	for index := range shares {
		share := &shares[index]
		localTargets[share.ResourceID] = share.TargetURL
	}
	for _, index := range tunnelRows {
		if target := localTargets[page.Items[index].ResourceID]; target != "" {
			page.Items[index].TargetURL = target
		}
	}

	return nil
}
