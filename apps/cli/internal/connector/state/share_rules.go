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

// OpenShareRules creates only the safe local directory.
func OpenShareRules(dir string) (*ShareRuleStore, error) {
	if dir == "" {
		return nil, errors.New("share rules require a profile")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, err
	}
	return &ShareRuleStore{dir: dir}, nil
}

// Get returns the link limits recorded for id, or nil.
func (s *ShareRuleStore) Get(ctx context.Context, id string) (*accessrules.LinkLimits, error) {
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
	if crid.Validate(id) != nil || limits != nil && !validLinkLimits(*limits) {
		return errors.New("invalid share link limits")
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

// validLinkLimits applies a public rule's own validation to kept limits.
func validLinkLimits(limits accessrules.LinkLimits) bool {
	rule := accessrules.Definition{ID: "00000000-0000-4000-8000-000000000000", Name: "kept", Mode: accessrules.ModePublic, LinkLimits: &limits}
	return rule.Validate(false) == nil
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
		return &AccessRulesFileError{Path: path, Err: err}
	case DecodeStrictJSON(data, &state) != nil || !state.valid():
		return &AccessRulesFileError{Path: path, Err: errors.New("invalid share rules")}
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
		if crid.Validate(id) != nil || !validLinkLimits(limits) {
			return false
		}
	}
	return true
}
