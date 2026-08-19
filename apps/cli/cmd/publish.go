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
	)

	cmd := &cobra.Command{
		Use:   "publish <target-url>",
		Short: "Publish a URL as a protected resource and get its CRID",
		Long: `Publish a URL as a qURL protected resource.

For a remote URL, the service registers the target and returns its CRID — the
resource's permanent, verifiable ID. Share the CRID anywhere; it contains no
secrets. Anyone authorized can later turn it into a short-lived access link
with "qurl resolve".

For a loopback HTTP origin such as http://127.0.0.1:3000, publish starts a
Connector, prints its CRID after the platform accepts the tunnel, and keeps
serving in the foreground until Ctrl-C. The first run uses your login to mint
a Connector-bound one-shot enrollment credential in memory; later runs reuse
the device identity saved on this machine. Use --id to choose a stable
Connector ID, or omit it for a stable opaque ID derived for this machine and
origin. ` + "`qurl connector run`" + ` remains the advanced surface for custom
state directories, refresh policy, and manually issued enrollment tokens.

Publishing the same URL again does not create a duplicate: while the URL
already has an active resource, publish returns that existing resource
and its CRID, and the output says so when it happens. Delete the resource
first to publish the same URL as a new resource with a fresh CRID. The
CRID is printed last so it is the easiest thing to select and copy, and
--quiet prints only the CRID. A local publish remains foregrounded, so command
substitution and processors such as jq wait until the Connector stops.`,
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
				return runLocalPublish(cmd.Context(), opts, target, connectorID)
			}
			if cmd.Flags().Changed("id") {
				return exitcode.UsageError(errors.New("--id applies only when publishing a loopback HTTP origin"))
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

	return cmd
}

func runLocalPublish(ctx context.Context, opts *globalOpts, target *publishTarget, flagID string) error {
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
		localIP:   target.localIP,
		localPort: target.localPort,
		configureAgent: func(cfg *agent.Config) {
			cfg.EnrollmentTokenProvider = provider
		},
		resolveID: resolveID,
		onAuthenticated: func(resolved *agent.ResolvedResource) error {
			resource := resolved.Resource
			if strings.TrimSpace(resource.CRID) == "" {
				printer.Warnf("%s", msgNoCRIDReturned)
			}
			foundExisting := resolved.FoundExisting != nil && *resolved.FoundExisting
			return printer.Publish(&qurlapi.Published{
				CRID:          resource.CRID,
				ResourceID:    resource.ResourceID,
				TargetURL:     target.original,
				Status:        "serving",
				FoundExisting: foundExisting,
			})
		},
	})
}
