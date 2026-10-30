package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

const accessRulesFile = "access_rules.json"
const accessRulesMaxJournal = accessrules.MaxBytes * 4

// AccessRuleStore owns one profile-local journal; it never opens device state.
type AccessRuleStore struct{ dir, endpoint string }
type accessRuleJournal struct {
	SchemaVersion int                      `json:"schema_version"`
	LocalVersion  int64                    `json:"local_version"`
	Items         []accessrules.Definition `json:"items"`
	Endpoint      string                   `json:"endpoint"`
	AccountID     string                   `json:"account_id"`
	BaseVersion   int64                    `json:"base_version"`
	Base          []accessrules.Definition `json:"base"`
	Sync          string                   `json:"sync"`
	Conflicts     []string                 `json:"conflicts"`
}

// OpenAccessRules creates only the safe local directory, never a device identity.
func OpenAccessRules(dir, endpoint string) (*AccessRuleStore, error) {
	if dir == "" || endpoint == "" {
		return nil, errors.New("access rules require a profile and deployment")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, err
	}
	return &AccessRuleStore{dir: dir, endpoint: strings.TrimRight(endpoint, "/")}, nil
}
func (s *AccessRuleStore) empty() accessRuleJournal {
	return accessRuleJournal{SchemaVersion: 1, Items: []accessrules.Definition{}, Endpoint: s.endpoint, Base: []accessrules.Definition{}, Sync: accessrules.SyncLocal, Conflicts: []string{}}
}
func (j *accessRuleJournal) snapshot() accessrules.Snapshot {
	return accessrules.Snapshot{SchemaVersion: 1, LocalVersion: j.LocalVersion, Items: j.Items, BuiltIns: accessrules.BuiltIns(), Sync: j.Sync, Conflicts: j.Conflicts}
}
func (j *accessRuleJournal) status() {
	switch {
	case len(j.Conflicts) > 0:
		j.Sync = accessrules.SyncConflict
	case j.AccountID == "":
		j.Sync = accessrules.SyncLocal
	case accessrules.EqualItems(j.Items, j.Base):
		j.Sync = accessrules.SyncSynced
	default:
		j.Sync = accessrules.SyncPending
	}
}
func (j *accessRuleJournal) validate() error {
	if j.SchemaVersion != 1 || j.LocalVersion < 0 || j.LocalVersion > accessrules.MaxVersion || j.BaseVersion < 0 || j.BaseVersion > accessrules.MaxVersion || j.Endpoint == "" || accessrules.ValidateItems(j.Items) != nil || accessrules.ValidateItems(j.Base) != nil || j.Conflicts == nil {
		return errors.New("invalid access rules journal")
	}
	if !slices.Contains([]string{accessrules.SyncLocal, accessrules.SyncPending, accessrules.SyncSynced, accessrules.SyncOffline, accessrules.SyncConflict}, j.Sync) {
		return errors.New("invalid access rules journal")
	}
	seen := map[string]bool{}
	for _, id := range j.Conflicts {
		if seen[id] || (!slices.ContainsFunc(j.Items, func(d accessrules.Definition) bool { return d.ID == id }) && !slices.ContainsFunc(j.Base, func(d accessrules.Definition) bool { return d.ID == id })) {
			return errors.New("invalid access rules conflicts")
		}
		seen[id] = true
	}
	return nil
}
func (s *AccessRuleStore) read() (accessRuleJournal, error) {
	return s.readJournal(filepath.Join(s.dir, accessRulesFile))
}

