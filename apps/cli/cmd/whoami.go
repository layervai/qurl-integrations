package main

import (
	"context"
	"errors"
	"strings"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"github.com/spf13/cobra"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// whoamiCmd reports the account and device identity behind the registered
// machine credential, via the platform's identity echo.
func whoamiCmd(opts *globalOpts) *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show which qURL account this device belongs to",
		Long: `Show the qURL account and registered device identity used by this machine.

The command opens the same durable device identity as publish, list, share,
and lifecycle commands, then checks it against the qURL service. It does not
read an account API key on a warm start. If this machine is not enrolled, run
"qurl login" or set QURL_API_KEY for one-time bootstrap.

Useful for checking which account a script will act as before it publishes
anything. With --local, read only public identifiers and pending recovery
status from the existing namespace, without contacting the service or
starting the native runtime. A pending replacement may have no key ID.`,
		Example: `  qurl whoami
  qurl whoami -o json
  qurl whoami -q`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if local {
				return runLocalIdentity(cmd.Context(), opts)
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			id := opts.registeredIdentity
			if id == nil {
				id, err = client.Me(cmd.Context())
				if err != nil {
					return err
				}
			}
			if id == nil {
				return errors.New("qURL account identity response is empty")
			}
			return opts.printer().WhoAmI(id)
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "read only local public device identifiers; never contact the service or enroll")
	return cmd
}

func runLocalIdentity(ctx context.Context, opts *globalOpts) error {
	dir, err := opts.resolveShareStateDir("")
	if err != nil {
		return err
	}
	identity, err := readLocalDeviceIdentity(ctx, dir)
	if err != nil {
		return err
	}
	return opts.printer().LocalIdentity(identity)
}

func readLocalDeviceIdentity(ctx context.Context, dir string) (identity output.LocalDeviceIdentity, retErr error) {
	reader, err := connectoragentstate.OpenSDKStateReader(dir, connectorstate.ConfiguredAgentID())
	if err != nil {
		return identity, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	state, err := reader.LoadAgentState(ctx)
	if err != nil {
		return identity, err
	}
	if state == nil || state.RegisteredAt == nil || strings.TrimSpace(state.AgentID) == "" {
		return identity, errors.New("local device identity is incomplete")
	}
	identity.AgentID = state.AgentID
	identity.RecoveryIssuePending = state.PendingCredentialRecoveryIssue != nil
	identity.RecoveryPending = identity.RecoveryIssuePending || state.PendingCredentialRecovery != nil || state.CredentialRecoveryRefreshRequired
	if state.DeviceAPIKeyID != "" {
		identity.DeviceKeyID = &state.DeviceAPIKeyID
	} else if !identity.RecoveryPending {
		return identity, errors.New("local device credential is incomplete")
	}
	return identity, nil
}
