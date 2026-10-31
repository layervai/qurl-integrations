// Package accessrules defines versionless named admission presets.
package accessrules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"
)

// MaxVersion is the largest interoperable integer revision.
const MaxVersion int64 = 1<<53 - 1

// MaxBytes bounds library and definition input before decoding.
const MaxBytes = 256 << 10

// MaxItems bounds one custom library.
const MaxItems = 100

const fieldEmailDomain = "email_domain"

// Policy modes and synchronization states are shared bridge vocabulary.
const (
	ModePublic      = "public"
	ModeEmailDomain = "email_domain"
	ModeContract    = "contract"
	ModeReview      = "review"
	ModeKeys        = "keys"
	SyncLocal       = "local"
	SyncSynced      = "synced"
	SyncPending     = "pending"
	SyncOffline     = "offline"
	SyncConflict    = "conflict"
)

// Definition holds only future-publication policy, never recipient grants.
type Definition struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Mode          string   `json:"mode"`
	EmailDomain   string   `json:"email_domain,omitempty"`
	AllowedEmails []string `json:"allowed_emails,omitempty"`
	ContractID    string   `json:"contract_id,omitempty"`
	// Share-time settings. Pointers keep an explicit default (false, 0), so
	// a library written back holds exactly what was read.
	// LinkLimits applies to the links a public rule shares; nil shares the
	// resource's permanent address.
	LinkLimits *LinkLimits `json:"link_limits,omitempty"`
	// ApprovalExpiresIn is how long a review or keys approval lasts; nil
	// approvals never expire.
	ApprovalExpiresIn *string `json:"approval_expires_in,omitempty"`
	// AllowDownload lets recipients download shared files. The CLI serves no
	// files, so it only carries the setting.
	AllowDownload *bool `json:"allow_download,omitempty"`
}

// LinkLimits bounds one minted temporary link. Durations use the service's
// expires_in and session_duration grammar; nil one_time_use is false and nil
// max_sessions is no limit.
type LinkLimits struct {
	ExpiresIn       string  `json:"expires_in"`
	OneTimeUse      *bool   `json:"one_time_use,omitempty"`
	MaxSessions     *int    `json:"max_sessions,omitempty"`
	SessionDuration *string `json:"session_duration,omitempty"`
}

// Bounds is an inclusive duration range.
type Bounds struct{ Min, Max time.Duration }

const day = 24 * time.Hour

// TODO(upstream-contract): bounds copied from the frozen access-rule contract
// and CreateQurlRequest; keep identical to qurl-service.
var (
	LinkExpiry     = Bounds{time.Minute, 30 * day}
	SessionLength  = Bounds{time.Second, day}
	ApprovalExpiry = Bounds{time.Hour, 365 * day}
)

// MaxLinkSessions is the largest max_sessions a link accepts.
const MaxLinkSessions = 1000

// Parse reads s in the service duration grammar and reports whether it is in range.
func (b Bounds) Parse(s string) (time.Duration, bool) {
	d, err := ParseDuration(s)
	return d, err == nil && d >= b.Min && d <= b.Max
}

// ParseDuration is Go's duration grammar plus whole days ("7d") and weeks ("1w").
// TODO(upstream-contract): copied from qurl-service internal/domain/duration.go
// ParseDuration; keep identical.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}
	unit, value := s[len(s)-1:], s[:len(s)-1]
	n, err := strconv.Atoi(value)
	if err != nil {
		return time.ParseDuration(s)
	}
	if n < 0 {
		return 0, errors.New("invalid duration: negative value not allowed")
	}
	switch unit {
	case "d":
		return time.Duration(n) * day, nil
	case "w":
		return time.Duration(n) * 7 * day, nil
	default:
		return time.ParseDuration(s)
	}
}

// ApprovalDuration is how long an approval lasts; false means it never expires.
func ApprovalDuration(expiresIn *string) (time.Duration, bool) {
	if expiresIn == nil {
		return 0, false
	}
	return ApprovalExpiry.Parse(*expiresIn)
}

