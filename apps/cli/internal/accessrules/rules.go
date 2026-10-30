// Package accessrules defines versionless named admission presets.
package accessrules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"
)

// MaxVersion is the largest interoperable integer revision.
const MaxVersion int64 = 1<<53 - 1

// MaxBytes bounds library and definition input before decoding.
const MaxBytes = 256 << 10

// Policy modes and synchronization states are shared bridge vocabulary.
const (
	ModePublic      = "public"
	ModeEmailDomain = "email_domain"
	ModeContract    = "contract"
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
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Modes is the exact creation vocabulary this binary can enforce.
func Modes() []string {
	return []string{ModePublic, "keys", "invite_only", "email_capture", ModeEmailDomain, "review", "contract"}
}

// BuiltIns returns independent immutable ready-made choices.
func BuiltIns() []Definition {
	return []Definition{
		{ID: "builtin.public", Name: "Anyone with the link", Mode: ModePublic},
		{ID: "builtin.invite_only", Name: "Invite only", Mode: "invite_only"},
		{ID: "builtin.review", Name: "Ask first by email", Mode: "review"},
		{ID: "builtin.email_capture", Name: "Verify an email", Mode: "email_capture"},
		{ID: "builtin.keys", Name: "Approved passkeys and devices", Mode: "keys"},
	}
}

// Policy expands this preset into an independent resource policy snapshot.
func (d *Definition) Policy(version int64) qurl.ConsumerAccessPolicy {
	return qurl.ConsumerAccessPolicy{Mode: d.Mode, Version: version, ContractID: d.ContractID, EmailDomain: d.EmailDomain, AllowedEmails: slices.Clone(d.AllowedEmails)}
}

// Validate rejects unsupported modes and contradictory or malformed definitions.
func (d *Definition) Validate(builtin bool) error {
	if builtin {
		if !slices.ContainsFunc(BuiltIns(), func(b Definition) bool { return reflect.DeepEqual(b, *d) }) {
			return errors.New("unsupported built-in access rule")
		}
		return nil
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

// ValidateItems bounds a complete custom library including storage overhead.
func ValidateItems(items []Definition) error {
	if items == nil || len(items) > 100 {
		return errors.New("access rules require an array of at most 100 custom rules")
	}
	seen := map[string]bool{}
	size := 1024
	for _, d := range items {
		if d.Validate(false) != nil || seen[d.ID] {
			return errors.New("invalid or duplicate custom access rule")
		}
		seen[d.ID] = true
		size += 128 + len(d.ID) + len(d.Name) + len(d.Mode) + len(d.EmailDomain) + len(d.ContractID)
		for _, e := range d.AllowedEmails {
			size += 1 + len(e)
		}
	}
	if size > MaxBytes {
		return errors.New("access rule library is too large")
	}
	return nil
}

// DecodeDefinition accepts one exact definition, including a frozen built-in snapshot.
func DecodeDefinition(data []byte) (Definition, error) {
	var d Definition
	if StrictDecode(data, &d) != nil || d.Validate(strings.HasPrefix(d.ID, "builtin.")) != nil {
		return Definition{}, errors.New("invalid access rule definition")
	}
	return d, nil
}

// StrictDecode rejects duplicate/unknown fields, trailing JSON and invalid UTF-8.
func StrictDecode(data []byte, target any) error {
	if len(data) > MaxBytes*4 || !utf8.Valid(data) || !validEscapedUnicode(data) {
		return errors.New("invalid access rules JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueValue(decoder, 0); err != nil {
		return errors.New("invalid access rules JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid access rules JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return errors.New("invalid access rules JSON")
	}
	return nil
}
func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 12 {
		return errors.New("JSON is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON field")
			}
			seen[name] = true
		}
		if err := uniqueValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// Merge preserves local edits while accepting independent remote changes.
// Conflicting edits/deletions retain the local choice until explicit resolution.
func Merge(base, local, remote []Definition) (items []Definition, conflicts []string) {
	index := func(items []Definition) map[string]Definition {
		result := map[string]Definition{}
		for _, d := range items {
			result[d.ID] = d
		}
		return result
	}
	b, l, r := index(base), index(local), index(remote)
	result := make([]Definition, 0, len(local)+len(remote))
	conflicts = []string{}
	ids := []string{}
	for _, d := range remote {
		ids = append(ids, d.ID)
	}
	for _, d := range local {
		if !slices.Contains(ids, d.ID) {
			ids = append(ids, d.ID)
		}
	}
	for _, d := range base {
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
	ErrAccountMismatch = errors.New("access rules belong to another account or deployment; use that profile")
	ErrUnlinked        = errors.New("access rules are local until this device is linked to an account")
)

// AccountLibrary is the authenticated service snapshot, never printed directly.
type AccountLibrary struct {
	AccountID string       `json:"account_id"`
	Version   int64        `json:"version"`
	Items     []Definition `json:"items"`
	BuiltIns  []Definition `json:"built_ins"`
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
		if a[i].ID != b[i].ID || a[i].Name != b[i].Name || !a[i].Policy(1).Equal(b[i].Policy(1)) {
			return false
		}
	}
	return true
}

// Validate verifies a complete service response without guessing absent fields.
func (l AccountLibrary) Validate() error {
	if strings.TrimSpace(l.AccountID) == "" || l.Version < 0 || l.Version > MaxVersion || ValidateItems(l.Items) != nil || !EqualItems(l.BuiltIns, BuiltIns()) {
		return errors.New("invalid account access rules response")
	}
	return nil
}

// UnmarshalJSON preserves field presence so irrelevant empty settings cannot pass.
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
		case ModeEmailDomain, "allowed_emails":
			if parsed.Mode != ModeEmailDomain || bytes.Equal(value, []byte("null")) {
				return errors.New("invalid access rule definition")
			}
		default:
			return errors.New("invalid access rule definition")
		}
	}
	*d = Definition(parsed)
	return nil
}
func validEscapedUnicode(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' {
				continue
			}
			i++
			if i >= len(data) || data[i] != 'u' {
				continue
			}
			var ok bool
			i, ok = unicodeEscapeEnd(data, i)
			if !ok {
				return false
			}
		}
	}
	return true
}
func unicodeEscapeEnd(data []byte, marker int) (int, bool) {
	if marker+4 >= len(data) {
		return marker, false
	}
	value, err := strconv.ParseUint(string(data[marker+1:marker+5]), 16, 16)
	if err != nil || value >= 0xdc00 && value <= 0xdfff {
		return marker, false
	}
	last := marker + 4
	if value < 0xd800 || value > 0xdbff {
		return last, true
	}
	if last+6 >= len(data) || data[last+1] != '\\' || data[last+2] != 'u' {
		return marker, false
	}
	low, err := strconv.ParseUint(string(data[last+3:last+7]), 16, 16)
	return last + 6, err == nil && low >= 0xdc00 && low <= 0xdfff
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
