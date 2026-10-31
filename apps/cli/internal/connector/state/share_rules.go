package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

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

// ShareRule is what a share keeps from the access rule it was created with,
// so later edits to that rule leave the share unchanged.
type ShareRule struct {
	RuleID            string `json:"rule_id"`
	ApprovalExpiresIn string `json:"approval_expires_in,omitempty"`
	AllowDownload     bool   `json:"allow_download,omitempty"`
}

// ApprovalDuration is how long the share's approvals last; false means they never expire.
func (r ShareRule) ApprovalDuration() (time.Duration, bool) {
	if r.ApprovalExpiresIn == "" {
		return 0, false
	}
	return accessrules.ApprovalDuration(&r.ApprovalExpiresIn)
}

type shareRulesState struct {
	SchemaVersion int                  `json:"schema_version"`
	Shares        map[string]ShareRule `json:"shares"`
}

// ShareRuleStore is the profile-local record of rule settings per CRID.
type ShareRuleStore struct{ dir string }

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

// ShareRuleFrom is the snapshot a share keeps from d; false when d carries
// nothing a share must remember.
func ShareRuleFrom(d *accessrules.Definition) (ShareRule, bool) {
	r := ShareRule{RuleID: d.ID, AllowDownload: d.AllowDownload != nil && *d.AllowDownload}
	if d.ApprovalExpiresIn != nil {
		r.ApprovalExpiresIn = *d.ApprovalExpiresIn
	}
	return r, r.ApprovalExpiresIn != "" || r.AllowDownload
}

// Get returns the snapshot recorded for crid.
func (s *ShareRuleStore) Get(ctx context.Context, id string) (ShareRule, bool, error) {
	var rule ShareRule
	var ok bool
	err := s.update(ctx, func(state *shareRulesState) bool {
		rule, ok = state.Shares[id]
		return false
	})
	return rule, ok, err
}

// Set records rule for crid, or forgets crid when rule is nil.
func (s *ShareRuleStore) Set(ctx context.Context, id string, rule *ShareRule) error {
	if crid.Validate(id) != nil || rule != nil && !rule.valid() {
		return errors.New("invalid share rule")
	}
	return s.update(ctx, func(state *shareRulesState) bool {
		if rule == nil {
			_, had := state.Shares[id]
			delete(state.Shares, id)
			return had
		}
		state.Shares[id] = *rule
		return true
	})
}

func (r ShareRule) valid() bool {
	_, approval := r.ApprovalDuration()
	return r.RuleID != "" && len(r.RuleID) <= 64 && (r.ApprovalExpiresIn == "" || approval)
}

func (s *ShareRuleStore) update(ctx context.Context, change func(*shareRulesState) bool) error {
	unlock, err := acquireNamedStateLock(ctx, s.dir, ".share_rules.lock")
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	path := filepath.Join(s.dir, shareRulesFile)
	state := shareRulesState{SchemaVersion: 1, Shares: map[string]ShareRule{}}
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
		return errors.New("too many shares remember access rule settings on this device")
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
	for id, rule := range s.Shares {
		if crid.Validate(id) != nil || !rule.valid() {
			return false
		}
	}
	return true
}
