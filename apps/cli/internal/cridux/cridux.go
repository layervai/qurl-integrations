// Package cridux is the warn-only UX layer over the SDK's crid package.
//
// The validation hierarchy is the design doc's: the server is the only
// authoritative validator. Locally the CLI may *warn* — a checksum that does
// not match is almost certainly a typo worth telling the user about — but it
// forwards anything ambiguous rather than rejecting it, so a future
// identifier form is never bricked by an old client. Only input that cannot
// be a resource identifier under any version (illegal characters, the
// permanently forbidden version byte) is rejected locally.
//
// Unknown-but-well-formed version bytes are forwarded silently: a false
// "Known" from a newer registry is the server's call, not ours.
package cridux

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/layervai/qurl-go/crid"
)

// Sentinel errors. ErrUnusableID maps to the invalid-input exit code;
// ErrTestIDOnProduction maps to the usage exit code (it is remedied by --yes).
var (
	// ErrUnusableID reports input that cannot be a resource identifier under
	// any current or future form: illegal characters, emptiness, or the
	// permanently forbidden version byte.
	ErrUnusableID = errors.New("cli: input cannot be a resource identifier")
	// ErrTestIDOnProduction reports a test-environment CRID aimed at the
	// production endpoint without --yes.
	ErrTestIDOnProduction = errors.New("cli: test CRID sent to the production endpoint without --yes")
)

// Kind classifies what the CLI locally believes an identifier operand is.
type Kind int

// Identifier kinds. The zero value is KindUnknown: forward silently, hold no
// verification anchor.
const (
	// KindUnknown is a well-formed-enough operand the CLI cannot classify.
	// It is forwarded silently; the server decides.
	KindUnknown Kind = iota
	// KindCRID is a CRID that passed the full local gate.
	KindCRID
	// KindCRIDTypo looks like a CRID with a typo (bad checksum, wrong
	// alphabet characters). It is warned about and still forwarded.
	KindCRIDTypo
	// KindResourceKey is a public-key resource identifier; KeyDER holds the
	// decoded key so the resolve response can be verified against it.
	KindResourceKey
)

// Assessment is the local classification of one identifier operand.
type Assessment struct {
	// Input is the operand exactly as supplied; it is what gets forwarded.
	Input string
	// Kind is the local classification.
	Kind Kind
	// CRID is set when Kind is KindCRID.
	CRID *crid.CRID
	// KeyDER holds the decoded DER SubjectPublicKeyInfo bytes when Kind is
	// KindResourceKey.
	KeyDER []byte
	// Warnings are §17.1-anatomy messages for stderr, in order.
	Warnings []string
}

// The two registered CRID encoded lengths (47-character truncated form and
// 60-character full form) and the boundary above which a value can be a
// public-key resource identifier (~122 characters today).
const (
	cridTruncatedLength  = 47
	cridFullLength       = 60
	minResourceKeyLength = 80
)

// Assess classifies input locally. A non-nil error means the input cannot be
// a resource identifier at all and was rejected before any request; every
// other outcome forwards, with warnings where a typo is likely.
func Assess(input string) (*Assessment, error) {
	if input == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrUnusableID)
	}
	for i := 0; i < len(input); i++ {
		if !isIdentifierByte(input[i]) {
			return nil, fmt.Errorf("%w: character %q at position %d can never appear in one", ErrUnusableID, string(input[i]), i)
		}
	}

	a := &Assessment{Input: input, Kind: KindUnknown}
	if len(input) == cridTruncatedLength || len(input) == cridFullLength {
		return assessCRIDLength(a)
	}
	if len(input) >= minResourceKeyLength {
		if der, err := base64.RawURLEncoding.DecodeString(input); err == nil {
			a.Kind = KindResourceKey
			a.KeyDER = der
			return a, nil
		}
	}
	return a, nil
}

// assessCRIDLength handles input at one of the two registered CRID lengths.
func assessCRIDLength(a *Assessment) (*Assessment, error) {
	c, err := crid.Parse(a.Input)
	if err == nil {
		a.Kind = KindCRID
		a.CRID = c
		return a, nil
	}
	switch {
	case errors.Is(err, crid.ErrForbiddenVersion):
		// Permanently invalid by contract; forwarding cannot help.
		return nil, fmt.Errorf("%w: it is a permanently invalid form", ErrUnusableID)
	case errors.Is(err, crid.ErrChecksum), errors.Is(err, crid.ErrNonCanonical):
		a.Kind = KindCRIDTypo
		a.Warnings = append(a.Warnings, MsgTypo)
	case errors.Is(err, crid.ErrCharset):
		if lowercaseAlnum(a.Input) {
			// CRID-length, lowercase letters and digits, but using digits the
			// CRID alphabet excludes — the classic hand-typed mistake.
			a.Kind = KindCRIDTypo
			a.Warnings = append(a.Warnings, MsgTypo, MsgAlphabetHint)
		}
		// Otherwise (uppercase, '-', '_') it is some other identifier form:
		// forward silently as KindUnknown.
	}
	return a, nil
}

// EnvironmentGuard applies the design doc's environment-mismatch rule for a
// locally parsed CRID: the first character of a CRID encodes its environment
// ('a...' = production version bytes, 'q...' = test), and pointing a test
// CRID at production is refused unless --yes was given. The reverse mismatch
// only warns, and unregistered version bytes are passed through silently.
func EnvironmentGuard(env crid.Environment, productionEndpoint, assumeYes bool) (string, error) {
	switch env {
	case crid.EnvironmentTest:
		if !productionEndpoint {
			return "", nil
		}
		if !assumeYes {
			return "", fmt.Errorf("%w: %s", ErrTestIDOnProduction, MsgTestOnProduction)
		}
		return MsgTestOnProductionAnyway, nil
	case crid.EnvironmentProduction:
		if productionEndpoint {
			return "", nil
		}
		return MsgProductionOnOther, nil
	case crid.EnvironmentUnknown:
		return "", nil
	default:
		return "", nil
	}
}

// isIdentifierByte reports whether b can appear in any resource identifier
// form the platform serves (CRID lowercase base32 or base64url key form).
func isIdentifierByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_':
		return true
	default:
		return false
	}
}

// lowercaseAlnum reports whether s is only lowercase letters and digits.
func lowercaseAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}
