package supervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/policy/security"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// FRPFactoryConfig carries the production runner factory's inputs.
type FRPFactoryConfig struct {
	// Knocker supplies the cycle RunID for the Login binding and serves the
	// redial refreshes. Required.
	Knocker knock.CycleKnocker
	// ResourceID keys the redial refreshes' overlay. Required.
	ResourceID string
	// Proxies is the route set the FRP service registers each cycle.
	// Required non-empty.
	Proxies []v1.ProxyConfigurer
	// Logger receives runner and refresher events; nil uses slog.Default().
	Logger *slog.Logger
	// OnAuthenticatedReady is called after a tunnel Login is authenticated
	// and its accepted RunID exactly matches the current native cycle RunID.
	// It is optional and must return immediately; use a nonblocking channel
	// send to hand work off, and guard it with sync.Once when the caller only
	// needs the first ready signal across supervised reconnect cycles.
	OnAuthenticatedReady func()
	// RedialKnockGate overrides the redial refresher's debounce; zero means
	// the package default. Test-only injection.
	RedialKnockGate time.Duration
}

// NewFRPRunnerFactory builds the production RunnerFactory: each supervised
// cycle constructs a real FRP client service whose Login is bound to the
// knocker's current cycle RunID and whose physical redials re-knock through
// the redial refresher.
//
// The proxy set is loaded once into a config source shared across cycles (the
// FRP service re-registers the same proxies on every reconnect), so the
// factory validates the proxies eagerly and cycle construction cannot fail on
// them later.
func NewFRPRunnerFactory(cfg *FRPFactoryConfig) (RunnerFactory, error) {
	if cfg == nil {
		return nil, errors.New("qURL Connector supervisor: FRP runner factory configuration is nil")
	}
	// Keep the factory immutable even if a caller reuses its config struct.
	config := *cfg
	if config.Knocker == nil {
		return nil, errors.New("qURL Connector supervisor: FRP runner factory requires a cycle knocker")
	}
	if config.ResourceID == "" {
		return nil, errors.New("qURL Connector supervisor: FRP runner factory requires the knock resource")
	}
	if len(config.Proxies) == 0 {
		return nil, errors.New("qURL Connector supervisor: FRP runner factory requires at least one proxy")
	}
	gate := config.RedialKnockGate
	if gate <= 0 {
		gate = redialKnockGate
	}
	for _, proxy := range config.Proxies {
		proxy.Complete()
	}
	cfgSource := source.NewConfigSource()
	if err := cfgSource.ReplaceAll(config.Proxies, nil); err != nil {
		return nil, fmt.Errorf("qURL Connector supervisor: load proxy configs: %w", err)
	}
	aggregator := source.NewAggregator(cfgSource)

	return func(cycleCommon *v1.ClientCommonConfig) (FRPRunner, error) {
		// The cycle RunID must exist before the service is built: the fork
		// presents it on the first Login, and the admission hook refuses a
		// session under any other value. A missing or noncanonical RunID
		// here is a sequencing bug, not a transient — fail the factory.
		runID := config.Knocker.CycleRunID()
		if err := qurl.ValidateCycleRunID(runID); err != nil {
			return nil, fmt.Errorf("native cycle RunID unavailable for tunnel login: %w", err)
		}
		runner := &cycleRunner{
			resourceID:           config.ResourceID,
			cycleRunID:           runID,
			logger:               config.Logger,
			onAuthenticatedReady: config.OnAuthenticatedReady,
		}
		refresher := &redialKnockRefresher{
			knocker:        config.Knocker,
			resourceID:     config.ResourceID,
			gate:           gate,
			logger:         config.Logger,
			requestRestart: runner.requestRestart,
		}
		svc, err := frpclient.NewService(frpclient.ServiceOptions{
			Common:       cycleCommon,
			InitialRunID: runID,
			// The fork calls this synchronously once the server has accepted
			// and authenticated the Login and before any proxy registration:
			// the only evidence-based admission signal, and the runner's
			// RunID verification point. This is the second copy of that
			// contract; the TODO(upstream-contract) on
			// cycleRunner.onFirstLoginSuccess is the one carrying the fork
			// call sites, so a bump must re-read it and then fix this too.
			OnFirstLoginSuccess:    runner.onFirstLoginSuccess,
			ConfigSourceAggregator: aggregator,
			UnsafeFeatures:         &security.UnsafeFeatures{},
			ConnectorCreator:       newKnockingConnectorCreator(refresher),
		})
		if err != nil {
			return nil, err
		}
		runner.svc = svc
		return runner, nil
	}, nil
}
