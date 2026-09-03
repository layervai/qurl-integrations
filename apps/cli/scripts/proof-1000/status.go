package main

import (
	"context"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const (
	daemonStateServing  = "serving"
	daemonStateStarting = "starting"
	daemonStateRetrying = "retrying"
	daemonStateFailed   = "failed"
	daemonStateStopped  = "stopped"
)

// registryRow is the harness's read-only view of one local share row.
type registryRow = connectorstate.LocalShare

// readRegistryByConnectorID reads the CLI's owner-only local share registry
// through the CLI's own reader and indexes it by Connector ID.
func readRegistryByConnectorID(ctx context.Context, stateDir string) (map[string]registryRow, error) {
	shares, _, err := connectorstate.ReadLocalSharesIfPresent(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	rows := make(map[string]registryRow, len(shares))
	for i := range shares {
		rows[shares[i].ConnectorID] = shares[i]
	}
	return rows, nil
}

// statusSample is one reading of the daemon's `/status` restricted to the
// proof's resources. Absent means the daemon lists the resource as running
// but has not published a diagnostic yet; Missing means the daemon does not
// know the resource at all.
type statusSample struct {
	At       time.Time      `json:"at"`
	Elapsed  float64        `json:"elapsed_s"`
	Serving  int            `json:"serving"`
	Starting int            `json:"starting"`
	Retrying int            `json:"retrying"`
	Failed   int            `json:"failed"`
	Stopped  int            `json:"stopped"`
	Absent   int            `json:"absent"`
	Missing  int            `json:"missing"`
	Total    int            `json:"total"`
	Failures map[string]int `json:"failures,omitempty"`
	Err      string         `json:"error,omitempty"`
	Window   string         `json:"in_window,omitempty"`
}

func (s *statusSample) degraded() bool { return s.Err != "" || s.Serving < s.Total }

func takeStatusSample(ctx context.Context, resourceIDs []string, socket string, start time.Time) statusSample {
	now := time.Now()
	sample := statusSample{At: now, Elapsed: now.Sub(start).Seconds(), Total: len(resourceIDs)}
	status, running, err := connectordaemon.IPCClient{SocketPath: socket}.Status(ctx)
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	if !running {
		sample.Err = "daemon not running"
		sample.Missing = len(resourceIDs)
		return sample
	}
	for _, id := range resourceIDs {
		diag, ok := status.Resources[id]
		if !ok {
			if _, managed := status.Running[id]; managed {
				sample.Absent++
			} else {
				sample.Missing++
			}
			continue
		}
		sample.count(&diag)
	}
	return sample
}

func (s *statusSample) count(diag *connectordaemon.ResourceDiagnostic) {
	switch diag.State {
	case daemonStateServing:
		s.Serving++
	case daemonStateStarting:
		s.Starting++
	case daemonStateRetrying:
		s.Retrying++
	case daemonStateFailed:
		s.Failed++
	case daemonStateStopped:
		s.Stopped++
	}
	if diag.FailureCategory == "" && diag.FailureCode == "" {
		return
	}
	if s.Failures == nil {
		s.Failures = map[string]int{}
	}
	s.Failures[diag.FailureCategory+"/"+diag.FailureCode]++
}

// waitAllServing samples the daemon every interval until every proof
// resource serves or the deadline passes, handing each sample to onSample.
func waitAllServing(ctx context.Context, resourceIDs []string, socket string, start time.Time,
	deadline, interval time.Duration, onSample func(statusSample),
) (statusSample, bool) {
	if len(resourceIDs) == 0 {
		sample := takeStatusSample(ctx, resourceIDs, socket, start)
		onSample(sample)
		return sample, false
	}
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		sample := takeStatusSample(waitCtx, resourceIDs, socket, start)
		onSample(sample)
		if sample.Err == "" && sample.Total > 0 && sample.Serving == sample.Total {
			return sample, true
		}
		select {
		case <-waitCtx.Done():
			return sample, false
		case <-time.After(interval):
		}
	}
}
