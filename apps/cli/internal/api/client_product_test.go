package qurlapi

import (
	"strings"
	"testing"
)

func TestValidClientProduct(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{name: "empty"},
		{name: "desktop", value: "qurl-desktop/1.2.3", valid: true},
		{name: "token punctuation", value: "AZaz09!#$%&'*+-.^_`|~/!#$%&'*+-.^_`|~", valid: true},
		{name: "64 bytes", value: strings.Repeat("a", MaxClientProductLen-2) + "/1", valid: true},
		{name: "65 bytes", value: strings.Repeat("a", MaxClientProductLen-1) + "/1"},
		{name: "reserved CLI name", value: "qurl-cli/0.0.1"},
		{name: "reserved CLI name any case", value: "QURL-CLI/0.0.1"},
		{name: "missing slash", value: "desktop"},
		{name: "empty name", value: "/1"},
		{name: "empty version", value: "desktop/"},
		{name: "extra slash", value: "desktop/1/2"},
		{name: "leading space", value: " desktop/1"},
		{name: "trailing space", value: "desktop/1 "},
		{name: "multiple products", value: "desktop/1 other/2"},
		{name: "tab", value: "desktop/1\t"},
		{name: "CR", value: "desktop/1\r"},
		{name: "LF", value: "desktop/1\n"},
		{name: "CRLF injection", value: "desktop/1\r\nInjected: yes"},
		{name: "control", value: "desktop/\x00"},
		{name: "DEL", value: "desktop/\x7f"},
		{name: "unicode name", value: "désktop/1"},
		{name: "unicode version", value: "desktop/é"},
		{name: "comment", value: "desktop/1(comment)"},
		{name: "separator", value: "desktop/1:2"},
		{name: "quoted", value: "desktop/\"1\""},
		{name: "backslash", value: "desktop/1\\2"},
	} {
		if got := ValidClientProduct(tc.value); got != tc.valid {
			t.Errorf("%s: ValidClientProduct(%q) = %t, want %t", tc.name, tc.value, got, tc.valid)
		}
	}
}
