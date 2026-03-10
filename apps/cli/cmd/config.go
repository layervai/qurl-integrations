package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
		Long: `Manage CLI configuration stored at ~/.config/qurl/config.yaml.

Supported keys: api_key, endpoint, output`,
	}

	cmd.AddCommand(configSetCmd(), configGetCmd(), configPathCmd())
	return cmd
}

func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "set <key> <value>",
		Short:   "Set a configuration value",
		Example: "  qurl config set api_key lv_live_xxx",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				cfg = &config.Config{}
			}

			if err := cfg.Set(args[0], args[1]); err != nil {
				return err
			}

			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Set %s\n", args[0])
			return err
		},
	}
}

func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <key>",
		Short:   "Get a configuration value",
		Example: "  qurl config get endpoint",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			v := cfg.Get(args[0])
			if v == "" {
				return fmt.Errorf("key %q is not set", args[0])
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), v)
			return err
		},
	}
}

func configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Show config file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), config.Path())
			return err
		},
	}
}
