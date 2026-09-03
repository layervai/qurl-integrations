package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// ShareManager is the reconciler the daemon's IPC server drives, whichever
// GroupMode built it: Run owns every session group until ctx ends, Trigger
// requests a coalesced reconcile, and Running and Diagnostics feed /status.
type ShareManager interface {
	Run(context.Context) error
	Trigger()
	Running() map[string]string
	Diagnostics() map[string]ResourceDiagnostic
}

// NewShareManager builds the reconciler for mode over the shared registry and
// group factory. GroupModeSingle is the one-group Manager. GroupModePerShare is
// a PerShareManager that runs one such Manager per desired-on share: the
// factory — and the native admitter behind it — stays shared, while every
// share spends an admission of its own.
func NewShareManager(registry Registry, factory GroupFactory, mode GroupMode) (ShareManager, error) {
	switch mode {
	case GroupModeSingle:
		manager, err := NewManager(registry, factory)
		if err != nil {
			return nil, err
		}
		return manager, nil
	case GroupModePerShare:
		manager, err := NewPerShareManager(registry, factory)
		if err != nil {
			return nil, err
		}
		return manager, nil
	default:
		if _, err := ParseGroupMode(string(mode)); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("share group mode %q has no reconciler", mode)
	}
}

// PerShareSoftCap is the share count above which per-share mode is expected to
// exhaust the per-owner platform budgets for sessions and heartbeat streams:
// single mode amortizes one of each across every share, per-share spends one
// of each per share. The daemon does not refuse rows above it — the platform's
// answer is authoritative — but warns on every reconcile so an operator can
// attribute a partially retrying fleet to the budget rather than to the shares.
// TODO(upstream-contract): mirrors the per-owner Connector session and
// heartbeat-stream budgets that capped the pre-#1326 one-session-per-share
// daemon near ~300 shares; move it with those budgets.
const PerShareSoftCap = 300

// PerShareManager reconciles durable local intent into one session group per
// desired-on share. Each share gets its own Manager over a one-row view of the
// registry, so every single-group semantic holds per share: publish adds a
// group, stop removes exactly that group, restart re-registers that share's
// route on its own group, and a refused or permanently denied share is handled
// by its own group while its siblings keep serving. The GroupFactory is shared,
// so every group knocks through the same native admitter — one admission each,
// serialized on the admitter — which is what a platform that fences one
// resource per session accepts.
type PerShareManager struct {
	registry Registry
	factory  GroupFactory

	mu       sync.Mutex
	groups   map[string]*shareGroup // resource ID -> its group
	lifetime context.Context

	trigger chan struct{}
	// failed carries the first exit a group's Manager made on its own (not one
	// this manager asked for), so the daemon fails as loudly as the
	// single-group daemon would.
	failed chan error

	// groupStopTimeout bounds waiting for removed groups to retire their
	// sessions, on top of each Manager's own bounded stop.
	groupStopTimeout time.Duration
	// configure tunes every group Manager as it is built; tests shorten backoffs.
	configure func(*Manager)
}

// shareGroup is one share's Manager, the registry view it reconciles, and the
// lifetime the parent controls.
type shareGroup struct {
	manager *Manager
	view    *shareView
	cancel  context.CancelFunc
	done    chan struct{}
	// exitErr is the Manager's Run result; it is written before done closes.
	exitErr error
}

// NewPerShareManager builds a one-group-per-share reconciler.
func NewPerShareManager(registry Registry, factory GroupFactory) (*PerShareManager, error) {
	if registry == nil || factory == nil {
		return nil, errors.New("share daemon requires a registry and group factory")
	}
	return &PerShareManager{
		registry: registry, factory: factory, groups: map[string]*shareGroup{},
		trigger: make(chan struct{}, 1), failed: make(chan error, 1),
		groupStopTimeout: 15 * time.Second,
	}, nil
}

// Trigger requests a coalesced asynchronous reconciliation.
func (m *PerShareManager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends. Every exit path stops every group so a
// registry failure cannot bypass exact admission retirement; a group that
// fails to retire cleanly on that final path is reported in the result.
func (m *PerShareManager) Run(ctx context.Context) (retErr error) {
	m.mu.Lock()
	m.lifetime = ctx
	m.mu.Unlock()
	defer func() {
		retErr = errors.Join(retErr, m.stopAllGroups())
	}()
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-m.failed:
			return err
		case <-m.trigger:
			if err := m.Reconcile(ctx); err != nil {
				return err
			}
		}
	}
}

