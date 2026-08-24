package matchedcohort

import (
	"context"
	"errors"
	"fmt"

	qurl "github.com/layervai/qurl-go/qurl"
)

// StateUpdate binds one fixed state key before and after an authenticated Hub
// refresh. The fixed identity stays immutable; only its durable SDK state
// version may advance.
type StateUpdate struct {
	Label  string         `json:"label"`
	Before StateReference `json:"before"`
	After  StateReference `json:"after"`
}

// IdentityStateRefresher is the narrow authenticated Hub refresh seam.
type IdentityStateRefresher interface {
	refresh(context.Context, qurl.AgentStateStore, qurl.HubBootstrap, *FixedIdentity, *CohortPlan) (*RuntimeBinding, error)
}

type qurlIdentityStateRefresher struct{}

func (qurlIdentityStateRefresher) refresh(ctx context.Context, store qurl.AgentStateStore, hub qurl.HubBootstrap,
	_ *FixedIdentity, _ *CohortPlan,
) (*RuntimeBinding, error) {
	_, binding, err := qurl.RefreshAgentRuntime(ctx, hub, store, qurl.WithAgentRuntimePinnedAssignment())
	if err != nil {
		return nil, err
	}
	defer binding.Destroy()
	assignment := binding.Assignment()
	return &RuntimeBinding{AgentID: binding.AgentID, PublicKeyB64: binding.PublicKeyB64, DeviceAPIKeyID: binding.DeviceAPIKeyID,
		CellID: assignment.CellID, AssignmentGeneration: assignment.AssignmentGeneration, Endpoint: assignment.Endpoint}, nil
}

// RefreshSelectedIdentityStates refreshes the exact ordered color projection
// through one signed Hub before operation preparation. Independent identities
// run concurrently. Each qurl-go save is already a durable CAS; a partial
// result is safe to retry because no operation has been prepared or sent yet.
//
//nolint:gocognit,gocritic,gocyclo // The closed authority projection is intentionally checked at one boundary.
func (c *Consumer) RefreshSelectedIdentityStates(ctx context.Context, authority Authority, color string, labels []string,
	hub qurl.HubBootstrap, expectedEndpoint qurl.NHPUDPEndpoint, refresher IdentityStateRefresher,
) ([]StateUpdate, error) {
	if c == nil || c.Blobs == nil || ValidateAuthority(authority) != nil ||
		(color != ColorBlue && color != ColorGreen) || len(labels) == 0 || len(labels) > 4 ||
		!validDNS(hub.Host) || hub.Port != 443 || !validBase64Raw32(hub.ServerPublicKeyB64) ||
		!validDNS(expectedEndpoint.Host) || expectedEndpoint.Port != 443 || !validBase64Raw32(expectedEndpoint.ServerPublicKeyB64) {
		return nil, fmt.Errorf("%w: fixed state refresh authority", errInvalidAuthority)
	}
	cohort, err := cohortFor(Plan{Cohorts: authority.Cohorts}, color)
	if err != nil {
		return nil, err
	}
	if hub.ServerPublicKeyB64 != cohort.HubServerPublicKeyB64 ||
		expectedEndpoint.ServerPublicKeyB64 != cohort.CellEndpoint.ServerPublicKeyB64 {
		return nil, fmt.Errorf("%w: signed Hub or cell endpoint key", errInvalidAuthority)
	}
	if refresher == nil {
		refresher = qurlIdentityStateRefresher{}
	}
	selected := make([]FixedIdentity, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if _, duplicate := seen[label]; duplicate || !containsLabel(label) {
			return nil, fmt.Errorf("%w: fixed state refresh labels", errInvalidAuthority)
		}
		seen[label] = struct{}{}
		found := false
		for index := range authority.Identities {
			identity := authority.Identities[index]
			if identity.Color == color && identity.Label == label {
				selected = append(selected, identity)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: fixed state refresh identity", errInvalidAuthority)
		}
	}
	type refreshResult struct {
		index  int
		update StateUpdate
		err    error
	}
	results := make(chan refreshResult, len(selected))
	for index := range selected {
		go func(index int) {
			identity := &selected[index]
			store, storeErr := NewDurableAgentStateStore(c.Blobs, identity.AgentState.Key)
			if storeErr != nil {
				results <- refreshResult{index: index, err: storeErr}
				return
			}
			before, beforeErr := store.Reference(ctx)
			if beforeErr != nil || before.Key != identity.AgentState.Key {
				results <- refreshResult{index: index, err: errors.Join(beforeErr, errStateConflict)}
				return
			}
			binding, refreshErr := refresher.refresh(ctx, store, hub, identity, &cohort)
			if refreshErr != nil {
				results <- refreshResult{index: index, err: refreshErr}
				return
			}
			if binding == nil || binding.AgentID != identity.AgentID || binding.PublicKeyB64 != identity.AgentPublicKeyB64 ||
				binding.DeviceAPIKeyID != identity.DeviceAPIKeyID || binding.CellID != cohort.CellID ||
				binding.AssignmentGeneration != cohort.AssignmentGeneration || binding.Endpoint != expectedEndpoint {
				results <- refreshResult{index: index, err: fmt.Errorf("%w: refreshed physical cohort", errStateConflict)}
				return
			}
			after, afterErr := store.Reference(ctx)
			if afterErr != nil || after.Key != before.Key || after.VersionID == "" || after.SHA256 == "" ||
				after.VersionID == before.VersionID || after.SHA256 == before.SHA256 {
				results <- refreshResult{index: index, err: errors.Join(afterErr, errStateConflict)}
				return
			}
			results <- refreshResult{index: index, update: StateUpdate{Label: identity.Label, Before: before, After: after}}
		}(index)
	}
	ordered := make([]StateUpdate, len(selected))
	var failures []error
	for range selected {
		result := <-results
		ordered[result.index] = result.update
		if result.err != nil {
			failures = append(failures, result.err)
		}
	}
	if len(failures) != 0 {
		return nil, errors.Join(failures...)
	}
	return ordered, nil
}