func (s *AccessRuleStore) readJournal(path string) (accessRuleJournal, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s.empty(), nil
	}
	if err != nil {
		return accessRuleJournal{}, err
	}
	if info.Size() > accessRulesMaxJournal || validateConnectorResourceFile(path, info) != nil {
		return accessRuleJournal{}, errors.New("unsafe access rules journal")
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return accessRuleJournal{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return accessRuleJournal{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		return accessRuleJournal{}, errors.New("access rules journal changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, accessRulesMaxJournal+1))
	if err != nil {
		return accessRuleJournal{}, err
	}
	var j accessRuleJournal
	if rejectUnpairedJSONSurrogates(data) != nil || accessrules.StrictDecode(data, &j) != nil || j.validate() != nil {
		return accessRuleJournal{}, errors.New("invalid access rules journal")
	}
	if j.Endpoint != s.endpoint {
		return accessRuleJournal{}, accessrules.ErrAccountMismatch
	}
	return j, nil
}
func (s *AccessRuleStore) update(ctx context.Context, change func(*accessRuleJournal) error) (accessrules.Snapshot, error) {
	unlock, err := acquireNamedStateLock(ctx, s.dir, ".access_rules.lock")
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	defer func() { _ = unlock() }()
	j, err := s.read()
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	if change == nil {
		return j.snapshot(), nil
	}
	before, err := json.Marshal(j)
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	if err := change(&j); err != nil {
		return accessrules.Snapshot{}, err
	}
	after, err := json.Marshal(j)
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	if bytes.Equal(before, after) {
		return j.snapshot(), nil
	}
	if j.LocalVersion >= accessrules.MaxVersion {
		return accessrules.Snapshot{}, accessrules.ErrConflict
	}
	j.LocalVersion++
	if err := j.validate(); err != nil {
		return accessrules.Snapshot{}, err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	if len(data) > accessRulesMaxJournal {
		return accessrules.Snapshot{}, errors.New("access rules journal is too large")
	}
	if err := replaceConnectorResources(s.dir, filepath.Join(s.dir, accessRulesFile), data); err != nil {
		return accessrules.Snapshot{}, err
	}
	return j.snapshot(), nil
}

// Snapshot reads the validated journal without authentication or network.
func (s *AccessRuleStore) Snapshot(ctx context.Context) (accessrules.Snapshot, error) {
	return s.update(ctx, nil)
}

// Save inserts or replaces one UUID only at the expected local revision.
func (s *AccessRuleStore) Save(ctx context.Context, version int64, d *accessrules.Definition) (accessrules.Snapshot, error) {
	if d == nil || d.Validate(false) != nil {
		return accessrules.Snapshot{}, errors.New("invalid custom access rule")
	}
	return s.update(ctx, func(j *accessRuleJournal) error {
		if version != j.LocalVersion {
			return accessrules.ErrConflict
		}
		if slices.Contains(j.Conflicts, d.ID) {
			return accessrules.ErrConflict
		}
		index := slices.IndexFunc(j.Items, func(item accessrules.Definition) bool { return item.ID == d.ID })
		if index < 0 {
			j.Items = append(j.Items, *d)
		} else {
			j.Items[index] = *d
		}
		j.status()
		return nil
	})
}

// Delete removes a preset; no resource is changed.
func (s *AccessRuleStore) Delete(ctx context.Context, version int64, id string) (accessrules.Snapshot, error) {
	return s.update(ctx, func(j *accessRuleJournal) error {
		if version != j.LocalVersion || slices.Contains(j.Conflicts, id) {
			return accessrules.ErrConflict
		}
		index := slices.IndexFunc(j.Items, func(d accessrules.Definition) bool { return d.ID == id })
		if index < 0 {
			return errors.New("custom access rule not found")
		}
		j.Items = slices.Delete(j.Items, index, index+1)
		j.status()
		return nil
	})
}

// Resolve deliberately keeps the local or latest account definition for one conflict.
func (s *AccessRuleStore) Resolve(ctx context.Context, version int64, id, use string) (accessrules.Snapshot, error) {
	if use != accessrules.SyncLocal && use != "account" {
		return accessrules.Snapshot{}, errors.New("choose local or account")
	}
	return s.update(ctx, func(j *accessRuleJournal) error {
		if version != j.LocalVersion || !slices.Contains(j.Conflicts, id) {
			return accessrules.ErrConflict
		}
		if use == "account" {
			index := slices.IndexFunc(j.Items, func(d accessrules.Definition) bool { return d.ID == id })
			remote := slices.IndexFunc(j.Base, func(d accessrules.Definition) bool { return d.ID == id })
			switch {
			case remote < 0:
				j.Items = slices.DeleteFunc(j.Items, func(d accessrules.Definition) bool { return d.ID == id })
			case index < 0:
				j.Items = append(j.Items, j.Base[remote])
			default:
				j.Items[index] = j.Base[remote]
			}
		}
		j.Conflicts = slices.DeleteFunc(j.Conflicts, func(value string) bool { return value == id })
		j.status()
		return nil
	})
}

// Sync merges with the canonical account. A process lock serializes syncs, while
// local edits retain their own short lock and remain possible during HTTP calls.
func (s *AccessRuleStore) Sync(ctx context.Context, remote accessrules.Remote) (accessrules.Snapshot, error) {
	unlock, err := acquireNamedStateLock(ctx, s.dir, ".access_rules.sync.lock")
	if err != nil {
		return accessrules.Snapshot{}, err
	}
	defer func() { _ = unlock() }()
	for range 3 {
		account, err := remote.Load(ctx)
		if err != nil {
			return s.syncFailure(ctx, err)
		}
		if account.Validate() != nil {
			return s.syncFailure(ctx, errors.New("invalid account access rules response"))
		}
		snapshot, candidate, err := s.mergeAccount(ctx, account)
		if err != nil {
			return accessrules.Snapshot{}, err
		}
		if snapshot.Sync == accessrules.SyncConflict || snapshot.Sync == accessrules.SyncSynced {
			return snapshot, nil
		}
		applied, err := remote.Replace(ctx, account.Version, candidate)
		if errors.Is(err, accessrules.ErrConflict) {
			continue
		}
		if err != nil {
			return s.syncFailure(ctx, err)
		}
		if applied.Validate() != nil || applied.AccountID != account.AccountID || applied.Version != account.Version+1 || !accessrules.EqualItems(applied.Items, candidate) {
			return s.syncFailure(ctx, errors.New("invalid account access rules response"))
		}
		return s.update(ctx, func(j *accessRuleJournal) error {
			if j.AccountID != account.AccountID {
				return accessrules.ErrAccountMismatch
			}
			j.Items, j.Conflicts = accessrules.Merge(candidate, j.Items, applied.Items)
			j.Base, j.BaseVersion = applied.Items, applied.Version
			j.status()
			return nil
		})
	}
	return s.syncFailure(ctx, accessrules.ErrConflict)
}
func (s *AccessRuleStore) mergeAccount(ctx context.Context, account accessrules.AccountLibrary) (accessrules.Snapshot, []accessrules.Definition, error) {
	var candidate []accessrules.Definition
	snapshot, err := s.update(ctx, func(j *accessRuleJournal) error {
		if err := s.switchAccount(j, account.AccountID); err != nil {
			return err
		}
		merged, conflicts := accessrules.Merge(j.Base, j.Items, account.Items)
		for _, id := range j.Conflicts {
			if !slices.Contains(conflicts, id) && !accessrules.SameItem(j.Items, account.Items, id) {
				conflicts = append(conflicts, id)
			}
		}
		j.Items, j.Conflicts = merged, conflicts
		j.AccountID, j.BaseVersion, j.Base = account.AccountID, account.Version, account.Items
		j.status()
		candidate = slices.Clone(j.Items)
		return nil
	})
	return snapshot, candidate, err
}
func (s *AccessRuleStore) switchAccount(j *accessRuleJournal, accountID string) error {
	if j.AccountID == "" || j.AccountID == accountID {
		return nil
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := replaceConnectorResources(s.dir, s.accountPath(j.AccountID), data); err != nil {
		return err
	}
	cached, err := s.readJournal(s.accountPath(accountID))
	if err != nil {
		return err
	}
	if cached.AccountID != "" && cached.AccountID != accountID {
		return accessrules.ErrAccountMismatch
	}
	cached.LocalVersion = j.LocalVersion
	*j = cached
	return nil
}
func (s *AccessRuleStore) syncFailure(ctx context.Context, cause error) (accessrules.Snapshot, error) {
	snapshot, err := s.update(ctx, func(j *accessRuleJournal) error {
		j.Sync = accessrules.SyncOffline
		if errors.Is(cause, accessrules.ErrUnlinked) && j.AccountID == "" {
			j.Sync = accessrules.SyncLocal
		}
		return nil
	})
	return snapshot, errors.Join(cause, err)
}

func (s *AccessRuleStore) accountPath(accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(s.dir, fmt.Sprintf(".access_rules.account.%x.json", digest))
}