// Reconcile applies one desired-state snapshot. It lists the shared registry
// once, starts a group for every newly desired-on share, refreshes the row of
// every share still desired-on and re-triggers only the groups whose row
// actually changed (so an epoch advance becomes a RestartRoute on that group
// alone, and an unrelated lifecycle command costs no push on the others), and
// stops the group of every share that is no longer desired-on.
//
// Only the registry listing is fatal. A removed group that does not retire in
// time is logged and left to finish on its own, so one stuck share never takes
// its serving siblings down — the mirror of single mode tolerating one route's
// failed push. Groups run under Run's lifetime context, as in
// Manager.Reconcile; ctx bounds this reconcile only. Production drives
// Reconcile solely from Run's long-lived context: a group started before Run
// (tests only) is bound to context.Background and retired only by Run's own
// shutdown path.
func (m *PerShareManager) Reconcile(ctx context.Context) error {
	shares, err := m.registry.List(ctx)
	if err != nil {
		return fmt.Errorf("list desired local shares: %w", err)
	}
	desired := desiredShares(shares)
	if len(desired) > PerShareSoftCap {
		slog.WarnContext(ctx, "share daemon per-share mode exceeds its soft cap; the platform's per-owner session budgets may leave the excess retrying",
			"shares", len(desired), "soft_cap", PerShareSoftCap)
	}
	keep := make(map[string]struct{}, len(desired))
	for i := range desired {
		keep[desired[i].ResourceID] = struct{}{}
	}
	var stopped []*shareGroup
	m.mu.Lock()
	for resourceID, group := range m.groups {
		if _, ok := keep[resourceID]; ok {
			continue
		}
		delete(m.groups, resourceID)
		stopped = append(stopped, group)
	}
	for i := range desired {
		share := &desired[i]
		if group, ok := m.groups[share.ResourceID]; ok {
			if group.view.set(share) {
				group.manager.Trigger()
			}
			continue
		}
		group, err := m.startGroupLocked(share)
		if err != nil {
			// Unreachable by construction (the factory is non-nil and the view is
			// ours); the next reconcile starts the group afresh rather than one
			// share's failure ending the daemon.
			slog.ErrorContext(ctx, "share daemon could not start a share group; the next reconcile will retry it",
				"resource_id", share.ResourceID, "error", redactShareError(err))
			continue
		}
		m.groups[share.ResourceID] = group
	}
	m.mu.Unlock()
	if err := m.stopGroups(stopped); err != nil {
		slog.WarnContext(ctx, "share daemon could not cleanly retire a removed share group; its siblings keep serving",
			"error", redactShareError(err))
	}
	return nil
}

// startGroupLocked builds and launches one share's Manager. The group is
// detached from the reconcile context and bound to Run's lifetime, exactly as
// Manager.launchRunner detaches its runner.
func (m *PerShareManager) startGroupLocked(share *connectorstate.LocalShare) (*shareGroup, error) {
	view := &shareView{parent: m, share: *share}
	manager, err := NewManager(view, m.factory)
	if err != nil {
		return nil, err
	}
	if m.configure != nil {
		m.configure(manager)
	}
	parent := m.lifetime
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	group := &shareGroup{manager: manager, view: view, cancel: cancel, done: make(chan struct{})}
	resourceID := share.ResourceID
	go func() {
		err := manager.Run(runCtx)
		group.exitErr = err
		close(group.done)
		if err != nil && runCtx.Err() == nil {
			m.reportGroupFailure(resourceID, err)
		}
	}()
	return group, nil
}

func (m *PerShareManager) reportGroupFailure(resourceID string, err error) {
	select {
	case m.failed <- fmt.Errorf("share group for resource %s failed: %w", resourceID, err):
	default:
	}
}

// stopGroups cancels every group at once and waits for each to retire its
// session, bounded by one shared deadline so one wedged shutdown cannot hang
// the daemon behind the others. A group that does not stop in time is left to
// finish on its own — its context is already canceled — and reported.
func (m *PerShareManager) stopGroups(groups []*shareGroup) error {
	if len(groups) == 0 {
		return nil
	}
	for _, group := range groups {
		group.cancel()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), m.groupStopTimeout)
	defer cancel()
	var errs error
	for _, group := range groups {
		select {
		case <-group.done:
			errs = errors.Join(errs, groupStopError(group.view.resourceID(), group.exitErr))
		case <-stopCtx.Done():
			errs = errors.Join(errs, fmt.Errorf("share group for resource %s did not stop within %s", group.view.resourceID(), m.groupStopTimeout))
		}
	}
	return errs
}

