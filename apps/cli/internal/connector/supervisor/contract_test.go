package supervisor

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
)

// qrtsKnockTokenLoginContract is the checked-in consumer snapshot of the
// tunnel server's knock-token Login wire contract. The server side vendors
// the same JSON; change needle set, this snapshot, and the server together.
//
//go:embed qrts_knock_token_login_wire_contract.json
var qrtsKnockTokenLoginContractJSON []byte

// qrtsKnockContractEnv optionally points at the producer's vendored copy of
// the same contract (the cross-repo workflow's hook); when present, both
// snapshots must agree exactly before the matcher assertions run.
const qrtsKnockContractEnv = "QRTS_KNOCK_TOKEN_LOGIN_CONTRACT"

type qrtsKnockTokenLoginContract struct {
	SchemaVersion        int      `json:"schema_version"`
	LoginMetasKey        string   `json:"login_metas_key"`
	RejectTag            string   `json:"reject_tag"`
	ClientNeedles        []string `json:"client_needles"`
	LoginRejectWireTexts []string `json:"login_reject_wire_texts"`
}

// frpLoginWrap is the prefix the FRP client stamps onto the server's
// RejectReason before the supervisor sees it — the reason the matcher is
// substring-based. Tests always exercise the wrapped form production sees.
const frpLoginWrap = "login to the server failed: "

// TestQRTSKnockTokenLoginContract binds the contract snapshot to the
// production surfaces that implement it: the IsTokenLoginError needle set and
// the Login metadata key the supervisor stamps. History note: a pre-contract
// needle set once pinned strings the server never emitted, so the classifier
// silently never fired on a real reject — the drift class this fixture turns
// into a test failure.
func TestQRTSKnockTokenLoginContract(t *testing.T) {
	t.Parallel()
	contract := decodeKnockTokenLoginContract(t, "CLI snapshot", qrtsKnockTokenLoginContractJSON)

	if producerPath := os.Getenv(qrtsKnockContractEnv); producerPath != "" {
		producerFixture, err := os.ReadFile(producerPath) //nolint:gosec // G304: CI-provided fixture path for the cross-repo diff.
		if err != nil {
			t.Fatalf("read producer fixture %q: %v", producerPath, err)
		}
		producer := decodeKnockTokenLoginContract(t, "producer fixture", producerFixture)
		if producer.LoginMetasKey != contract.LoginMetasKey ||
			producer.RejectTag != contract.RejectTag ||
			!slices.Equal(producer.ClientNeedles, contract.ClientNeedles) ||
			!slices.Equal(producer.LoginRejectWireTexts, contract.LoginRejectWireTexts) {
			t.Errorf("knock-token Login contract drifted:\n  producer %+v\n  CLI      %+v\nreconcile the producer's vendored copy and this package's snapshot together",
				producer, contract)
		}
	}

	// The Login metadata key the supervisor stamps must be the contract key —
	// the server reads exactly this map entry.
	if frpgen.MetaQURLKnockToken != contract.LoginMetasKey {
		t.Errorf("frpgen.MetaQURLKnockToken = %q, contract login_metas_key = %q; the stamped key and the read key must be identical",
			frpgen.MetaQURLKnockToken, contract.LoginMetasKey)
	}

	// The production needle set is the contract's, verbatim and in order.
	if !slices.Equal(tokenErrorNeedles, contract.ClientNeedles) {
		t.Errorf("tokenErrorNeedles = %q, contract client_needles = %q; update loginerror.go and the JSON together",
			tokenErrorNeedles, contract.ClientNeedles)
	}

	// Every needle must be matched by at least one producer wire text; a
	// needle no wire text contains is dead and the classifier would never
	// fire on a real reject.
	for _, needle := range contract.ClientNeedles {
		matched := false
		for _, wireText := range contract.LoginRejectWireTexts {
			if strings.Contains(strings.ToLower(wireText), needle) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("client needle %q is contained in no login_reject_wire_texts entry — dead needle", needle)
		}
	}

	for _, wireText := range contract.LoginRejectWireTexts {
		t.Run(wireText, func(t *testing.T) {
			t.Parallel()
			wrapped := errors.New(frpLoginWrap + wireText)
			if !IsTokenLoginError(wrapped) {
				t.Errorf("IsTokenLoginError(%q) = false, want true", wrapped)
			}
			// Substring semantics are the compatibility contract: case
			// changes, extra wrapping, and additive detail suffixes must all
			// still classify, so the server can add a new tagged variant
			// without breaking deployed clients.
			for name, variant := range map[string]string{
				"uppercased":    strings.ToUpper(frpLoginWrap + wireText),
				"extra_wrap":    "work conn: " + frpLoginWrap + wireText,
				"detail_suffix": frpLoginWrap + wireText + " (trace abc123)",
			} {
				if !IsTokenLoginError(errors.New(variant)) {
					t.Errorf("IsTokenLoginError(%s %q) = false, want true (substring matcher must tolerate this)", name, variant)
				}
			}
		})
	}

	// Negative fixtures: strings adjacent to the contract surface that must
	// NOT classify, each documenting a boundary of the narrow-needle
	// rationale in loginerror.go.
	negatives := []string{
		frpLoginWrap + "connection refused",
		frpLoginWrap + "token expired",                             // generic token prose without the server-owned tag
		frpLoginWrap + "knock token retry pending",                 // transient-state prose, not a reject tag
		frpLoginWrap + "owner_missing: connector identity missing", // identity reject, distinct class
		frpLoginWrap + "knock-invalid: knock token expired",        // hyphenated near-miss of the tag
		"i/o timeout",
	}
	for _, msg := range negatives {
		if IsTokenLoginError(errors.New(msg)) {
			t.Errorf("IsTokenLoginError(%q) = true, want false (outside the contract surface)", msg)
		}
	}
	if IsTokenLoginError(nil) {
		t.Error("IsTokenLoginError(nil) = true, want false")
	}
}

