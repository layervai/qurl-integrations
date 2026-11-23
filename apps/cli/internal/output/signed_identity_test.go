package output

import (
	"bytes"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestSignedIdentityOutput(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, mode := range []string{"text", "quiet", "json", "status"} {
			var out, errOut bytes.Buffer
			format := FormatText
			if mode == "json" {
				format = FormatJSON
			}
			printer := New(&Streams{Out: &out, Err: &errOut}, format, mode == "quiet", false, false, nil)
			row := &qurlapi.ResourceSummary{CRID: "test-crid", SignedIdentity: &enabled}
			var err error
			if mode == "status" {
				err = printer.ResourceStatus(row)
			} else {
				err = printer.SignedIdentity(row)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "false"
			if enabled {
				want = "true"
			}
			switch mode {
			case "quiet":
				if out.String() != want+"\n" {
					t.Fatal(out.String())
				}
			case "json":
				if !strings.Contains(out.String(), `"signed_identity": `+want) {
					t.Fatal(out.String())
				}
			case "status":
				if !strings.Contains(out.String(), "Signed identity:") || !strings.Contains(out.String(), want) {
					t.Fatal(out.String())
				}
			default:
				state := "disabled"
				if enabled {
					state = "enabled"
				}
				if !strings.Contains(out.String(), "Signed identity: "+state) {
					t.Fatal(out.String())
				}
			}
		}
	}
}
