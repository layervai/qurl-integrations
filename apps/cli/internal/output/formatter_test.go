package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"days", 3 * 24 * time.Hour, "3d"},
		{"hours", 5 * time.Hour, "5h"},
		{"minutes", 45 * time.Minute, "45m"},
		{"seconds", 30 * time.Second, "30s"},
		{"just over a day", 25 * time.Hour, "1d"},
		{"just over an hour", 61 * time.Minute, "1h"},
		{"one minute", time.Minute, "1m"},
		{"sub-minute", 59 * time.Second, "59s"},
		{"one second", time.Second, "1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestTableFormatQURL(t *testing.T) {
	q := &client.QURL{
		ID:         "qurl_123",
		LinkURL:    "https://qurl.link/abc",
		TargetURL:  "https://example.com",
		Title:      "Test",
		ClickCount: 5,
	}

	var buf bytes.Buffer
	err := TableFormatter{}.FormatQURL(&buf, q)
	if err != nil {
		t.Fatalf("FormatQURL: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"qurl_123", "https://qurl.link/abc", "https://example.com", "Test", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTableFormatList(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	output := &client.ListOutput{
		QURLs: []client.QURL{
			{ID: "qurl_1", TargetURL: "https://example.com", ClickCount: 3, ExpiresAt: &future},
			{ID: "qurl_2", TargetURL: "https://example.com", ClickCount: 0},
		},
		NextCursor: "cursor_abc",
	}

	var buf bytes.Buffer
	err := TableFormatter{}.FormatList(&buf, output)
	if err != nil {
		t.Fatalf("FormatList: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "qurl_1") {
		t.Errorf("output missing qurl_1:\n%s", out)
	}
	if !strings.Contains(out, "qurl_2") {
		t.Errorf("output missing qurl_2:\n%s", out)
	}
	if !strings.Contains(out, "--cursor cursor_abc") {
		t.Errorf("output missing cursor hint:\n%s", out)
	}
	if !strings.Contains(out, "never") {
		t.Errorf("output missing 'never' for no-expiry QURL:\n%s", out)
	}
}

func TestTableFormatListTruncatesLongURL(t *testing.T) {
	output := &client.ListOutput{
		QURLs: []client.QURL{
			{ID: "qurl_1", TargetURL: "https://example.com/very/long/path/that/exceeds/thirty/characters"},
		},
	}

	var buf bytes.Buffer
	err := TableFormatter{}.FormatList(&buf, output)
	if err != nil {
		t.Fatalf("FormatList: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "...") {
		t.Errorf("expected truncated URL with '...':\n%s", out)
	}
}

func TestTableFormatResolve(t *testing.T) {
	output := &client.ResolveOutput{
		TargetURL:  "https://api.example.com/data",
		ResourceID: "r_abc123",
		AccessGrant: &client.AccessGrant{
			ExpiresIn: 305,
			SrcIP:     "203.0.113.42",
		},
	}

	var buf bytes.Buffer
	err := TableFormatter{}.FormatResolve(&buf, output)
	if err != nil {
		t.Fatalf("FormatResolve: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"https://api.example.com/data", "r_abc123", "305", "203.0.113.42"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestJSONFormatQURL(t *testing.T) {
	q := &client.QURL{
		ID:        "qurl_123",
		TargetURL: "https://example.com",
	}

	var buf bytes.Buffer
	err := JSONFormatter{}.FormatQURL(&buf, q)
	if err != nil {
		t.Fatalf("FormatQURL: %v", err)
	}

	var parsed client.QURL
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if parsed.ID != "qurl_123" {
		t.Errorf("got ID %q, want %q", parsed.ID, "qurl_123")
	}
}

func TestJSONFormatResolve(t *testing.T) {
	output := &client.ResolveOutput{
		TargetURL:  "https://api.example.com/data",
		ResourceID: "r_abc123",
	}

	var buf bytes.Buffer
	err := JSONFormatter{}.FormatResolve(&buf, output)
	if err != nil {
		t.Fatalf("FormatResolve: %v", err)
	}

	var parsed client.ResolveOutput
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if parsed.TargetURL != "https://api.example.com/data" {
		t.Errorf("got TargetURL %q, want %q", parsed.TargetURL, "https://api.example.com/data")
	}
}
