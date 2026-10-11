package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestConsumerPolicyReceiptsKeepConfirmedModeVersionAndAddress(t *testing.T) {
	policy := &qurl.ConsumerAccessPolicy{Mode: "invite_only", Version: 3}
	private := true
	published := &qurlapi.Published{CRID: accessCRID, Private: &private, ConsumerAccess: policy, LinkSiteURL: accessAddress}
	resource := &qurlapi.ResourceSummary{CRID: accessCRID, Private: &private, ConsumerAccess: policy}
	cases := []struct {
		name   string
		render func(*Printer) error
		list   bool
	}{
		{"publish", func(p *Printer) error { return p.Publish(published) }, false},
		{"status", func(p *Printer) error { return p.ResourceStatus(resource) }, false},
		{"list", func(p *Printer) error {
			return p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{*resource}})
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if err := tc.render(newTestPrinter(&out, &stderr, FormatJSON, false, false, false)); err != nil {
				t.Fatal(err)
			}
			var receipt map[string]any
			if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			if tc.list {
				receipt = receipt["resources"].([]any)[0].(map[string]any)
			}
			got, ok := receipt["consumer_access"].(map[string]any)
			if !ok || got["mode"] != "invite_only" || got["version"] != float64(3) {
				t.Fatalf("missing confirmed policy: %v", receipt)
			}
			if tc.name == "publish" && receipt["resource_url"] != accessAddress {
				t.Fatal("private policy lost consumer address")
			}
		})
	}
	var out, stderr bytes.Buffer
	if err := newTestPrinter(&out, &stderr, FormatText, false, false, false).Publish(published); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(accessAddress)) {
		t.Fatal("text publish lost consumer address")
	}
}
