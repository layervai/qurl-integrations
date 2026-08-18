package cridux

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/crid"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// TestAssessValidCRIDs drives Assess with CRIDs derived by the
// SDK-cross-checked test deriver, for both registered environments,
// asserting classification, environment, and the q/a first-character rule
// the environment guard's UX is documented against.
func TestAssessValidCRIDs(t *testing.T) {
	key := apitest.GenerateResourceKey(t)

	cases := []struct {
		name      string
		version   byte
		env       crid.Environment
		firstChar byte
	}{
		{"production", apitest.VersionProduction, crid.EnvironmentProduction, 'a'},
		{"test", apitest.VersionTest, crid.EnvironmentTest, 'q'},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := apitest.DeriveCRID(t, key.DER, tc.version)
			if value[0] != tc.firstChar {
				t.Fatalf("first char = %q, want %q (env encoding)", value[0], tc.firstChar)
			}
			a, err := Assess(value)
			if err != nil {
				t.Fatalf("Assess: %v", err)
			}
			if a.Kind != KindCRID || a.CRID == nil {
				t.Fatalf("Kind = %v, want KindCRID", a.Kind)
			}
			if a.CRID.Environment() != tc.env {
				t.Errorf("environment = %v, want %v", a.CRID.Environment(), tc.env)
			}
			if len(a.Warnings) != 0 {
				t.Errorf("valid CRID must not warn: %v", a.Warnings)
			}
		})
	}
}

func TestAssessTypoWarnsAndForwards(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	valid := key.CRID

	// Corrupt one character in the middle: alphabet and length stay valid,
	// the internal consistency check fails.
	corrupted := []byte(valid)
	if corrupted[10] == 'a' {
		corrupted[10] = 'b'
	} else {
		corrupted[10] = 'a'
	}

	a, err := Assess(string(corrupted))
	if err != nil {
		t.Fatalf("typo input must forward, got local reject: %v", err)
	}
	if a.Kind != KindCRIDTypo {
		t.Fatalf("Kind = %v, want KindCRIDTypo", a.Kind)
	}
	if len(a.Warnings) == 0 || !strings.Contains(a.Warnings[0], "typo") {
		t.Errorf("expected the typo warning, got %v", a.Warnings)
	}
	if a.Input != string(corrupted) {
		t.Errorf("forwarded input must be verbatim")
	}
}

func TestAssessExcludedDigitsGetAlphabetHint(t *testing.T) {
	// CRID length, lowercase alphanumeric, but with digits the alphabet
	// excludes — the hand-typed o/l/b/g confusion.
	input := "q018" + strings.Repeat("a", 56)
	a, err := Assess(input)
	if err != nil {
		t.Fatalf("digit-typo input must forward: %v", err)
	}
	if a.Kind != KindCRIDTypo {
		t.Fatalf("Kind = %v, want KindCRIDTypo", a.Kind)
	}
	joined := strings.Join(a.Warnings, "\n")
	if !strings.Contains(joined, "0, 1, 8, and 9 never appear") {
		t.Errorf("expected the alphabet hint, got %v", a.Warnings)
	}
}

func TestAssessResourceKeyForm(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	a, err := Assess(key.ResourceID)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Kind != KindResourceKey {
		t.Fatalf("Kind = %v, want KindResourceKey", a.Kind)
	}
	if !bytes.Equal(a.KeyDER, key.DER) {
		t.Error("KeyDER must round-trip the decoded identifier")
	}
	ok, err := crid.KeyMatches(key.CRID, a.KeyDER)
	if err != nil || !ok {
		t.Errorf("decoded key must derive the key's CRID (ok=%v err=%v)", ok, err)
	}
}

func TestAssessUnknownFormsForwardSilently(t *testing.T) {
	for _, input := range []string{
		"abc",                            // far too short for any known form
		"AAAA" + strings.Repeat("a", 43), // CRID length but uppercase: some other identifier
		strings.Repeat("x", 70),          // between the known forms
	} {
		a, err := Assess(input)
		if err != nil {
			t.Fatalf("input %q must forward, got: %v", input, err)
		}
		if a.Kind != KindUnknown {
			t.Errorf("input %q: Kind = %v, want KindUnknown", input, a.Kind)
		}
		if len(a.Warnings) != 0 {
			t.Errorf("input %q must not warn: %v", input, a.Warnings)
		}
	}
}

func TestAssessLocalRejects(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	forbidden := apitest.DeriveCRID(t, key.DER, 0x00)

	for name, input := range map[string]string{
		"empty":             "",
		"space":             "has a space",
		"punctuation":       "definitely!not@an#id",
		"forbidden version": forbidden,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Assess(input)
			if !errors.Is(err, ErrUnusableID) {
				t.Fatalf("Assess(%q) = %v, want ErrUnusableID", input, err)
			}
		})
	}
}

func TestEnvironmentGuard(t *testing.T) {
	cases := []struct {
		name       string
		env        crid.Environment
		production bool
		yes        bool
		wantErr    bool
		wantWarn   bool
	}{
		{"test on production refused", crid.EnvironmentTest, true, false, true, false},
		{"test on production with yes warns", crid.EnvironmentTest, true, true, false, true},
		{"test on test silent", crid.EnvironmentTest, false, false, false, false},
		{"production on production silent", crid.EnvironmentProduction, true, false, false, false},
		{"production on other warns", crid.EnvironmentProduction, false, false, false, true},
		{"unknown forwarded silently", crid.EnvironmentUnknown, true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warning, err := EnvironmentGuard(tc.env, tc.production, tc.yes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrTestIDOnProduction) {
				t.Errorf("refusal must wrap ErrTestIDOnProduction, got %v", err)
			}
			if (warning != "") != tc.wantWarn {
				t.Errorf("warning = %q, wantWarn = %v", warning, tc.wantWarn)
			}
		})
	}
}
