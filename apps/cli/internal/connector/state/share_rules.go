package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/layervai/qurl-go/crid"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

const (
	shareRulesFile = "share_rules.json"
	// shareRulesMaxItems bounds one profile's remembered shares; a row is
	// about 200 bytes, so a full file stays near 2 MiB.
	shareRulesMaxItems = 10000
	shareRulesMaxBytes = 4 << 20
)

// ShareRuleStore is the profile-local record of the link limits each public
// share was created with, per CRID, so later links default to them.
type ShareRuleStore struct{ dir string }

type shareRulesState struct {
	SchemaVersion int                               `json:"schema_version"`
	Shares        map[string]accessrules.LinkLimits `json:"shares"`
}

// OpenShareRules creates nothing; Set makes the safe local directory when it writes.
func OpenShareRules(dir string) (*ShareRuleStore, error) {
	if dir == "" {
		return nil, errors.New("share rules require a profile")
	}
	return &ShareRuleStore{dir: dir}, nil
}

// Get returns the link limits recorded for id, or nil. Without a saved file
// it creates no directory or lock, so plain shares stay read-only here.
func (s *ShareRuleStore) Get(ctx context.Context, id string) (*accessrules.LinkLimits, error) {
	if !s.saved() {
		return nil, nil //nolint:nilnil // nil limits mean none were saved
	}
	var limits *accessrules.LinkLimits
	err := s.update(ctx, func(state *shareRulesState) bool {
		if kept, ok := state.Shares[id]; ok {
			limits = &kept
		}
		return false
	})
	return limits, err
}

// Set records limits for id, or forgets id when limits is nil.
func (s *ShareRuleStore) Set(ctx context.Context, id string, limits *accessrules.LinkLimits) error {
	if crid.Validate(id) != nil || limits != nil && limits.Validate() != nil {
		return errors.New("invalid share link limits")
	}
	if limits == nil && !s.saved() {
		return nil
	}
	if err := EnsureDirMode(s.dir); err != nil {
		return err
	}
	return s.update(ctx, func(state *shareRulesState) bool {
		if limits == nil {
			_, had := state.Shares[id]
			delete(state.Shares, id)
			return had
		}
		state.Shares[id] = *limits
		return true
	})
}

// saved reports whether the file may exist; any other Lstat error is left
// for the locked read to report.
func (s *ShareRuleStore) saved() bool {
	_, err := os.Lstat(filepath.Join(s.dir, shareRulesFile))
	return !errors.Is(err, os.ErrNotExist)
}

func (s *ShareRuleStore) update(ctx context.Context, change func(*shareRulesState) bool) error {
	unlock, err := acquireNamedStateLock(ctx, s.dir, ".share_rules.lock")
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	path := filepath.Join(s.dir, shareRulesFile)
	state := shareRulesState{SchemaVersion: 1, Shares: map[string]accessrules.LinkLimits{}}
	data, err := readOwnerStateFile(path, shareRulesMaxBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return &AccessRulesFileError{Path: path, Err: err, LinkLimits: true}
	case DecodeStrictJSON(data, &state) != nil || !state.valid():
		return &AccessRulesFileError{Path: path, Err: errors.New("invalid share rules"), LinkLimits: true}
	}
	if !change(&state) {
		return nil
	}
	if len(state.Shares) > shareRulesMaxItems {
		return errors.New("too many shares remember link limits on this device")
	}
	if data, err = json.Marshal(state); err != nil {
		return err
	}
	return replaceConnectorResources(s.dir, path, data)
}

func (s *shareRulesState) valid() bool {
	if s.SchemaVersion != 1 || s.Shares == nil || len(s.Shares) > shareRulesMaxItems {
		return false
	}
	for id, limits := range s.Shares {
		if crid.Validate(id) != nil || limits.Validate() != nil {
			return false
		}
	}
	return true
}
