package output

import (
	"bytes"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestRetargetReceiptOutput(t *testing.T) {
	for _, tc := range []struct {
		format Format
		quiet  bool
		want   string
	}{
		{FormatJSON, false, "{\n  \"changed\": 2\n}\n"},
		{FormatText, true, "2\n"},
		{FormatText, false, "Updated 2 local share targets.\n"},
	} {
		var out, errOut bytes.Buffer
		p := newTestPrinter(&out, &errOut, tc.format, tc.quiet, false, false)
		if err := p.Retargeted(2); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want || errOut.Len() != 0 {
			t.Fatalf("receipt=%q error=%q", out.String(), errOut.String())
		}
	}
}

func TestRequestEnvelopeKeepsHTMLCharacters(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newTestPrinter(&out, &errOut, FormatJSON, false, false, false)
	if err := p.RequestEnvelope(&qurlapi.RequestResponse{Status: 200, Headers: map[string]string{}, Body: []byte(`"a<b>&c"`)}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\n  \"status\": 200,\n  \"headers\": {},\n  \"body\": \"a<b>&c\"\n}\n" {
		t.Fatalf("envelope=%q", out.String())
	}
}
