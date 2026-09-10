package internal

import (
	"strings"
	"testing"
)

func TestSanitizeLogValueEscapesLineBreaks(t *testing.T) {
	got := sanitizeLogValue("team\r\nforged\nentry\rtail")
	if want := `team\r\nforged\nentry\rtail`; got != want {
		t.Fatalf("sanitizeLogValue() = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("sanitizeLogValue() retained a line break: %q", got)
	}
}
