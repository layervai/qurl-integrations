package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func publishCmd(opts *globalOpts) *cobra.Command {
	var (
		description string
		tags        []string
		alias       string
		connectorID string
		refreshMode string
	)

	cmd := &cobra.Command{
		Use:   "publish <target-url>",
		Short: "Publish a URL or local app and get its CRID",
		Long: `Publish a local app or remote URL and get its CRID.

For a local app, pass its loopback HTTP address:

  qurl publish http://127.0.0.1:3000

qURL prints the CRID when the route is ready, then keeps serving until Ctrl-C.
Running the same command later reuses the same resource and CRID. Use --id only
when you want to choose the Connector ID yourself.

For a remote URL, qURL registers it, prints the CRID, and exits:

  qurl publish https://api.example.com/reports

A CRID is safe to share: it identifies the resource but grants no access.
Authorized users open it with "qurl get <CRID>". The --quiet flag prints only
the CRID. Use "qurl connector run" for advanced Connector configuration.`,
		Example: `  qurl publish http://127.0.0.1:3000
  qurl publish http://localhost:8080 --id local-dashboard
  qurl publish https://api.example.com/reports
  qurl publish https://grafana.internal.example.com --description "Team dashboard" --quiet`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := classifyPublishTarget(args[0])
			if err != nil {
				return err
			}
			if target.kind == publishTargetLocal {
				for _, name := range []string{"description", "tag", "alias"} {
					if cmd.Flags().Changed(name) {
						return exitcode.UsageError(fmt.Errorf("--%s is not supported for a local Connector publish", name))
					}
				}
				resolvedRefreshMode, err := validateRefreshModeFlag(refreshMode)
				if err != nil {
					return err
				}
				return runLocalPublish(cmd.Context(), opts, target, connectorID, resolvedRefreshMode)
			}
			if cmd.Flags().Changed("id") {
				return exitcode.UsageError(errors.New("--id applies only when publishing a loopback HTTP origin"))
			}
			if cmd.Flags().Changed("refresh-mode") {
				return exitcode.UsageError(errors.New("--refresh-mode applies only when publishing a loopback HTTP origin"))
			}
			client, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := client.Publish(cmd.Context(), args[0], qurlapi.PublishOptions{
				Description: description,
				Tags:        tags,
				Alias:       alias,
			})
			if err != nil {
				return err
			}

			printer := opts.printer()
			if result.CRID == "" {
				printer.Warnf("%s", msgNoCRIDReturned)
			}
			return printer.Publish(result)
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "human-readable description stored with the resource")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag stored with the resource (repeatable)")
	cmd.Flags().StringVar(&alias, "alias", "", "memorable handle stored with the resource")
	cmd.Flags().StringVar(&connectorID, "id", "", "Connector ID for a local publish (default: stable ID for this machine and origin)")
	cmd.Flags().StringVar(&refreshMode, "refresh-mode", "", "assignment-refresh policy after sustained local-Connector failures: manual, auto, or disabled (default manual)")

	return cmd
}

func runLocalPublish(ctx context.Context, opts *globalOpts, target *publishTarget, flagID, refreshMode string) error {
	requestedID, err := resolveConnectorID(opts, &connectorRunFlags{id: flagID})
	if err != nil {
		return err
	}
	if requestedID != "" {
		if err := validateConnectorID(requestedID); err != nil {
			return err
		}
	}

	var mintedMu sync.Mutex
	var mintedID string
	provider := func(providerCtx context.Context, request agent.EnrollmentTokenRequest) (string, error) {
		id := requestedID
		if id == "" {
			var err error
			id, err = generatedLocalConnectorID(request.AgentID, target.canonicalOrigin)
			if err != nil {
				return "", err
			}
		}
		idempotencyKey, err := localEnrollmentIdempotencyKey(request.AgentID, id)
		if err != nil {
			return "", err
		}
		client, err := opts.newClient()
		if err != nil {
			return "", err
		}
		token, err := client.MintConnectorEnrollmentToken(providerCtx, qurlapi.MintConnectorEnrollmentTokenOptions{
			ConnectorID:    id,
			IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return "", err
		}
		mintedMu.Lock()
		defer mintedMu.Unlock()
		if mintedID != "" && mintedID != id {
			return "", fmt.Errorf("local Connector identity changed during enrollment: %q then %q", mintedID, id)
		}
		mintedID = id
		return token.Token, nil
	}

	resolveID := func(rt *agent.Runtime) (string, error) {
		id := requestedID
		if id == "" {
			var err error
			id, err = generatedLocalConnectorID(rt.AgentID, target.canonicalOrigin)
			if err != nil {
				return "", err
			}
		}
		mintedMu.Lock()
		defer mintedMu.Unlock()
		if mintedID != "" && mintedID != id {
			return "", fmt.Errorf("local Connector identity does not match its enrollment claim: runtime resolved %q, token was bound to %q", id, mintedID)
		}
		return id, nil
	}

	printer := opts.printer()
	return serveConnector(ctx, opts, &connectorServeInputs{
		localIP:     target.localIP,
		localPort:   target.localPort,
		refreshMode: refreshMode,
		configureAgent: func(cfg *agent.Config) {
			cfg.EnrollmentTokenProvider = provider
		},
		resolveID: resolveID,
		onServing: func(resolved *agent.ResolvedResource) error {
			resource := resolved.Resource
			if strings.TrimSpace(resource.CRID) == "" {
				printer.Warnf("%s", msgNoCRIDReturned)
			}
			return printer.Publish(&qurlapi.Published{
				CRID:          resource.CRID,
				ResourceID:    resource.ResourceID,
				TargetURL:     target.original,
				Status:        "serving",
				FoundExisting: resolved.FoundExisting,
			})
		},
	})
}