func (d *Definition) validateSettings() error {
	if l := d.LinkLimits; l != nil {
		_, expiry := LinkExpiry.Parse(l.ExpiresIn)
		switch {
		case d.Mode != ModePublic:
			return errors.New("link limits apply only to public rules")
		case !expiry:
			return errors.New("link_limits.expires_in must be from 1m to 30d")
		case l.MaxSessions != nil && (*l.MaxSessions < 0 || *l.MaxSessions > MaxLinkSessions):
			return errors.New("link_limits.max_sessions must be from 0 to 1000")
		case l.SessionDuration != nil && !ValidSessionLength(*l.SessionDuration):
			return errors.New("link_limits.session_duration must be whole seconds from 1s to 24h")
		}
	}
	if d.ApprovalExpiresIn != nil {
		if !ApprovalsApply(d.Mode) {
			return errors.New("approval_expires_in applies only to review and keys rules")
		}
		if _, ok := ApprovalDuration(d.ApprovalExpiresIn); !ok {
			return errors.New("approval_expires_in must be from 1h to 365d")
		}
	}
	return nil
}

// ApprovalsApply reports whether a rule of mode has approvals that can expire.
func ApprovalsApply(mode string) bool { return mode == ModeReview || mode == ModeKeys }

// ValidSessionLength reports whether s is a session_duration the service
// accepts: whole seconds, because that is its wire unit.
func ValidSessionLength(s string) bool {
	d, ok := SessionLength.Parse(s)
	return ok && d%time.Second == 0
}

func (d *Definition) hasSettings() bool {
	return d.LinkLimits != nil || d.ApprovalExpiresIn != nil || d.AllowDownload != nil
}

func samePointer[T comparable](a, b *T) bool {
	return a == b || a != nil && b != nil && *a == *b
}

// sameSettings compares field presence too, since an explicit default round-trips.
func sameSettings(a, b *Definition) bool {
	if (a.LinkLimits == nil) != (b.LinkLimits == nil) {
		return false
	}
	if l, m := a.LinkLimits, b.LinkLimits; l != nil && (l.ExpiresIn != m.ExpiresIn || !samePointer(l.OneTimeUse, m.OneTimeUse) ||
		!samePointer(l.MaxSessions, m.MaxSessions) || !samePointer(l.SessionDuration, m.SessionDuration)) {
		return false
	}
	return samePointer(a.ApprovalExpiresIn, b.ApprovalExpiresIn) && samePointer(a.AllowDownload, b.AllowDownload)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Modes is the exact creation vocabulary this binary can enforce.
func Modes() []string {
	return []string{ModePublic, ModeKeys, "invite_only", "email_capture", ModeEmailDomain, ModeReview, ModeContract}
}

// BuiltIns returns independent immutable ready-made choices.
func BuiltIns() []Definition {
	return []Definition{
		{ID: "builtin.public", Name: "Anyone with the link", Mode: ModePublic},
		{ID: "builtin.invite_only", Name: "People you invite", Mode: "invite_only"},
		{ID: "builtin.review", Name: "People you approve", Mode: "review"},
		{ID: "builtin.email_capture", Name: "Anyone with a verified email", Mode: "email_capture"},
		{ID: "builtin.keys", Name: "Devices you approve", Mode: "keys"},
	}
}

// Policy expands this preset into an independent resource policy snapshot.
func (d *Definition) Policy(version int64) qurl.ConsumerAccessPolicy {
	return qurl.ConsumerAccessPolicy{Mode: d.Mode, Version: version, ContractID: d.ContractID, EmailDomain: d.EmailDomain, AllowedEmails: slices.Clone(d.AllowedEmails)}
}

// Validate rejects unsupported modes and contradictory or malformed definitions.
// A built-in matches by ID and policy only, so its display name may change.
func (d *Definition) Validate(builtin bool) error {
	if builtin {
		if !d.builtIn() {
			return errors.New("unsupported built-in access rule")
		}
		return nil
	}
	if err := d.validateSettings(); err != nil {
		return err
	}
	if !uuidPattern.MatchString(d.ID) || !utf8.ValidString(d.Name) || utf8.RuneCountInString(d.Name) < 1 || utf8.RuneCountInString(d.Name) > 120 || strings.TrimSpace(d.Name) == "" || strings.ContainsFunc(d.Name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
	}) {
		return errors.New("invalid access rule identity or name")
	}
	if d.Mode == ModePublic {
		if d.EmailDomain != "" || len(d.AllowedEmails) > 0 || d.ContractID != "" {
			return errors.New("public access rule cannot contain private settings")
		}
		return nil
	}
	if d.Policy(1).Validate() != nil {
		return errors.New("invalid access rule policy")
	}
	return nil
}

func (d *Definition) builtIn() bool {
	builtIns := BuiltIns()
	for i := range builtIns {
		if b := &builtIns[i]; b.ID == d.ID && b.Policy(1).SameDefinition(d.Policy(1)) && !d.hasSettings() {
			return true
		}
	}
	return false
}

