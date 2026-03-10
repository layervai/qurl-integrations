package main

import "github.com/spf13/cobra"

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for qurl.

  Bash:   qurl completion bash > /etc/bash_completion.d/qurl
  Zsh:    qurl completion zsh > "${fpath[1]}/_qurl"
  Fish:   qurl completion fish > ~/.config/fish/completions/qurl.fish`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		Hidden:    true, // Available but not cluttering --help
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
