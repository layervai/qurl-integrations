package state

import (
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

func TestDecodeStrictJSONRejectsAmbiguousSpellings(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var d accessrules.Definition
	if err := DecodeStrictJSON([]byte(`{"id":"`+id+`","name":"\ud83d\ude00","mode":"keys"}`), &d); err != nil || d.Name != "\U0001F600" {
		t.Fatalf("paired surrogate: %q %v", d.Name, err)
	}
	for _, raw := range []string{
		`{"id":"` + id + `","name":"\ud800","mode":"keys"}`,
		`{"id":"` + id + `","name":"Team","mode":"keys","mode":"public"}`,
		`{"id":"` + id + `","name":"Team","mode":"keys"} {}`,
		`{"id":"` + id + `","name":"Team","mode":"keys","extra":1}`,
		"{\"id\":\"" + id + "\",\"name\":\"\xff\",\"mode\":\"keys\"}",
	} {
		if DecodeStrictJSON([]byte(raw), &d) == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
