package slacklog

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSlackOwnerLogIDKeepsStructuredSlackOwnerIDs(t *testing.T) {
	t.Parallel()

	cases := []string{"T123ABCDEF", "E123ABCDEF", "U123ABCDEF", "W123ABCDEF", "C123ABCDEF", strings.Repeat("A", maxIDBytes), ""}
	for _, tc := range cases {
		if got := ID(tc); got != tc {
			t.Fatalf("ID(%q) = %q, want %q", tc, got, tc)
		}
	}
}

func TestSlackOwnerLogIDRejectsMalformedOwnerIDs(t *testing.T) {
	t.Parallel()

	cases := []string{"T123\nINJECT", "T123-ABC", "T123_ABC", "T123abc", strings.Repeat("T", maxIDBytes+1)}
	for _, tc := range cases {
		want := fmt.Sprintf(invalidIDFormat, len(tc))
		if got := ID(tc); got != want {
			t.Fatalf("ID(%q) = %q, want %q", tc, got, want)
		}
	}
}

func TestIDsDoesNotChangeInput(t *testing.T) {
	input := []string{"U123ABC", "E123ABC", "bad\nvalue"}
	got := IDs(input)
	if !reflect.DeepEqual(got, []string{"U123ABC", "E123ABC", ID("bad\nvalue")}) {
		t.Fatalf("got %v", got)
	}
	if input[2] != "bad\nvalue" {
		t.Fatal("input changed")
	}
}
