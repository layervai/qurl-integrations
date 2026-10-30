package accessrules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
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
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.SchemaVersion != 1 || !reflect.DeepEqual(catalog.Modes, Modes()) || !EqualItems(catalog.BuiltIns, BuiltIns()) {
		t.Fatal("built-in catalog drifted")
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
	for _, d := range []Definition{{ID: "builtin.keys", Name: "Approved passkeys and devices", Mode: "review"}, {ID: "builtin.unknown", Name: "x", Mode: "keys"}} {
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
	if err := ValidateItems(items); err == nil {
		t.Fatalf("library one byte over the service limit: %v", err)
	}
}
