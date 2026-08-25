package matchedcohort

import (
	"context"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestRefreshSelectedIdentityStatesAdvancesSharedSetInParallel(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	refresher := &fakeStateRefresher{started: make(chan string, 2), release: make(chan struct{})}
	cohort, _ := cohortFor(Plan{Cohorts: authority.Cohorts})
	done := make(chan struct {
		updates []StateUpdate
		err     error
	}, 1)
	go func() {
		updates, err := consumer.RefreshSelectedIdentityStates(context.Background(), authority,
			[]string{labelDirectA, labelDirectB}, qurl.HubBootstrap{Host: cohort.HubHost, Port: cohort.HubPort,
				ServerPublicKeyB64: cohort.HubServerPublicKeyB64}, cohort.CellEndpoint, refresher)
		done <- struct {
			updates []StateUpdate
			err     error
		}{updates, err}
	}()
	first, second := <-refresher.started, <-refresher.started
	if first == second {
		t.Fatalf("refresh did not start distinct identities: %q/%q", first, second)
	}
	close(refresher.release)
	result := <-done
	if result.err != nil || len(result.updates) != 2 || result.updates[0].Label != labelDirectA || result.updates[1].Label != labelDirectB {
		t.Fatalf("refresh = %#v, %v", result.updates, result.err)
	}
	for _, update := range result.updates {
		if update.Before.Key != update.After.Key || update.Before.VersionID == update.After.VersionID || update.Before.SHA256 == update.After.SHA256 {
			t.Fatalf("state did not advance by exact CAS: %#v", update)
		}
	}
}

func TestRefreshSelectedIdentityStatesRejectsWrongPhysicalCohort(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	cohort, _ := cohortFor(Plan{Cohorts: authority.Cohorts})
	refresher := &fakeStateRefresher{endpointHost: "other-ac.sandbox.layerv.xyz"}
	updates, err := consumer.RefreshSelectedIdentityStates(context.Background(), authority, []string{labelDirectA, labelDirectB},
		qurl.HubBootstrap{Host: cohort.HubHost, Port: 443, ServerPublicKeyB64: cohort.HubServerPublicKeyB64},
		cohort.CellEndpoint, refresher)
	if err == nil || updates != nil {
		t.Fatalf("wrong physical cohort refresh = %#v, %v", updates, err)
	}
}

func TestRefreshSelectedIdentityStatesRejectsWrongHubKeyBeforeRefresh(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	cohort, _ := cohortFor(Plan{Cohorts: authority.Cohorts})
	refresher := &fakeStateRefresher{}
	updates, err := consumer.RefreshSelectedIdentityStates(context.Background(), authority, []string{labelDirectA, labelDirectB},
		qurl.HubBootstrap{Host: cohort.HubHost, Port: 443, ServerPublicKeyB64: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		cohort.CellEndpoint, refresher)
	if err == nil || updates != nil || refresher.calls.Load() != 0 {
		t.Fatalf("wrong Hub key refresh = %#v, calls=%d, err=%v", updates, refresher.calls.Load(), err)
	}
}

func TestRefreshSelectedIdentityStatesRejectsRouteDriftBeforeRefresh(t *testing.T) {
	for _, labels := range [][]string{{labelDirectA, labelDirectB}, {labelRelayC, labelRelayD}} {
		for _, field := range []string{"hub-host", "cell-host"} {
			t.Run(labels[0]+"/"+field, func(t *testing.T) {
				consumer, authority, _ := lifecycleFixture(t)
				cohort, _ := cohortFor(Plan{Cohorts: authority.Cohorts})
				hub := qurl.HubBootstrap{Host: cohort.HubHost, Port: cohort.HubPort, ServerPublicKeyB64: cohort.HubServerPublicKeyB64}
				endpoint := cohort.CellEndpoint
				if field == "hub-host" {
					hub.Host = "other-hub.sandbox.layerv.xyz"
				} else {
					endpoint.Host = "other-cell.sandbox.layerv.xyz"
				}
				refresher := &fakeStateRefresher{}
				updates, err := consumer.RefreshSelectedIdentityStates(context.Background(), authority, labels, hub, endpoint, refresher)
				if err == nil || updates != nil || refresher.calls.Load() != 0 {
					t.Fatalf("route drift refresh = %#v, calls=%d, err=%v", updates, refresher.calls.Load(), err)
				}
			})
		}
	}
}

type fakeStateRefresher struct {
	started      chan string
	release      chan struct{}
	endpointHost string
	calls        atomic.Int32
}

func (f *fakeStateRefresher) refresh(ctx context.Context, store qurl.AgentStateStore, _ qurl.HubBootstrap,
	identity *FixedIdentity, cohort *CohortPlan,
) (*RuntimeBinding, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.started <- identity.Label
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	state, err := store.LoadAgentState(ctx)
	if err != nil || state == nil || state.Assignment == nil {
		return nil, errors.New("load fixed state")
	}
	state.Assignment.EndpointRevision++
	state.Assignment.LeaseExpiresAt = time.Unix(2_100_000_000, 0).UTC()
	if err := store.SaveAgentState(ctx, state); err != nil {
		return nil, err
	}
	host := cohort.CellEndpoint.Host
	if f.endpointHost != "" {
		host = f.endpointHost
	}
	return &RuntimeBinding{AgentID: identity.AgentID, PublicKeyB64: identity.AgentPublicKeyB64,
		DeviceAPIKeyID: identity.DeviceAPIKeyID, CellID: cohort.CellID, AssignmentGeneration: cohort.AssignmentGeneration,
		Endpoint: qurl.NHPUDPEndpoint{Host: host, Port: 443, ServerPublicKeyB64: cohort.CellEndpoint.ServerPublicKeyB64}}, nil
}
