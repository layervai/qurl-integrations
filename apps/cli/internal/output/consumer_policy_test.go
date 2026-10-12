package output

import (
	"bytes"
	"encoding/json"
	"strings"
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

func TestConsumerManagementQuietListsDecisionIDs(t *testing.T) {
	for _, action := range []string{"list_invitations", "list_grants"} {
		var out, stderr bytes.Buffer
		result := &qurlapi.ConsumerManagementResult{Action: action, CRID: accessCRID}
		if action == "list_invitations" {
			result.Invitations = []qurl.ConsumerInvitation{{ID: "decision"}}
		} else {
			result.Grants = []qurl.ConsumerGrant{{ID: "decision"}}
		}
		if err := newTestPrinter(&out, &stderr, FormatText, true, false, false).ConsumerManagement(result); err != nil {
			t.Fatal(err)
		}
		if out.String() != "decision\n" {
			t.Fatalf("quiet list=%q", out.String())
		}
	}
}

func TestConsumerPolicyTextStatusAndPublish(t *testing.T) {
	policy := &qurl.ConsumerAccessPolicy{Mode: "email_capture", Version: 3}
	private := true
	var out, stderr bytes.Buffer
	p := newTestPrinter(&out, &stderr, FormatText, false, false, false)
	if err := p.ResourceStatus(&qurlapi.ResourceSummary{CRID: accessCRID, Private: &private, ConsumerAccess: policy}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "email_capture (version 3)") {
		t.Fatalf("status=%q", out.String())
	}
	out.Reset()
	if err := p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{{CRID: accessCRID, ConsumerAccess: policy}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "email_capture (version 3)") {
		t.Fatalf("list=%q", out.String())
	}
	out.Reset()
	if err := p.Publish(&qurlapi.Published{CRID: accessCRID, Private: &private, ConsumerAccess: policy}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), msgPublishPrivate) || !strings.Contains(out.String(), "publisher's access policy") {
		t.Fatalf("publish=%q", out.String())
	}
}

func TestConsumerManagementJSONEmptyListsAndUnknownExpiry(t *testing.T) {
	for _, action := range []string{"list_invitations", "list_grants"} {
		var out, stderr bytes.Buffer
		if err := newTestPrinter(&out, &stderr, FormatJSON, false, false, false).ConsumerManagement(&qurlapi.ConsumerManagementResult{Action: action, CRID: accessCRID}); err != nil {
			t.Fatal(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		field := strings.TrimPrefix(action, "list_")
		items, ok := receipt[field].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("empty list=%s", out.String())
		}
	}
	var out, stderr bytes.Buffer
	if err := newTestPrinter(&out, &stderr, FormatJSON, false, false, false).ConsumerManagement(&qurlapi.ConsumerManagementResult{Action: "revoke_invitation", CRID: accessCRID, Invitation: &qurl.ConsumerInvitation{ID: "decision", Status: "revoked", Version: 2}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "expires_at") || strings.Contains(out.String(), "0001-") {
		t.Fatalf("invented expiry=%s", out.String())
	}
}