// decodeKnockTokenLoginContract decodes one snapshot strictly: unknown
// fields, schema bumps, malformed needles, and untagged wire texts all fail
// so a drifted artifact cannot pass as nominally compatible metadata.
func decodeKnockTokenLoginContract(t *testing.T, source string, fixture []byte) qrtsKnockTokenLoginContract {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(fixture))
	decoder.DisallowUnknownFields()
	var contract qrtsKnockTokenLoginContract
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("decode %s: %v", source, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s: expected one JSON value, got trailing content: %v", source, err)
	}
	if contract.SchemaVersion != 1 {
		t.Fatalf("%s schema_version = %d, want 1", source, contract.SchemaVersion)
	}
	for name, value := range map[string]string{
		"login_metas_key": contract.LoginMetasKey,
		"reject_tag":      contract.RejectTag,
	} {
		if value == "" || value != strings.TrimSpace(value) || value != strings.ToLower(value) {
			t.Fatalf("%s %s = %q, want non-empty lowercase with no padding", source, name, value)
		}
		if strings.ContainsAny(value, " \t") {
			t.Fatalf("%s %s = %q, want no internal whitespace", source, name, value)
		}
	}
	if len(contract.ClientNeedles) == 0 {
		t.Fatalf("%s has no client_needles", source)
	}
	seenNeedles := make(map[string]struct{}, len(contract.ClientNeedles))
	for i, needle := range contract.ClientNeedles {
		// IsTokenLoginError lowercases the message before scanning, so a
		// needle with an uppercase byte could never match anything.
		if needle == "" || needle != strings.TrimSpace(needle) || needle != strings.ToLower(needle) {
			t.Fatalf("%s client_needles[%d] = %q, want non-empty lowercase with no padding", source, i, needle)
		}
		if _, duplicate := seenNeedles[needle]; duplicate {
			t.Fatalf("%s repeats client needle %q", source, needle)
		}
		seenNeedles[needle] = struct{}{}
	}
	if len(contract.LoginRejectWireTexts) == 0 {
		t.Fatalf("%s has no login_reject_wire_texts", source)
	}
	seenWireTexts := make(map[string]struct{}, len(contract.LoginRejectWireTexts))
	for i, wireText := range contract.LoginRejectWireTexts {
		if wireText == "" || wireText != strings.TrimSpace(wireText) {
			t.Fatalf("%s login_reject_wire_texts[%d] = %q, want non-empty with no padding", source, i, wireText)
		}
		if !strings.HasPrefix(wireText, contract.RejectTag+": ") {
			t.Fatalf("%s login_reject_wire_texts[%d] = %q, want exact reject_tag %q followed by colon-space",
				source, i, wireText, contract.RejectTag)
		}
		if strings.TrimSpace(strings.TrimPrefix(wireText, contract.RejectTag+": ")) == "" {
			t.Fatalf("%s login_reject_wire_texts[%d] = %q, want a non-empty detail after the tag", source, i, wireText)
		}
		if _, duplicate := seenWireTexts[wireText]; duplicate {
			t.Fatalf("%s repeats wire text %q", source, wireText)
		}
		seenWireTexts[wireText] = struct{}{}
	}
	return contract
}
