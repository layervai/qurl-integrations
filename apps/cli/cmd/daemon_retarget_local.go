package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// daemonRetargetLocalCmd changes only local origins while the external owner
// has stopped its daemon. It never opens native credentials or a REST client.
func daemonRetargetLocalCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use: "retarget-local", Short: "Move stopped local shares to a private Unix origin", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.resolvedSupervision != connectorstate.RuntimeSupervisionExternal {
				return exitcode.UsageError(errors.New("local target conversion requires external supervision"))
			}
			data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4097))
			if err != nil || len(data) > 4096 {
				return exitcode.UsageError(errors.New("local target conversion input must be JSON under 4096 bytes"))
			}
			var input struct {
				OwnerID           string `json:"owner_id"`
				ConnectorIDPrefix string `json:"connector_id_prefix"`
				Target            string `json:"target"`
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
				return exitcode.UsageError(errors.New("local target conversion input has an invalid JSON shape"))
			}
			if _, err := connectorstate.ParseUnixTarget(input.Target); err != nil {
				return exitcode.UsageError(err)
			}
			stateDir, err := opts.resolveShareStateDir("")
			if err != nil {
				return err
			}
			if err := connectorstate.RequireRuntimeSupervision(stateDir, connectorstate.RuntimeSupervisionExternal); err != nil {
				return err
			}
			registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				return err
			}
			socketPath, err := connectordaemon.SocketPathForStateDir(stateDir, opts.lookupEnv)
			if err != nil {
				return err
			}
			var changed int
			err = connectordaemon.WithStoppedDaemon(cmd.Context(), socketPath, func() error {
				var updateErr error
				changed, updateErr = registry.RetargetStoppedToUnix(cmd.Context(), input.OwnerID, input.ConnectorIDPrefix, input.Target)
				return updateErr
			})
			if err != nil {
				return err
			}
			if opts.resolvedFormat == output.FormatJSON {
				return json.NewEncoder(opts.streams.Out).Encode(struct {
					Changed int `json:"changed"`
				}{changed})
			}
			_, err = fmt.Fprintf(opts.streams.Out, "Updated %d local share targets.\n", changed)
			return err
		},
	}
}
