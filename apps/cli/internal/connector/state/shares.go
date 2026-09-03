package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
)

const (
	// LocalSharesFile is the owner-only durable desired-state registry.
	LocalSharesFile = "local_shares.json"

	localSharesVersion = 2
	// LocalSharesMaxItems matches the connector session group's MaxGroupRoutes:
	// one daemon serves every desired-on share on one Connector session, so the
	// registry can hold as many rows as one admission carries proxies. It is
	// exported so the daemon package (which imports both this package and
	// connectorshare) can pin it to MaxGroupRoutes at compile time.
	// TODO(upstream-contract): keep LocalSharesMaxItems == connectorshare.MaxGroupRoutes.
	LocalSharesMaxItems = 2000
	localSharesMaxItems = LocalSharesMaxItems
	// localSharesMaxBytes bounds the whole registry file. A full 2000-row
	// registry of maximal rows (a ~124-char base64url resource identity and
	// CRID, a 64-char Connector ID, a 54-char routing ID, a 64-char knock
	// resource ID, and a verbose IPv6 loopback target per row) marshals to
	// ~1.7 MiB; the 4 MiB cap keeps ~2x headroom above that measured worst case.
	localSharesMaxBytes = 4 << 20
	desiredStateOn      = "on"
	desiredStateOff     = "off"
)

var (
	// ErrLocalShareOwnerConflict marks an attempt to retarget one durable
	// native identity namespace to a different account.
	ErrLocalShareOwnerConflict = errors.New("local share account owner conflicts with the native identity namespace")
	// ErrLocalShareVersionUnsupported marks a registry written with another
	// schema version. v2 deliberately does not migrate prerelease state.
	ErrLocalShareVersionUnsupported = errors.New("local share registry version is unsupported")
	errLocalShareUnchanged          = errors.New("local share registry is unchanged")
)

// LocalShare is the non-secret local half of one tunnel resource. The qURL
// service remains authoritative for desired state and serving epoch; this
// owner-only registry retains the loopback target needed to resume a desired-on
// share after login, sleep, network loss, or daemon restart.
type LocalShare struct {
	CRID               string    `json:"crid" yaml:"crid"`
	ResourceID         string    `json:"resource_id" yaml:"resource_id"`
	ConnectorID        string    `json:"connector_id" yaml:"connector_id"`
	ConnectorRoutingID string    `json:"connector_routing_id" yaml:"connector_routing_id"`
	KnockResourceID    string    `json:"knock_resource_id" yaml:"knock_resource_id"`
	TargetURL          string    `json:"target_url" yaml:"target_url"`
	LocalIP            string    `json:"local_ip" yaml:"local_ip"`
	LocalPort          int       `json:"local_port" yaml:"local_port"`
	DesiredState       string    `json:"desired_state" yaml:"desired_state"`
	ServingEpoch       uint64    `json:"serving_epoch" yaml:"serving_epoch"`
	UpdatedAt          time.Time `json:"updated_at" yaml:"-"`
}

type localSharesState struct {
	Version int                   `json:"version"`
	OwnerID string                `json:"owner_id,omitempty"`
	Shares  map[string]LocalShare `json:"shares"`
}

// LocalShareRegistry is a crash-safe owner-only registry rooted beside the
// Connector's native state. Each operation takes the existing cross-process
// Connector-state lock, so foreground commands and the daemon cannot lose one
// another's updates.
type LocalShareRegistry struct{ dir string }

// BindOwner durably binds this native identity namespace to the account owner
// authenticated during initial publish. The owner is non-secret, but changing
// it would retarget every native operation, so an existing binding is
// immutable.
func (r *LocalShareRegistry) BindOwner(ctx context.Context, ownerID string) (retErr error) {
	ownerID = strings.TrimSpace(ownerID)
	if !validLocalOwnerID(ownerID) {
		return errors.New("local share account owner is invalid")
	}
	state, unlock, err := r.loadLocked(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	if state.OwnerID != "" && state.OwnerID != ownerID {
		return fmt.Errorf("%w: bound to %q, requested %q", ErrLocalShareOwnerConflict, state.OwnerID, ownerID)
	}
	if state.OwnerID == ownerID {
		return nil
	}
	state.OwnerID = ownerID
	return writeLocalShares(r.dir, state)
}

// OwnerID returns the durable account owner without reading an API key. A
// warm daemon uses this value with trusted build/deployment configuration, so
// steady-state native operations require only the device keypair.
func (r *LocalShareRegistry) OwnerID(ctx context.Context) (ownerID string, present bool, err error) {
	state, unlock, err := r.loadLocked(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = unlock() }()
	return state.OwnerID, state.OwnerID != "", nil
}

// ReadLocalSharesIfPresent reads an existing registry without creating the
// state directory or registry. Resource reads use it only after registered
// authentication so a remote-only result never creates local share rows or a
// daemon control surface as a side effect.
func ReadLocalSharesIfPresent(ctx context.Context, dir string) ([]LocalShare, bool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, false, errors.New("local share registry directory is empty")
	}
	if _, err := os.Lstat(filepath.Join(dir, LocalSharesFile)); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("inspect local share registry: %w", err)
	}
	registry := &LocalShareRegistry{dir: dir}
	shares, err := registry.List(ctx)
	return shares, err == nil, err
}

