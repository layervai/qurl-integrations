package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
		Long: fmt.Sprintf(`Manage CLI configuration stored at ~/.config/qurl/config.yaml.

Supported keys: %s

Profiles:
  Use --profile to manage named profiles stored under ~/.config/qurl/profiles/.
  qurl config set --profile staging api_key lv_live_yyy
  qurl --profile staging list`, strings.Join(config.ValidKeys(), ", ")),
	}

	cmd.AddCommand(configSetCmd(), configGetCmd(), configPathCmd(), configListProfilesCmd())
	return cmd
}

func configSetCmd() *cobra.Command {
	var profile string

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Example: `  qurl config set api_key lv_live_xxx
  qurl config set --profile staging api_key lv_live_yyy`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]

			cfg, err := config.LoadProfile(profile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if err := cfg.Set(key, value); err != nil {
				return err
			}

			if err := config.SaveProfile(profile, cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			msg := "Set " + key
			if profile != "" {
				msg += fmt.Sprintf(" (profile: %s)", profile)
			}
			if key == "api_key" {
				msg += "\n  Note: API key is stored in plaintext with file permissions 0600."
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), msg)
			return err
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "Profile name to configure")
	return cmd
}

func configGetCmd() *cobra.Command {
	var profile string

	cmd := &cobra.Command{
		Use:     "get <key>",
		Short:   "Get a configuration value",
		Example: "  qurl config get endpoint",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadProfile(profile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			key := args[0]
			if !config.IsValidKey(key) {
				return fmt.Errorf("unknown key %q (valid: %s)", key, strings.Join(config.ValidKeys(), ", "))
			}
			v := cfg.Get(key)
			if v == "" {
				return fmt.Errorf("key %q is not set", key)
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), v)
			return err
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "Profile name to read")
	return cmd
}

func configPathCmd() *cobra.Command {
	var profile string

	cmd := &cobra.Command{
		Use:   "path",
		Short: "Show config file path",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var p string
			if profile != "" {
				var err error
				p, err = config.ProfilePath(profile)
				if err != nil {
					return err
				}
			} else {
				p = config.Path()
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), p)
			return err
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "Profile name")
	return cmd
}

func configListProfilesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "profiles",
		Short:   "List available configuration profiles",
		Example: "  qurl config profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			profiles, err := config.ListProfiles()
			if err != nil {
				return err
			}
			if len(profiles) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No profiles configured. Create one with: qurl config set --profile <name> api_key <key>")
				return err
			}
			for _, p := range profiles {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), p); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