// groupStopError classifies a group Manager's Run result after this manager
// canceled it. The expected exit is the cancellation and nothing else: any
// other leaf in the returned error tree — the Manager's bounded stop expiring
// before the session retired, or a teardown failure joined with the
// cancellation — is surfaced rather than mistaken for a clean stop.
func groupStopError(resourceID string, err error) error {
	if err == nil || onlyCancellation(err) {
		return nil
	}
	return fmt.Errorf("share group for resource %s did not stop cleanly: %w", resourceID, err)
}

// onlyCancellation reports whether every leaf of err's wrap tree is
// context.Canceled.
func onlyCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		leaves := joined.Unwrap()
		if len(leaves) == 0 {
			return false
		}
		for _, leaf := range leaves {
			if !onlyCancellation(leaf) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyCancellation(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}

func (m *PerShareManager) stopAllGroups() error {
	m.mu.Lock()
	groups := make([]*shareGroup, 0, len(m.groups))
	for resourceID := range m.groups {
		groups = append(groups, m.groups[resourceID])
	}
	m.groups = map[string]*shareGroup{}
	m.mu.Unlock()
	return m.stopGroups(groups)
}

func (m *PerShareManager) snapshotGroups() []*shareGroup {
	m.mu.Lock()
	defer m.mu.Unlock()
	groups := make([]*shareGroup, 0, len(m.groups))
	for resourceID := range m.groups {
		groups = append(groups, m.groups[resourceID])
	}
	return groups
}

// Running returns the union of every group's running set: a share is reported
// only once its own session group is running.
func (m *PerShareManager) Running() map[string]string {
	result := map[string]string{}
	for _, group := range m.snapshotGroups() {
		for resourceID, crid := range group.manager.Running() {
			result[resourceID] = crid
		}
	}
	return result
}

// Diagnostics returns the union of every group's redacted diagnostics.
func (m *PerShareManager) Diagnostics() map[string]ResourceDiagnostic {
	result := map[string]ResourceDiagnostic{}
	for _, group := range m.snapshotGroups() {
		for resourceID, diagnostic := range group.manager.Diagnostics() {
			result[resourceID] = diagnostic
		}
	}
	return result
}

// shareView is the one-row Registry a per-share group's Manager reconciles
// against. The parent lists the shared registry once per reconcile and pushes
// each share's current row into its view before triggering that group, so N
// groups cost one registry read per reconcile rather than N. A durable disable
// (the group's own knock permanently denied) writes through to the shared
// registry, turns the view off so the group retires rather than re-knocks, and
// triggers the parent so the group is pruned.
type shareView struct {
	parent *PerShareManager

	mu    sync.Mutex
	share connectorstate.LocalShare
}

// set replaces the row and reports whether it changed in any way the group
// reconciles on; the registry's own write timestamp is not one.
func (v *shareView) set(share *connectorstate.LocalShare) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if sameShareRow(&v.share, share) {
		return false
	}
	v.share = *share
	return true
}

func sameShareRow(a, b *connectorstate.LocalShare) bool {
	x, y := *a, *b
	x.UpdatedAt, y.UpdatedAt = time.Time{}, time.Time{}
	return x == y
}

func (v *shareView) resourceID() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.share.ResourceID
}

// List returns the view's one row.
func (v *shareView) List(context.Context) ([]connectorstate.LocalShare, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return []connectorstate.LocalShare{v.share}, nil
}

// DisableAtCurrentEpoch persists the row off through the shared registry. A
// row that was concurrently deleted is already terminal, exactly as
// Manager.persistResourceGone treats it.
func (v *shareView) DisableAtCurrentEpoch(ctx context.Context, resourceID string, epoch uint64) (*connectorstate.LocalShare, error) {
	share, err := v.parent.registry.DisableAtCurrentEpoch(ctx, resourceID, epoch)
	switch {
	case err == nil && share != nil:
		v.set(share)
	case err == nil || errors.Is(err, os.ErrNotExist):
		v.mu.Lock()
		v.share.DesiredState = "off"
		v.mu.Unlock()
	default:
		return nil, err
	}
	v.parent.Trigger()
	return share, err
}

var (
	_ ShareManager = (*Manager)(nil)
	_ ShareManager = (*PerShareManager)(nil)
	_ Registry     = (*shareView)(nil)
)