// ValidateItems checks a complete custom library, returning ErrLibraryLimit
// when only the count or size budget is exceeded.
func ValidateItems(items []Definition) error {
	if items == nil {
		return errors.New("access rules require an array")
	}
	seen := map[string]bool{}
	// TODO(upstream-contract): copied from qurl-service
	// internal/repository/access_rules.go ValidateAccessRules; keep identical.
	size := 8192
	for i := range items {
		d := &items[i]
		if d.Validate(false) != nil || seen[d.ID] {
			return errors.New("invalid or duplicate custom access rule")
		}
		seen[d.ID] = true
		size += 7 + len("idnamemode") + len(d.ID) + len(d.Name) + len(d.Mode)
		if d.EmailDomain != "" {
			size += len(fieldEmailDomain) + len(d.EmailDomain)
		}
		if d.ContractID != "" {
			size += len("contract_id") + len(d.ContractID)
		}
		if d.AllowedEmails != nil {
			size += 3 + len("allowed_emails")
			for _, e := range d.AllowedEmails {
				size += 1 + len(e)
			}
		}
		size += settingsSize(d)
	}
	if len(items) > MaxItems || size > MaxBytes {
		return ErrLibraryLimit
	}
	return nil
}

// settingsSize counts field names and values; a boolean is 1 byte.
// TODO(upstream-contract): copied from qurl-service
// internal/repository/access_rules.go accessRuleSettingsSize; keep identical.
func settingsSize(d *Definition) int {
	size := 0
	if d.ApprovalExpiresIn != nil {
		size += len("approval_expires_in") + len(*d.ApprovalExpiresIn)
	}
	if d.AllowDownload != nil {
		size += len("allow_download") + 1
	}
	if l := d.LinkLimits; l != nil {
		size += 3 + len("link_limits") + len("expires_in") + len(l.ExpiresIn)
		if l.OneTimeUse != nil {
			size += len("one_time_use") + 1
		}
		if l.MaxSessions != nil {
			size += len("max_sessions") + len(strconv.Itoa(*l.MaxSessions))
		}
		if l.SessionDuration != nil {
			size += len("session_duration") + len(*l.SessionDuration)
		}
	}
	return size
}

// Find returns the one rule whose ID or exact name is selector.
func Find(selector string, lists ...[]Definition) (Definition, error) {
	var matches []Definition
	for _, list := range lists {
		for i := range list {
			d := &list[i]
			if d.ID == selector {
				return *d, nil
			}
			if d.Name == selector && !slices.ContainsFunc(matches, func(m Definition) bool { return m.ID == d.ID }) {
				matches = append(matches, *d)
			}
		}
	}
	switch len(matches) {
	case 0:
		return Definition{}, ErrRuleNotFound
	case 1:
		return matches[0], nil
	default:
		return Definition{}, ErrAmbiguousRule
	}
}

// NameTaken reports whether another custom rule or a built-in already uses
// d's name, ignoring case and surrounding space.
func NameTaken(items []Definition, d *Definition) bool {
	name := strings.TrimSpace(d.Name)
	return slices.ContainsFunc(append(BuiltIns(), items...), func(other Definition) bool {
		return other.ID != d.ID && strings.EqualFold(strings.TrimSpace(other.Name), name)
	})
}

// Merge preserves local edits while accepting independent remote changes.
// Conflicting edits/deletions retain the local choice until explicit resolution.
func Merge(base, local, remote []Definition) (items []Definition, conflicts []string) {
	index := func(items []Definition) map[string]Definition {
		result := map[string]Definition{}
		for i := range items {
			result[items[i].ID] = items[i]
		}
		return result
	}
	b, l, r := index(base), index(local), index(remote)
	result := make([]Definition, 0, len(local)+len(remote))
	conflicts = []string{}
	ids := []string{}
	for i := range remote {
		d := &remote[i]
		ids = append(ids, d.ID)
	}
	for i := range local {
		d := &local[i]
		if !slices.Contains(ids, d.ID) {
			ids = append(ids, d.ID)
		}
	}
	for i := range base {
		d := &base[i]
		if !slices.Contains(ids, d.ID) {
			ids = append(ids, d.ID)
		}
	}
	for _, id := range ids {
		before, bok := b[id]
		here, lok := l[id]
		there, rok := r[id]
		localChanged := bok != lok || !EqualItems([]Definition{before}, []Definition{here})
		remoteChanged := bok != rok || !EqualItems([]Definition{before}, []Definition{there})
		selected, exists := there, rok
		if localChanged {
			selected, exists = here, lok
			if remoteChanged && (lok != rok || !EqualItems([]Definition{here}, []Definition{there})) {
				conflicts = append(conflicts, id)
			}
		}
		if exists {
			result = append(result, selected)
		}
	}
	return result, conflicts
}

