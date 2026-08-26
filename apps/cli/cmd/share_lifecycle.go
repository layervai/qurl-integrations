package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type localShareRegistry interface {
	Get(context.Context, string) (*connectorstate.LocalShare, error)
	Put(context.Context, *connectorstate.LocalShare) error
	SetDesired(context.Context, string, string, uint64) (*connectorstate.LocalShare, error)
	Delete(context.Context, string) error
}

type shareDaemonController interface {
	Ensure(context.Context) error
	ReloadIfRunning(context.Context) (bool, error)
}

func shareStartCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use: "start <CRID>", Short: "Start sharing a local app", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeShareState(cmd.Context(), opts, args[0], "start")
		},
	}
}

func shareStopCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use: "stop <CRID>", Short: "Stop sharing a local app", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeShareState(cmd.Context(), opts, args[0], "stop")
		},
	}
}

func shareRestartCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use: "restart <CRID>", Short: "Restart sharing a local app", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeShareState(cmd.Context(), opts, args[0], "restart")
		},
	}
}

func shareStatusCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use: "status <CRID>", Short: "Show sharing state", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := opts.newClient()
			if err != nil {
				return err
			}
			sharing, err := client.Sharing(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			local, _, err := readLocalShareIfPresent(cmd.Context(), opts, args[0])
			if err != nil {
				return err
			}
			target := ""
			if local != nil {
				if err := validateLocalSharing(local, sharing); err != nil {
					return err
				}
				target = local.TargetURL
			}
			return opts.printer().Sharing(target, sharing)
		},
	}
}

func changeShareState(ctx context.Context, opts *globalOpts, id, action string) error {
	if action == "stop" {
		return stopShare(ctx, opts, id)
	}
	if err := requireBackgroundShareSupport(runtime.GOOS); err != nil {
		return err
	}
	registry, daemon, _, err := openShareControl(opts)
	if err != nil {
		return err
	}
	local, err := registry.Get(ctx, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no local share is registered for %s", id)
		}
		return err
	}
	if err := opts.preflightTarget(ctx, local.LocalIP, local.LocalPort); err != nil {
		return err
	}
	client, err := opts.newClient()
	if err != nil {
		return err
	}
	prior, err := client.Sharing(ctx, local.CRID)
	if err != nil {
		return err
	}
	if err := validateLocalSharing(local, prior); err != nil {
		return err
	}
	authorityAction := action
	if action == "start" && local.DesiredState == string(qurlapi.DesiredStateOff) && prior.DesiredState == qurlapi.DesiredStateOn {
		// A resource-local terminal denial turns only the local row off. The
		// service can still be desired-on at the old epoch. A plain idempotent
		// PUT would return that same fenced epoch and the local registry must
		// reject an off-to-on contradiction at it, so start rotates the epoch.
		authorityAction = "restart"
	}
	sharing, compensateOff, err := changeAuthoritativeSharing(ctx, client, local.CRID, prior, authorityAction)
	if err != nil {
		return err
	}
	if err := validateLocalSharing(local, sharing); err != nil {
		return compensateShareChange(err, compensateOff, client, registry, local, sharing)
	}
	updated, updateErr := registry.SetDesired(ctx, local.ResourceID, string(sharing.DesiredState), sharing.ServingEpoch)
	if updateErr != nil {
		return compensateShareChange(updateErr, compensateOff, client, registry, local, sharing)
	}
	local = updated
	if err := daemon.Ensure(ctx); err != nil {
		return compensateShareChange(err, compensateOff, client, registry, local, sharing)
	}
	sharing, err = waitForSharing(ctx, client, local, sharing.ServingEpoch, opts.sharingWaitLimit)
	if err != nil {
		return err
	}
	return opts.printer().Sharing(local.TargetURL, sharing)
}

func requireBackgroundShareSupport(goos string) error {
	if goos == "darwin" {
		return nil
	}
	return fmt.Errorf("background local sharing is currently supported only on macOS; use qurl publish --foreground on %s", goos)
}

func changeAuthoritativeSharing(ctx context.Context, client qurlapi.Client, id string, prior *qurlapi.Sharing, action string) (*qurlapi.Sharing, bool, error) {
	switch action {
	case "start":
		sharing, err := client.SetSharing(ctx, id, qurlapi.DesiredStateOn)
		return sharing, prior.DesiredState == qurlapi.DesiredStateOff, err
	case "restart":
		sharing, err := restartSharingReconciled(ctx, client, id, prior)
		return sharing, true, err
	default:
		return nil, false, fmt.Errorf("unsupported share lifecycle action %q", action)
	}
}

func compensateShareChange(cause error, enabled bool, client qurlapi.Client, registry localShareRegistry, local *connectorstate.LocalShare, sharing *qurlapi.Sharing) error {
	if !enabled {
		return cause
	}
	compensationCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	off, offErr := client.SetSharing(compensationCtx, sharing.CRID, qurlapi.DesiredStateOff)
	var localErr error
	if offErr == nil {
		_, localErr = registry.SetDesired(compensationCtx, local.ResourceID, string(off.DesiredState), off.ServingEpoch)
	}
	return errors.Join(cause, offErr, localErr)
}

