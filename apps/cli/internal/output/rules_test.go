package output

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

func TestAccessModesMatchCatalog(t *testing.T) {
	raw, err := os.ReadFile("../accessrules/testdata/catalog.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Presentation map[string]struct {
			Label string `json:"label"`
			Level *int   `json:"level"`
		} `json:"presentation"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presentation) != len(accessModes) {
		t.Fatalf("catalog has %d modes, CLI labels %d", len(catalog.Presentation), len(accessModes))
	}
	for mode, want := range catalog.Presentation {
		level := 0
		if want.Level != nil {
			level = *want.Level
		}
		if got := accessModes[mode]; got.label != want.Label || got.level != level {
			t.Errorf("%s: CLI has %q level %d, catalog %q level %d", mode, got.label, got.level, want.Label, level)
		}
	}
}

func TestAccessRulesListBuiltInsByProtection(t *testing.T) {
	var out, stderr bytes.Buffer
	snapshot := accessrules.Snapshot{BuiltIns: accessrules.BuiltIns()}
	if err := newTestPrinter(&out, &stderr, FormatText, false, false, false).AccessRules(&snapshot); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(out.String(), "\n") {
		if id, _, ok := strings.Cut(line, "  "); ok && strings.HasPrefix(id, "builtin.") {
			ids = append(ids, id)
		}
	}
	want := "builtin.public builtin.email_capture builtin.invite_only builtin.review builtin.keys"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("built-in order %q, want %q", got, want)
	}
}
