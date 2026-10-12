package output

import (
	"bytes"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestPreviewOutputEscapesTextAndQuietEmpty(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, true)
	value := &qurlapi.Preview{Description: "guide\x1b[2K", Media: []qurlapi.PreviewMedia{{ID: "id", Kind: "image\n", Alt: "alt\x1b"}}, Uploads: []qurlapi.PreviewUpload{{ID: "job", Status: "failed\n"}}}
	if err := p.Preview(value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `guide\x1b[2K`) {
		t.Fatal(out.String())
	}
	out.Reset()
	p = publisherPrinter(&out, &errBuf, FormatText, false, true, false, true)
	if err := p.Preview(&qurlapi.Preview{}); err != nil || out.Len() != 0 {
		t.Fatalf("%q %v", out.String(), err)
	}
}
