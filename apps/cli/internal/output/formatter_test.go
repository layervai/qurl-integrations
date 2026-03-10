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

func TestFormatRelativeTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now, "just now"},
		{"minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"hours ago", now.Add(-3 * time.Hour), "3h ago"},
		{"days ago", now.Add(-2 * 24 * time.Hour), "2d ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRelativeTime(tt.t)
			if got != tt.want {
				t.Errorf("formatRelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTableFormatQURL(t *testing.T) {
	q := &client.QURL{
		ResourceID:  "r_abc123test",
		TargetURL:   "https://example.com",
		Status:      "active",
		Description: "Test QURL",
		QURLSite:    "https://r_abc123test.qurl.site",
		CreatedAt:   time.Now().Add(-1 * time.Hour),
		OneTimeUse:  true,
		MaxSessions: 5,
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatQURL(&buf, q)
	if err != nil {
		t.Fatalf("FormatQURL: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"r_abc123test", "https://example.com", "active", "Test QURL", "yes", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTableFormatCreate(t *testing.T) {
	result := &client.CreateOutput{
		ResourceID: "r_abc123test",
		QURLLink:   "https://qurl.link/at_abc123",
		QURLSite:   "https://r_abc123test.qurl.site",
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatCreate(&buf, result)
	if err != nil {
		t.Fatalf("FormatCreate: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"created", "r_abc123test", "https://qurl.link/at_abc123"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTableFormatList(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	output := &client.ListOutput{
		QURLs: []client.QURL{
			{ResourceID: "r_1", TargetURL: "https://example.com", Status: "active", CreatedAt: time.Now().Add(-1 * time.Hour), ExpiresAt: &future},
			{ResourceID: "r_2", TargetURL: "https://example.com", Status: "expired", CreatedAt: time.Now().Add(-48 * time.Hour)},
		},
		NextCursor: "cursor_abc",
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatList(&buf, output)
	if err != nil {
		t.Fatalf("FormatList: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "r_1") {
		t.Errorf("output missing r_1:\n%s", out)
	}
	if !strings.Contains(out, "r_2") {
		t.Errorf("output missing r_2:\n%s", out)
	}
	if !strings.Contains(out, "cursor_abc") {
		t.Errorf("output missing cursor hint:\n%s", out)
	}
}

func TestTableFormatListEmpty(t *testing.T) {
	var buf bytes.Buffer
	err := NewTableFormatter().FormatList(&buf, &client.ListOutput{})
	if err != nil {
		t.Fatalf("FormatList: %v", err)
	}
	if !strings.Contains(buf.String(), "No QURLs found") {
		t.Errorf("expected empty message, got:\n%s", buf.String())
	}
}

func TestTableFormatListTruncatesLongURL(t *testing.T) {
	output := &client.ListOutput{
		QURLs: []client.QURL{
			{ResourceID: "r_1", TargetURL: "https://example.com/very/long/path/that/exceeds/forty/characters/easily", Status: "active", CreatedAt: time.Now()},
		},
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatList(&buf, output)
	if err != nil {
		t.Fatalf("FormatList: %v", err)
	}
	if !strings.Contains(buf.String(), "…") {
		t.Errorf("expected truncated URL with '…':\n%s", buf.String())
	}
}

func TestTableFormatResolve(t *testing.T) {
	output := &client.ResolveOutput{
		TargetURL:  "https://api.example.com/data",
		ResourceID: "r_abc123test",
		AccessGrant: &client.AccessGrant{
			ExpiresIn: 305,
			SrcIP:     "203.0.113.42",
		},
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatResolve(&buf, output)
	if err != nil {
		t.Fatalf("FormatResolve: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"https://api.example.com/data", "r_abc123test", "305", "203.0.113.42", "granted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTableFormatMint(t *testing.T) {
	output := &client.MintOutput{
		QURLLink: "https://qurl.link/at_newtoken",
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatMint(&buf, output)
	if err != nil {
		t.Fatalf("FormatMint: %v", err)
	}
	if !strings.Contains(buf.String(), "https://qurl.link/at_newtoken") {
		t.Errorf("output missing link:\n%s", buf.String())
	}
}

func TestTableFormatQuota(t *testing.T) {
	output := &client.QuotaOutput{
		Plan:        "growth",
		PeriodStart: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC),
		Usage: &client.UsageInfo{
			ActiveQURLs:  45,
			QURLsCreated: 150,
		},
		RateLimits: &client.RateLimits{
			MaxActiveQURLs: 1000,
		},
	}

	var buf bytes.Buffer
	err := NewTableFormatter().FormatQuota(&buf, output)
	if err != nil {
		t.Fatalf("FormatQuota: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"GROWTH", "45", "1000", "150"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestJSONFormatQURL(t *testing.T) {
	q := &client.QURL{
		ResourceID: "r_abc123test",
		TargetURL:  "https://example.com",
		Status:     "active",
	}

	var buf bytes.Buffer
	err := JSONFormatter{}.FormatQURL(&buf, q)
	if err != nil {
		t.Fatalf("FormatQURL: %v", err)
	}

	var parsed client.QURL
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.ResourceID != "r_abc123test" {
		t.Errorf("got ResourceID %q, want %q", parsed.ResourceID, "r_abc123test")
	}
}

func TestJSONFormatResolve(t *testing.T) {
	output := &client.ResolveOutput{
		TargetURL:  "https://api.example.com/data",
		ResourceID: "r_abc123test",
	}

	var buf bytes.Buffer
	err := JSONFormatter{}.FormatResolve(&buf, output)
	if err != nil {
		t.Fatalf("FormatResolve: %v", err)
	}

	var parsed client.ResolveOutput
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.TargetURL != "https://api.example.com/data" {
		t.Errorf("got TargetURL %q", parsed.TargetURL)
	}
}
