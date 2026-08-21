package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate a shell completion script for qurl.

  Bash:   eval "$(qurl completion bash)"
  Zsh:    qurl completion zsh > "${fpath[1]}/_qurl"
  Fish:   qurl completion fish > ~/.config/fish/completions/qurl.fish`,
		Example: `  qurl completion zsh > "${fpath[1]}/_qurl"
  eval "$(qurl completion bash)"`,
		Args:      exactArgs(1),
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
				return exitcode.UsageError(fmt.Errorf("unsupported shell %q: use bash, zsh, fish, or powershell", args[0]))
			}
		},
	}
}
