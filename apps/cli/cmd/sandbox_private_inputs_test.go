//go:build clisandbox

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	sandboxAPIKeyFileEnv     = "QURL_API_KEY_FILE"
	sandboxCleanupJWTFileEnv = "QURL_CLI_SANDBOX_CLEANUP_JWT_FILE"
	sandboxRunIDEnv          = "QURL_SHARING_RUN_ID"
	sandboxRunAttemptEnv     = "QURL_SHARING_RUN_ATTEMPT"
	sandboxRuntimeEnv        = "QURL_SHARING_RUNTIME"
	sandboxQURLImageIDEnv    = "QURL_SHARING_QURL_IMAGE"
)

var sandboxPositiveDecimal = regexp.MustCompile(`^[1-9]\d{0,19}$`)
var sandboxImmutableImageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type sandboxRunNamespace struct {
	AgentID     string
	ConnectorID string
}

func sandboxNamespace(label string) (sandboxRunNamespace, error) {
	runID := os.Getenv(sandboxRunIDEnv)
	attempt := os.Getenv(sandboxRunAttemptEnv)
	runtimeName := os.Getenv(sandboxRuntimeEnv)
	if !sandboxPositiveDecimal.MatchString(runID) || !sandboxPositiveDecimal.MatchString(attempt) {
		return sandboxRunNamespace{}, errors.New("qURL sharing run ID and attempt must be canonical positive decimals")
	}
	if _, err := strconv.ParseUint(runID, 10, 64); err != nil {
		return sandboxRunNamespace{}, errors.New("qURL sharing run ID exceeds uint64")
	}
	if _, err := strconv.ParseUint(attempt, 10, 64); err != nil {
		return sandboxRunNamespace{}, errors.New("qURL sharing run attempt exceeds uint64")
	}
	runtimeCode := ""
	// TODO(upstream-contract): Keep this exact runtime enum in lockstep with
	// the protected lifecycle orchestrator. The short namespace code is stable.
	switch runtimeName {
	case "host":
		runtimeCode = "h"
	case "hardened_container":
		runtimeCode = "c"
	default:
		return sandboxRunNamespace{}, fmt.Errorf("%s is unsupported; accepted values are host and hardened_container", sandboxRuntimeEnv)
	}
	labelCode := ""
	switch label {
	case "smoke":
		labelCode = "s"
	case "soak":
		labelCode = "k"
	case "crid":
		labelCode = "r"
	case "sibling-a":
		labelCode = "a"
	case "sibling-b":
		labelCode = "b"
	case "failure":
		labelCode = "f"
	default:
		return sandboxRunNamespace{}, errors.New("qURL sharing test label is unsupported")
	}
	agentID := fmt.Sprintf("qurl-share-r%s-a%s-%s%s", runID, attempt, runtimeCode, labelCode)
	if len(agentID) > 64 {
		return sandboxRunNamespace{}, errors.New("qURL sharing agent identity exceeds the platform bound")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"qurl-sharing-sandbox-v1", runID, attempt, runtimeName, label}, "\x00")))
	connectorID := "connector-sandbox-local-publish-" + hex.EncodeToString(digest[:12])
	return sandboxRunNamespace{AgentID: agentID, ConnectorID: connectorID}, nil
}

func TestSandboxNamespaceIsCanonicalAndSeparated(t *testing.T) {
	t.Setenv(sandboxRunIDEnv, "32635672597")
	t.Setenv(sandboxRunAttemptEnv, "2")
	t.Setenv(sandboxRuntimeEnv, "host")
	first, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sandboxNamespace("smoke")
	if err != nil || second != first {
		t.Fatalf("repeat namespace = %+v, %v; want %+v", second, err, first)
	}
	if !strings.HasPrefix(first.AgentID, "qurl-share-r32635672597-a2-") ||
		!strings.HasPrefix(first.ConnectorID, "connector-sandbox-local-publish-") || len(first.AgentID) > 64 {
		t.Fatalf("namespace = %+v", first)
	}
	seen := map[sandboxRunNamespace]bool{first: true}
	for _, tc := range []struct{ runtime, label string }{
		{"hardened_container", "smoke"}, {"host", "soak"}, {"host", "crid"}, {"host", "sibling-a"}, {"host", "sibling-b"}, {"host", "failure"},
	} {
		t.Setenv(sandboxRuntimeEnv, tc.runtime)
		got, gotErr := sandboxNamespace(tc.label)
		if gotErr != nil || seen[got] {
			t.Fatalf("namespace %s/%s = %+v, %v", tc.runtime, tc.label, got, gotErr)
		}
		seen[got] = true
	}
	t.Setenv(sandboxRunIDEnv, "18446744073709551615")
	t.Setenv(sandboxRunAttemptEnv, "18446744073709551615")
	t.Setenv(sandboxRuntimeEnv, "hardened_container")
	maximal, maxErr := sandboxNamespace("soak")
	if maxErr != nil || len(maximal.AgentID) > 64 {
		t.Fatalf("maximal namespace = %+v, %v", maximal, maxErr)
	}
	t.Setenv(sandboxRuntimeEnv, "container")
	if _, legacyErr := sandboxNamespace("smoke"); legacyErr == nil {
		t.Fatal("legacy container runtime identity was accepted")
	}
	for name, value := range map[string]string{
		"zero": "0", "leading-zero": "01", "negative": "-1", "overflow": "18446744073709551616",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(sandboxRunIDEnv, value)
			t.Setenv(sandboxRunAttemptEnv, "1")
			t.Setenv(sandboxRuntimeEnv, "host")
			if _, gotErr := sandboxNamespace("smoke"); gotErr == nil {
				t.Fatalf("run ID %q accepted", value)
			}
		})
	}
}

func TestSandboxRunIdentityBindsOnlyImmutableHardenedImage(t *testing.T) {
	const exactImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	setIdentity := func(t *testing.T, runtimeName, imageID string) {
		t.Helper()
		t.Setenv(sandboxRunIDEnv, "32635672597")
		t.Setenv(sandboxRunAttemptEnv, "2")
		t.Setenv(sandboxRuntimeEnv, runtimeName)
		t.Setenv(sandboxQURLImageIDEnv, imageID)
	}

	t.Run("hardened-container", func(t *testing.T) {
		setIdentity(t, "hardened_container", exactImageID)
		got, err := sandboxRunIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if got[sandboxQURLImageIDEnv] != exactImageID {
			t.Fatalf("hardened image binding = %q, want exact immutable ID", got[sandboxQURLImageIDEnv])
		}
	})

	t.Run("host", func(t *testing.T) {
		setIdentity(t, "host", exactImageID)
		got, err := sandboxRunIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got[sandboxQURLImageIDEnv]; present {
			t.Fatal("host runtime inherited the hardened-container image binding")
		}
	})

	for name, imageID := range map[string]string{
		"missing":     "",
		"mutable-tag": "qurl-sharing-cli:32635672597-2",
		"whitespace":  exactImageID + " ",
		"uppercase":   "sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		t.Run(name, func(t *testing.T) {
			setIdentity(t, "hardened_container", imageID)
			if got, err := sandboxRunIdentity(); err == nil || got != nil {
				t.Fatalf("unsafe hardened image binding returned (%v, %v)", got, err)
			}
		})
	}

	t.Run("unsupported-runtime", func(t *testing.T) {
		setIdentity(t, "container", exactImageID)
		if got, err := sandboxRunIdentity(); err == nil || got != nil {
			t.Fatalf("unsupported runtime returned (%v, %v)", got, err)
		}
	})
}
