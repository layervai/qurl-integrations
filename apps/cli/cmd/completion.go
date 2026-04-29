package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for qurl.

  Bash:   eval "$(qurl completion bash)"
  Zsh:    qurl completion zsh > "${fpath[1]}/_qurl"
  Fish:   qurl completion fish > ~/.config/fish/completions/qurl.fish`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return cmd.Usage()
			}
		},
	}
}

// resourceIDCompletion provides dynamic shell completions for resource IDs.
func resourceIDCompletion(opts *globalOpts) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		c, err := opts.newClient()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		result, err := c.List(ctx, &client.ListInput{Limit: 20, Query: toComplete})
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		var ids []string
		for i := range result.QURLs {
			q := &result.QURLs[i]
			desc := q.TargetURL
			if len(desc) > 50 {
				desc = desc[:49] + "…"
			}
			ids = append(ids, q.ResourceID+"\t"+desc)
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
}
