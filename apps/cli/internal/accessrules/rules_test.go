package accessrules

import (
	"os"
	"reflect"
	"testing"
)

const testID = "11111111-1111-4111-8111-111111111111"
const otherID = "22222222-2222-4222-8222-222222222222"

func TestDefinitionRejectsUnsupportedOrContradictoryRules(t *testing.T) {
	valid := Definition{ID: testID, Name: "Team", Mode: "email_domain", EmailDomain: "example.com", AllowedEmails: []string{"Person@elsewhere.com"}}
	if err := valid.Validate(false); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"id":"` + testID + `","name":"x","mode":"public","contract_id":"plan"}`, `{"id":"` + testID + `","name":"x","mode":"future"}`, `{"id":"` + testID + `","name":"x","mode":"keys","secret":"value"}`} {
		if _, err := DecodeDefinition([]byte(raw)); err == nil {
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
		`{"id":"` + testID + `","name":"\ud800","mode":"keys"}`,
		`{"id":"` + testID + `","name":"Team","mode":"keys","mode":"public"}`,
	} {
		if _, err := DecodeDefinition([]byte(raw)); err == nil {
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
	if err = StrictDecode(data, &catalog); err != nil {
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