// OpenLocalShareRegistry validates or creates an owner-only registry directory.
func OpenLocalShareRegistry(dir string) (*LocalShareRegistry, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("local share registry directory is empty")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, fmt.Errorf("prepare local share registry: %w", err)
	}
	return &LocalShareRegistry{dir: dir}, nil
}

// Put commits a validated row without permitting lifecycle or identity regression.
func (r *LocalShareRegistry) Put(ctx context.Context, candidate *LocalShare) error {
	if candidate == nil {
		return errors.New("local share is nil")
	}
	share := *candidate
	share.UpdatedAt = time.Now().UTC()
	if err := validateLocalShare(&share); err != nil {
		return err
	}
	return r.update(ctx, func(state *localSharesState) error {
		if state.OwnerID == "" {
			return errors.New("local share account owner must be bound before a share is stored")
		}
		if existing, ok := state.Shares[share.ResourceID]; ok {
			if existing.CRID != share.CRID || existing.ConnectorID != share.ConnectorID ||
				existing.ConnectorRoutingID != share.ConnectorRoutingID || existing.KnockResourceID != share.KnockResourceID {
				return errors.New("local share immutable resource identity changed")
			}
			if share.ServingEpoch < existing.ServingEpoch {
				return fmt.Errorf("refuse stale serving epoch %d below local epoch %d", share.ServingEpoch, existing.ServingEpoch)
			}
			if share.ServingEpoch == existing.ServingEpoch && share.DesiredState != existing.DesiredState {
				return fmt.Errorf("refuse contradictory desired state %q at serving epoch %d", share.DesiredState, share.ServingEpoch)
			}
			targetChanged := share.TargetURL != existing.TargetURL || share.LocalIP != existing.LocalIP || share.LocalPort != existing.LocalPort
			if targetChanged && share.ServingEpoch == existing.ServingEpoch {
				return fmt.Errorf("refuse local target change without a newer serving epoch than %d", existing.ServingEpoch)
			}
		}
		for resourceID := range state.Shares {
			existing := state.Shares[resourceID]
			if resourceID != share.ResourceID && (existing.CRID == share.CRID || existing.ConnectorID == share.ConnectorID) {
				return errors.New("local share identity aliases an existing resource")
			}
		}
		state.Shares[share.ResourceID] = share
		return nil
	})
}