func restartSharingReconciled(ctx context.Context, client qurlapi.Client, id string, prior *qurlapi.Sharing) (*qurlapi.Sharing, error) {
	restarted, err := client.RestartSharing(ctx, id)
	if err == nil {
		return restarted, nil
	}
	if ctx.Err() != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	var apiErr *qurlapi.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusTooManyRequests && apiErr.StatusCode < http.StatusInternalServerError {
		return nil, err
	}
	// Restart has no service-backed idempotency key. A transport or transient
	// response failure may still mean the POST advanced the epoch, so reconcile
	// exactly once with the authoritative state and never replay the POST.
	current, reconcileErr := client.Sharing(ctx, id)
	if reconcileErr != nil {
		return nil, errors.Join(
			fmt.Errorf("qURL sharing restart result is ambiguous: %w", err),
			fmt.Errorf("read authoritative sharing state: %w", reconcileErr),
		)
	}
	if prior != nil && current.ResourceID == prior.ResourceID && current.DesiredState == qurlapi.DesiredStateOn && current.ServingEpoch > prior.ServingEpoch {
		return current, nil
	}
	return nil, fmt.Errorf("qURL sharing restart result is ambiguous and authoritative state did not advance: %w", err)
}

// stopShare commits the authoritative cloud-off transition before consulting
// optional local state. A machine that never published this CRID therefore
// needs no Connector directory, registry, log path, or daemon controller.
func stopShare(ctx context.Context, opts *globalOpts, id string) error {
	client, err := opts.newClient()
	if err != nil {
		return err
	}
	sharing, err := client.SetSharing(ctx, id, qurlapi.DesiredStateOff)
	if err != nil {
		return err
	}
	local, stateDir, err := readLocalShareIfPresent(ctx, opts, id)
	if err != nil {
		// The cloud is already off. Surface corrupt/inaccessible existing local
		// state without attempting to undo that fail-closed transition.
		return err
	}
	if local == nil {
		return opts.printer().Sharing("", sharing)
	}
	if err := validateLocalSharing(local, sharing); err != nil {
		return err
	}
	registry, err := opts.openShareRegistry(stateDir)
	if err != nil {
		return err
	}
	local, err = registry.SetDesired(ctx, local.ResourceID, string(sharing.DesiredState), sharing.ServingEpoch)
	if err != nil {
		return err
	}
	logDir, err := connectordaemon.DefaultLogDir()
	if err != nil {
		return err
	}
	if _, err := opts.newShareDaemon(stateDir, logDir).ReloadIfRunning(ctx); err != nil {
		return err
	}
	return opts.printer().Sharing(local.TargetURL, sharing)
}

func readLocalShareIfPresent(ctx context.Context, opts *globalOpts, id string) (*connectorstate.LocalShare, string, error) {
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		if errors.Is(err, connectorstate.ErrNoDefaultStateDir) {
			return nil, "", nil
		}
		return nil, "", err
	}
	shares, present, err := connectorstate.ReadLocalSharesIfPresent(ctx, stateDir)
	if err != nil || !present {
		return nil, stateDir, err
	}
	for i := range shares {
		share := shares[i]
		if share.ResourceID == id || share.CRID == id || share.ConnectorID == id {
			return &share, stateDir, nil
		}
	}
	return nil, stateDir, nil
}

func openShareControl(opts *globalOpts) (localShareRegistry, shareDaemonController, string, error) {
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return nil, nil, "", err
	}
	registry, err := opts.openShareRegistry(stateDir)
	if err != nil {
		return nil, nil, "", err
	}
	logDir, err := connectordaemon.DefaultLogDir()
	if err != nil {
		return nil, nil, "", err
	}
	return registry, opts.newShareDaemon(stateDir, logDir), stateDir, nil
}

func validateLocalSharing(local *connectorstate.LocalShare, sharing *qurlapi.Sharing) error {
	if local == nil || sharing == nil {
		return errors.New("qURL sharing response is incomplete")
	}
	if sharing.ResourceID != local.ResourceID || sharing.CRID != local.CRID {
		return fmt.Errorf("qURL sharing response identity does not match local share %s", local.CRID)
	}
	return nil
}

func waitForSharing(ctx context.Context, client qurlapi.Client, local *connectorstate.LocalShare, epoch uint64, limit time.Duration) (*qurlapi.Sharing, error) {
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		sharing, err := client.Sharing(waitCtx, local.CRID)
		if err != nil {
			return nil, err
		}
		if err := validateLocalSharing(local, sharing); err != nil {
			return nil, err
		}
		if sharing.ServingEpoch < epoch {
			return nil, fmt.Errorf("qURL sharing state regressed to serving epoch %d below %d", sharing.ServingEpoch, epoch)
		}
		if sharing.ServingEpoch > epoch {
			return nil, fmt.Errorf("qURL sharing state advanced to serving epoch %d while waiting for %d", sharing.ServingEpoch, epoch)
		}
		if sharing.ServingEpoch == epoch && sharing.ConnectionState == qurlapi.ConnectionServing {
			return sharing, nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("wait for qURL share %s to serve: %w", local.CRID, waitCtx.Err())
		case <-timer.C:
		}
	}
}

func preflightLocalTarget(ctx context.Context, ip string, port int) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || !parsed.IsLoopback() || port < 1 || port > 65535 {
		return errors.New("local share target must be a loopback IP and valid port")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	address := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return fmt.Errorf("local app is not accepting TCP connections at %s: start it, then try again: %w", address, err)
	}
	return conn.Close()
}
