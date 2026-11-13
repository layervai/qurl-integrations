package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestPreviewOutputEscapesTextAndQuietEmpty(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, true)
	value := &qurlapi.Preview{Name: "app\x1b[2K", Description: "guide\x1b[2K", Media: []qurlapi.PreviewMedia{{ID: "id", Kind: "image\n", Alt: "alt\x1b"}}, Uploads: []qurlapi.PreviewUpload{{ID: "job", Status: "failed\n"}}}
	if err := p.Preview(value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `guide\x1b[2K`) || !strings.Contains(out.String(), `Name: "app\x1b[2K"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := p.Preview(&qurlapi.Preview{Description: "only"}); err != nil || strings.Contains(out.String(), "Name:") {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
	out.Reset()
	p = publisherPrinter(&out, &errBuf, FormatText, false, true, false, true)
	if err := p.Preview(&qurlapi.Preview{}); err != nil || out.Len() != 0 {
		t.Fatalf("%q %v", out.String(), err)
	}
}

func TestPreviewJSONEscapesWithoutChangingValues(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatJSON, false, false, false, true)
	text := "text\u202e\u009b\x1b"
	value := &qurlapi.Preview{Revision: 9, Name: text, Description: text, Media: []qurlapi.PreviewMedia{{ID: text, Kind: text, Alt: text, Width: 2, Height: 3}}, Uploads: []qurlapi.PreviewUpload{{ID: text, Status: text, Alt: text, ExpiresAt: 12}}}
	if err := p.Preview(value); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"\u202e", "\u009b", "\x1b"} {
		if strings.Contains(out.String(), raw) {
			t.Fatalf("raw terminal control: %q", out.String())
		}
	}
	var decoded qurlapi.Preview
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != text || decoded.Description != text || decoded.Media[0] != value.Media[0] || decoded.Uploads[0] != value.Uploads[0] || decoded.Revision != 9 {
		t.Fatal(decoded)
	}
}
func TestPreviewTextUsesASCIIAndBoundsText(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, true)
	p.ascii = true
	if err := p.Preview(&qurlapi.Preview{Description: strings.Repeat("é", 600)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "é") || strings.Count(out.String(), `\u00e9`) != 500 {
		t.Fatal(out.String())
	}
}