// Local revision and account guards never carry rule contents.
var (
	ErrConflict        = errors.New("access rules changed; reload before retrying")
	ErrRuleConflict    = fmt.Errorf("%w: resolve this rule's sync conflict first", ErrConflict)
	ErrAccountMismatch = errors.New("access rules belong to another account or deployment; use that profile")
	ErrUnlinked        = errors.New("access rules are local until this device is linked to an account")
	ErrRuleNotFound    = errors.New("access rule not found")
	ErrAmbiguousRule   = errors.New("more than one access rule has this name; use its ID")
	ErrDuplicateName   = errors.New("an access rule with this name already exists")
	ErrLibraryLimit    = errors.New("access rules exceed the 100-rule or size limit")
)

// AccountLibrary is the authenticated service snapshot, never printed directly.
// The service's built-ins are ignored: the CLI never uses them.
type AccountLibrary struct {
	AccountID string       `json:"account_id"`
	Version   int64        `json:"version"`
	Items     []Definition `json:"items"`
}

// Remote preserves the library's whole-list CAS contract.
type Remote interface {
	Load(context.Context) (AccountLibrary, error)
	Replace(context.Context, int64, []Definition) (AccountLibrary, error)
}

// Snapshot is the stable CLI/Desktop result; account identity stays internal.
type Snapshot struct {
	SchemaVersion int          `json:"schema_version"`
	LocalVersion  int64        `json:"local_version"`
	Items         []Definition `json:"items"`
	BuiltIns      []Definition `json:"built_ins"`
	Sync          string       `json:"sync"`
	Conflicts     []string     `json:"conflicts"`
}

// EqualItems compares policy definitions in library order.
func EqualItems(a, b []Definition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Name != b[i].Name || !a[i].Policy(1).Equal(b[i].Policy(1)) || !sameSettings(&a[i], &b[i]) {
			return false
		}
	}
	return true
}

// Validate verifies a complete service response without guessing absent fields.
func (l AccountLibrary) Validate() error {
	if strings.TrimSpace(l.AccountID) == "" || l.Version < 0 || l.Version > MaxVersion || ValidateItems(l.Items) != nil {
		return errors.New("invalid account access rules response")
	}
	return nil
}

// UnmarshalJSON preserves field presence so irrelevant empty settings cannot pass.
// It accepts exactly the contract's fields; a write-back therefore never drops one.
func (d *Definition) UnmarshalJSON(data []byte) error {
	type plain Definition
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields["id"] == nil || fields["name"] == nil || fields["mode"] == nil {
		return errors.New("invalid access rule definition")
	}
	var parsed plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&parsed) != nil {
		return errors.New("invalid access rule definition")
	}
	for key, value := range fields {
		switch key {
		case "id", "name", "mode":
		case "contract_id":
			if parsed.Mode != "contract" || bytes.Equal(value, []byte("null")) {
				return errors.New("invalid access rule definition")
			}
		case fieldEmailDomain, "allowed_emails":
			if parsed.Mode != ModeEmailDomain || bytes.Equal(value, []byte("null")) {
				return errors.New("invalid access rule definition")
			}
		case "link_limits":
			// A nested null would read as omitted; Validate checks the mode.
			if !presentWithoutNulls(value, "expires_in") {
				return errors.New("invalid access rule definition")
			}
		case "approval_expires_in", "allow_download":
			if bytes.Equal(value, []byte("null")) {
				return errors.New("invalid access rule definition")
			}
		default:
			return errors.New("invalid access rule definition")
		}
	}
	*d = Definition(parsed)
	return nil
}

// presentWithoutNulls reports whether value is an object holding required and no null member.
func presentWithoutNulls(value json.RawMessage, required string) bool {
	var members map[string]json.RawMessage
	if json.Unmarshal(value, &members) != nil || members == nil || members[required] == nil {
		return false
	}
	for _, member := range members {
		if bytes.Equal(member, []byte("null")) {
			return false
		}
	}
	return true
}

// SameItem includes the absence of a UUID when comparing conflict choices.
func SameItem(a, b []Definition, id string) bool {
	first := slices.IndexFunc(a, func(d Definition) bool { return d.ID == id })
	second := slices.IndexFunc(b, func(d Definition) bool { return d.ID == id })
	if first < 0 || second < 0 {
		return first == second
	}
	return EqualItems(a[first:first+1], b[second:second+1])
}