// SetDesired advances the authoritative desired state and serving epoch.
func (r *LocalShareRegistry) SetDesired(ctx context.Context, id, desired string, epoch uint64) (*LocalShare, error) {
	var updated LocalShare
	err := r.update(ctx, func(state *localSharesState) error {
		key, share, ok := findLocalShare(state.Shares, id)
		if !ok {
			return os.ErrNotExist
		}
		if desired != desiredStateOn && desired != desiredStateOff {
			return fmt.Errorf("invalid local share desired state %q", desired)
		}
		if epoch < share.ServingEpoch {
			return fmt.Errorf("refuse stale serving epoch %d below local epoch %d", epoch, share.ServingEpoch)
		}
		if epoch == share.ServingEpoch && desired != share.DesiredState {
			return fmt.Errorf("refuse contradictory desired state %q at serving epoch %d", desired, epoch)
		}
		updated = share
		if epoch == share.ServingEpoch {
			return errLocalShareUnchanged
		}
		share.DesiredState = desired
		share.ServingEpoch = epoch
		share.UpdatedAt = time.Now().UTC()
		state.Shares[key] = share
		updated = share
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DisableAtCurrentEpoch records a fail-closed local stop without rotating the
// serving epoch. It is used after either a permanent terminal denial for the
// current session or an authoritative idempotent cloud-off response. It may
// only turn the exact stored epoch from on to off; it never advances an epoch,
// changes identity or target data, or turns sharing on.
func (r *LocalShareRegistry) DisableAtCurrentEpoch(ctx context.Context, id string, epoch uint64) (*LocalShare, error) {
	var updated LocalShare
	err := r.update(ctx, func(state *localSharesState) error {
		key, share, ok := findLocalShare(state.Shares, id)
		if !ok {
			return os.ErrNotExist
		}
		if epoch != share.ServingEpoch {
			return fmt.Errorf("refuse local disable for serving epoch %d while local epoch is %d", epoch, share.ServingEpoch)
		}
		if share.DesiredState != desiredStateOn && share.DesiredState != desiredStateOff {
			return fmt.Errorf("invalid local share desired state %q", share.DesiredState)
		}
		updated = share
		if share.DesiredState == desiredStateOff {
			return errLocalShareUnchanged
		}
		share.DesiredState = desiredStateOff
		share.UpdatedAt = time.Now().UTC()
		state.Shares[key] = share
		updated = share
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// Get resolves one row by public resource ID, CRID, or internal Connector ID.
func (r *LocalShareRegistry) Get(ctx context.Context, id string) (*LocalShare, error) {
	state, unlock, err := r.loadLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	_, share, ok := findLocalShare(state.Shares, id)
	if !ok {
		return nil, os.ErrNotExist
	}
	return &share, nil
}

// List returns one locked point-in-time snapshot of every local share. The
// item order is unspecified because callers key rows by resource identity.
func (r *LocalShareRegistry) List(ctx context.Context) ([]LocalShare, error) {
	state, unlock, err := r.loadLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	items := make([]LocalShare, 0, len(state.Shares))
	for resourceID := range state.Shares {
		items = append(items, state.Shares[resourceID])
	}
	return items, nil
}

// Delete removes one row by public resource ID, CRID, or internal Connector ID.
func (r *LocalShareRegistry) Delete(ctx context.Context, id string) error {
	return r.update(ctx, func(state *localSharesState) error {
		key, _, ok := findLocalShare(state.Shares, id)
		if !ok {
			return errLocalShareUnchanged
		}
		delete(state.Shares, key)
		return nil
	})
}

func (r *LocalShareRegistry) update(ctx context.Context, mutate func(*localSharesState) error) (retErr error) {
	state, unlock, err := r.loadLocked(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	if err := mutate(&state); err != nil {
		if errors.Is(err, errLocalShareUnchanged) {
			return nil
		}
		return err
	}
	return writeLocalShares(r.dir, state)
}

func (r *LocalShareRegistry) loadLocked(ctx context.Context) (localSharesState, func() error, error) {
	if r == nil || r.dir == "" {
		return localSharesState{}, nil, errors.New("local share registry is not open")
	}
	unlock, err := acquireConnectorResourcesLock(ctx, r.dir)
	if err != nil {
		return localSharesState{}, nil, err
	}
	state, err := loadLocalShares(r.dir)
	if err != nil {
		return localSharesState{}, nil, errors.Join(err, unlock())
	}
	return state, unlock, nil
}

func emptyLocalShares() localSharesState {
	return localSharesState{Version: localSharesVersion, Shares: map[string]LocalShare{}}
}

func findLocalShare(shares map[string]LocalShare, id string) (string, LocalShare, bool) {
	id = strings.TrimSpace(id)
	if share, ok := shares[id]; ok {
		return id, share, true
	}
	for key := range shares {
		share := shares[key]
		if share.CRID == id || share.ConnectorID == id {
			return key, share, true
		}
	}
	return "", LocalShare{}, false
}

func loadLocalShares(dir string) (localSharesState, error) {
	path := filepath.Join(dir, LocalSharesFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyLocalShares(), nil
	}
	if err != nil {
		return localSharesState{}, fmt.Errorf("inspect local share registry: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return localSharesState{}, errors.New("local share registry must be a non-symlink regular file")
	}
	if err := validateConnectorResourceFile(path, info); err != nil {
		return localSharesState{}, fmt.Errorf("validate local share registry: %w", err)
	}
	if info.Size() > localSharesMaxBytes {
		return localSharesState{}, fmt.Errorf("local share registry exceeds %d bytes", localSharesMaxBytes)
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return localSharesState{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return localSharesState{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		return localSharesState{}, errors.New("local share registry changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, localSharesMaxBytes+1))
	if err != nil {
		return localSharesState{}, err
	}
	state, err := decodeLocalShares(data)
	if err != nil {
		if errors.Is(err, ErrLocalShareVersionUnsupported) {
			return localSharesState{}, fmt.Errorf("%w at %q; this CLI does not migrate old state: revoke any device key stored with it in the qURL dashboard, then move or remove the complete state directory and run `qurl login` again", err, path)
		}
		return localSharesState{}, fmt.Errorf("decode local share registry: %w", err)
	}
	return state, nil
}

func decodeLocalShares(data []byte) (localSharesState, error) {
	if !utf8.Valid(data) {
		return localSharesState{}, errors.New("local share registry is not valid UTF-8")
	}
	if err := rejectDuplicateResourceFields(data); err != nil {
		return localSharesState{}, err
	}
	if err := rejectUnpairedJSONSurrogates(data); err != nil {
		return localSharesState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state localSharesState
	if err := decoder.Decode(&state); err != nil {
		return localSharesState{}, err
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return localSharesState{}, err
	}
	if err := validateLocalSharesState(state); err != nil {
		return localSharesState{}, err
	}
	return state, nil
}

func writeLocalShares(dir string, state localSharesState) error {
	if err := validateLocalSharesState(state); err != nil {
		return fmt.Errorf("refuse invalid local share registry: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > localSharesMaxBytes {
		return fmt.Errorf("local share registry exceeds %d bytes", localSharesMaxBytes)
	}
	path := filepath.Join(dir, LocalSharesFile)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("local share registry must remain a non-symlink regular file")
		}
		if err := validateConnectorResourceFile(path, info); err != nil {
			return fmt.Errorf("validate local share registry before write: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return replaceConnectorResources(dir, path, data)
}

func validateLocalSharesState(state localSharesState) error {
	if state.Version != localSharesVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrLocalShareVersionUnsupported, state.Version, localSharesVersion)
	}
	if state.Shares == nil {
		return errors.New("local share registry has an unsupported shape")
	}
	if len(state.Shares) > localSharesMaxItems {
		return fmt.Errorf("local share registry is limited to %d entries", localSharesMaxItems)
	}
	if state.OwnerID != "" && !validLocalOwnerID(state.OwnerID) {
		return errors.New("local share registry account owner is invalid")
	}
	if len(state.Shares) > 0 && state.OwnerID == "" {
		return errors.New("local share registry with shares has no account owner")
	}
	crids := map[string]string{}
	connectorIDs := map[string]string{}
	for key := range state.Shares {
		share := state.Shares[key]
		if key != share.ResourceID {
			return fmt.Errorf("local share key %q does not match resource identity", key)
		}
		if err := validateLocalShare(&share); err != nil {
			return fmt.Errorf("local share %q: %w", key, err)
		}
		if owner, ok := crids[share.CRID]; ok {
			return fmt.Errorf("local shares %q and %q have the same CRID", owner, key)
		}
		if owner, ok := connectorIDs[share.ConnectorID]; ok {
			return fmt.Errorf("local shares %q and %q have the same Connector ID", owner, key)
		}
		crids[share.CRID] = key
		connectorIDs[share.ConnectorID] = key
	}
	return nil
}

func validLocalOwnerID(ownerID string) bool {
	if ownerID == "" || len(ownerID) > 256 || strings.TrimSpace(ownerID) != ownerID || !utf8.ValidString(ownerID) {
		return false
	}
	for _, char := range ownerID {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validateLocalShare(share *LocalShare) error {
	if share == nil {
		return errors.New("local share is nil")
	}
	if share.UpdatedAt.IsZero() {
		return errors.New("local share updated_at is required")
	}
	return ValidateLocalShareDefinition(share)
}

// ValidateLocalShareDefinition validates the non-secret, portable definition
// of one local share. Headless bootstrap validates every row with this method
// before it opens or mutates the durable registry; Put supplies UpdatedAt only
// when the complete input set is ready to commit.
func ValidateLocalShareDefinition(share *LocalShare) error {
	if share == nil {
		return errors.New("local share is nil")
	}
	if err := validateLocalShareIdentity(share); err != nil {
		return err
	}
	if share.DesiredState != desiredStateOn && share.DesiredState != desiredStateOff {
		return fmt.Errorf("local share desired state %q is invalid", share.DesiredState)
	}
	if share.DesiredState == desiredStateOn && share.ServingEpoch == 0 {
		return errors.New("local share serving epoch must be positive while desired state is on")
	}
	return validateLocalShareTarget(share)
}

func validateLocalShareIdentity(share *LocalShare) error {
	if err := validateConnectorID(share.ConnectorID); err != nil {
		return err
	}
	der, err := validateResourceID(share.ResourceID)
	if err != nil {
		return err
	}
	matched, err := crid.KeyMatches(share.CRID, der)
	if err != nil || !matched {
		return errors.New("local share CRID does not match its resource identity")
	}
	if !validRoutingID(share.ConnectorRoutingID) || !validKnockResourceID(share.KnockResourceID) {
		return errors.New("local share routing or knock identity is invalid")
	}
	return nil
}

func validateLocalShareTarget(share *LocalShare) error {
	parsed, err := url.Parse(share.TargetURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("local share target must be a plain loopback HTTP origin")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != share.LocalIP || ip == nil || !ip.IsLoopback() {
		return errors.New("local share target host must be the recorded literal loopback IP address")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port != share.LocalPort || port < 1 || port > 65535 {
		return errors.New("local share target port is invalid")
	}
	return nil
}
