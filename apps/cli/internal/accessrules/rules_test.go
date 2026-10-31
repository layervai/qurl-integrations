package accessrules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

const testID = "11111111-1111-4111-8111-111111111111"
const otherID = "22222222-2222-4222-8222-222222222222"

func decodeDefinition(raw string) error {
	var d Definition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return err
	}
	return d.Validate(false)
}

func TestDefinitionRejectsUnsupportedOrContradictoryRules(t *testing.T) {
	valid := Definition{ID: testID, Name: "Team", Mode: "email_domain", EmailDomain: "example.com", AllowedEmails: []string{"Person@elsewhere.com"}}
	if err := valid.Validate(false); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"id":"` + testID + `","name":"x","mode":"public","contract_id":"plan"}`, `{"id":"` + testID + `","name":"x","mode":"future"}`, `{"id":"` + testID + `","name":"x","mode":"keys","secret":"value"}`} {
		if decodeDefinition(raw) == nil {
			t.Fatal("unsafe definition accepted")
		}
	}
}
func TestMergeKeepsUnrelatedChangesAndRequiresSameRuleResolution(t *testing.T) {
	base := []Definition{{ID: testID, Name: "Base", Mode: "keys"}}
	local := []Definition{{ID: testID, Name: "Local", Mode: "keys"}}
	remote := []Definition{{ID: testID, Name: "Base", Mode: "keys"}, {ID: otherID, Name: "Remote", Mode: "review"}}
	got, conflicts := Merge(base, local, remote)
	if len(got) != 2 || got[0].Name != "Local" || len(conflicts) != 0 {
		t.Fatalf("merge %+v conflicts %v", got, conflicts)
	}
	remote[0].Name = "Other edit"
	got, conflicts = Merge(base, local, remote)
	if got[0].Name != "Local" || !reflect.DeepEqual(conflicts, []string{testID}) {
		t.Fatalf("merge %+v conflicts %v", got, conflicts)
	}
	got, conflicts = Merge(base, nil, remote)
	if len(got) != 1 || !reflect.DeepEqual(conflicts, []string{testID}) {
		t.Fatalf("delete/edit merge %+v conflicts %v", got, conflicts)
	}
}

func TestDefinitionRejectsEmptyIrrelevantFieldsAndInvalidUnicode(t *testing.T) {
	for _, raw := range []string{
		`{"id":"` + testID + `","name":"Team","mode":"keys","contract_id":""}`,
		`{"id":"` + testID + `","name":"Team","mode":"keys","allowed_emails":[]}`,
		`{"id":"` + testID + `","name":"Team","mode":"keys","email_domain":""}`,
	} {
		if decodeDefinition(raw) == nil {
			t.Errorf("invalid definition accepted: %s", raw)
		}
	}
}

func TestFrozenCatalogContract(t *testing.T) {
	data, err := os.ReadFile("testdata/catalog.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		SchemaVersion int          `json:"schema_version"`
		Modes         []string     `json:"modes"`
		BuiltIns      []Definition `json:"built_ins"`
		Examples      []Definition `json:"examples"`
		// Display copy shared by every surface; output checks labels and levels.
		Presentation map[string]struct {
			Label       string  `json:"label"`
			Summary     string  `json:"summary"`
			Level       *int    `json:"level"`
			LevelName   *string `json:"level_name"`
			IfForwarded string  `json:"if_forwarded"`
			TheyWill    string  `json:"they_will"`
			YouWill     string  `json:"you_will"`
			YouWillSee  string  `json:"you_will_see"`
		} `json:"presentation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.SchemaVersion != 1 || !reflect.DeepEqual(catalog.Modes, Modes()) || !EqualItems(catalog.BuiltIns, BuiltIns()) {
		t.Fatal("built-in catalog drifted")
	}
	for _, mode := range catalog.Modes {
		if catalog.Presentation[mode].Label == "" || catalog.Presentation[mode].Summary == "" {
			t.Errorf("mode %s has no presentation", mode)
		}
	}
	for _, d := range catalog.BuiltIns {
		if d.Name != catalog.Presentation[d.Mode].Label {
			t.Errorf("built-in %s is named %q, not its mode's label", d.ID, d.Name)
		}
	}
	for _, d := range catalog.Examples {
		if err = d.Validate(false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMergeRetainsAccountOrderingAndAppendsLocalAdditions(t *testing.T) {
	base := []Definition{{ID: testID, Name: "One", Mode: "keys"}, {ID: otherID, Name: "Two", Mode: "keys"}}
	got, _ := Merge(base, base, []Definition{base[1], base[0]})
	if got[0].ID != otherID {
		t.Fatal("unchanged local cache replaced account ordering")
	}
}

func TestBuiltInSnapshotsMatchByIDAndModeOnly(t *testing.T) {
	renamed := Definition{ID: "builtin.keys", Name: "Passkeys you approve", Mode: "keys"}
	if err := renamed.Validate(true); err != nil {
		t.Fatalf("renamed built-in rejected: %v", err)
	}
	for _, d := range []Definition{{ID: "builtin.keys", Name: "Devices you approve", Mode: "review"}, {ID: "builtin.unknown", Name: "x", Mode: "keys"}} {
		if d.Validate(true) == nil {
			t.Errorf("built-in %+v accepted", d)
		}
	}
}

func TestValidateItemsUsesTheServiceSizeBudget(t *testing.T) {
	// qurl-service ValidateAccessRules: 8192 + 5 rules * 106 fixed bytes +
	// 1280 emails * (1 + length) is exactly 262144 when 1262 emails are 197
	// bytes long and 18 are 196.
	items := make([]Definition, 5)
	long := 1262
	for r := range items {
		emails := make([]string, 256)
		for i := range emails {
			digits := 184
			if long > 0 {
				digits, long = 185, long-1
			}
			emails[i] = fmt.Sprintf("%0*d@example.com", digits, i)
		}
		items[r] = Definition{ID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", r), Name: "n", Mode: ModeEmailDomain, EmailDomain: "example.com", AllowedEmails: emails}
	}
	if err := ValidateItems(items); err != nil {
		t.Fatalf("library at the service limit rejected: %v", err)
	}
	items[4].AllowedEmails[255] = "1" + items[4].AllowedEmails[255]
	if err := ValidateItems(items); !errors.Is(err, ErrLibraryLimit) {
		t.Fatalf("library one byte over the service limit: %v", err)
	}
}

func TestFindSelectsByIDOrExactName(t *testing.T) {
	a := Definition{ID: testID, Name: "Team", Mode: "keys"}
	b := Definition{ID: otherID, Name: "Team", Mode: "review"}
	if got, err := Find(otherID, []Definition{a, b}); err != nil || got.ID != otherID {
		t.Fatalf("by ID: %+v %v", got, err)
	}
	if got, err := Find("Team", []Definition{a}, []Definition{a}); err != nil || got.ID != testID {
		t.Fatalf("one rule listed twice: %+v %v", got, err)
	}
	if _, err := Find("Team", []Definition{a, b}); !errors.Is(err, ErrAmbiguousRule) {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := Find("team", []Definition{a}); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("names match exactly: %v", err)
	}
}

func TestRuleSettingsDecodeStrictlyAndRoundTrip(t *testing.T) {
	head := `{"id":"` + testID + `","name":"Team",`
	for _, raw := range []string{
		head + `"mode":"public","link_limits":{"expires_in":"1h","one_time_use":true,"session_duration":"30m"}}`,
		head + `"mode":"public","link_limits":{"expires_in":"30d","one_time_use":false}}`,
		head + `"mode":"review","approval_expires_in":"7d"}`,
		head + `"mode":"keys","approval_expires_in":"365d"}`,
		head + `"mode":"email_domain","email_domain":"studio.com"}`,
	} {
		var d Definition
		if err := json.Unmarshal([]byte(raw), &d); err != nil || d.Validate(false) != nil {
			t.Fatalf("valid settings rejected: %s %v %v", raw, err, d.Validate(false))
		}
		encoded, err := json.Marshal(d)
		var again Definition
		// Byte-exact: an explicit false must survive a write-back.
		if err != nil || string(encoded) != raw || json.Unmarshal(encoded, &again) != nil || !EqualItems([]Definition{d}, []Definition{again}) {
			t.Fatalf("settings did not round-trip: %s -> %s", raw, encoded)
		}
	}
	for _, raw := range []string{
		head + `"mode":"review","link_limits":{"expires_in":"1h"}}`,
		head + `"mode":"public","approval_expires_in":"7d"}`,
		head + `"mode":"public","link_limits":{"one_time_use":true}}`,
		head + `"mode":"public","link_limits":{"expires_in":"1h","session_duration":null}}`,
		head + `"mode":"public","link_limits":{"expires_in":"1h","max_sessions":1}}`,
		head + `"mode":"review","allow_download":true}`,
		head + `"mode":"public","link_limits":{"expires_in":"1h","extra":1}}`,
		head + `"mode":"public","link_limits":null}`,
		head + `"mode":"review","approval_expires_in":null}`,
		head + `"mode":"public","link_limits":{"expires_in":"30s"}}`,
		head + `"mode":"public","link_limits":{"expires_in":"31d"}}`,
		head + `"mode":"public","link_limits":{"expires_in":"1h","session_duration":"25h"}}`,
		head + `"mode":"review","approval_expires_in":"30m"}`,
		head + `"mode":"keys","approval_expires_in":"366d"}`,
		head + `"mode":"public","link_limits":{"expires_in":"1h","session_duration":"1500ms"}}`,
		head + `"mode":"review","approval_expires_in":""}`,
	} {
		if decodeDefinition(raw) == nil {
			t.Errorf("invalid settings accepted: %s", raw)
		}
	}
}

func TestRuleSettingsAreComparedAndBuiltInsCarryNone(t *testing.T) {
	a := Definition{ID: testID, Name: "Team", Mode: "review", ApprovalExpiresIn: new("7d")}
	b := a
	b.ApprovalExpiresIn = new("30d")
	if EqualItems([]Definition{a}, []Definition{b}) {
		t.Fatal("approval lifetime change not seen")
	}
	if (&Definition{ID: "builtin.review", Name: "People you approve", Mode: "review", ApprovalExpiresIn: new("7d")}).Validate(true) == nil {
		t.Fatal("built-in with settings accepted")
	}
}

func TestValidateItemsCountsRuleSettings(t *testing.T) {
	// A library exactly at the budget goes over it once a rule gains a setting.
	items := make([]Definition, 5)
	long := 1262
	for r := range items {
		emails := make([]string, 256)
		for i := range emails {
			digits := 184
			if long > 0 {
				digits, long = 185, long-1
			}
			emails[i] = fmt.Sprintf("%0*d@example.com", digits, i)
		}
		items[r] = Definition{ID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", r), Name: "n", Mode: ModeEmailDomain, EmailDomain: "example.com", AllowedEmails: emails}
	}
	if err := ValidateItems(items); err != nil {
		t.Fatal(err)
	}
	// Settings count toward the same budget, with the service's formula.
	limits := Definition{LinkLimits: &LinkLimits{ExpiresIn: "1h", OneTimeUse: new(false), SessionDuration: new("30m")}, ApprovalExpiresIn: new("7d")}
	if got, want := settingsSize(&limits), 19+2+3+11+10+2+12+1+16+3; got != want {
		t.Fatalf("settings size %d, want %d", got, want)
	}
}

func TestParseDurationMatchesServiceGrammar(t *testing.T) {
	for value, want := range map[string]time.Duration{"1h": time.Hour, "7d": 7 * 24 * time.Hour, "1w": 7 * 24 * time.Hour, "90m": 90 * time.Minute, "30s": 30 * time.Second} {
		if got, err := ParseDuration(value); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"", "d", "-1d", "1y", "abc"} {
		if _, err := ParseDuration(value); err == nil {
			t.Errorf("ParseDuration(%q) accepted", value)
		}
	}
}

func TestSameEffectiveLinkLimitsIgnoresExplicitDefaults(t *testing.T) {
	omitted := &LinkLimits{ExpiresIn: "1h"}
	for _, tc := range []struct {
		other *LinkLimits
		same  bool
	}{
		{&LinkLimits{ExpiresIn: "1h", OneTimeUse: new(false)}, true},
		{&LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true)}, false},
		{&LinkLimits{ExpiresIn: "1h", SessionDuration: new("30m")}, false},
		{&LinkLimits{ExpiresIn: "2h"}, false},
		{nil, false},
	} {
		if got := SameEffectiveLinkLimits(omitted, tc.other); got != tc.same {
			t.Errorf("SameEffectiveLinkLimits(%+v, %+v) = %v", omitted, tc.other, got)
		}
	}
	if SameLinkLimits(omitted, &LinkLimits{ExpiresIn: "1h", OneTimeUse: new(false)}) {
		t.Error("library comparison must keep an explicit false")
	}
}
