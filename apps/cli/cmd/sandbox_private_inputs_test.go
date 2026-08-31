//go:build clisandbox

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	sandboxCleanupIDDirEnv   = "QURL_CLI_CI_CLEANUP_ID_DIR"
)

var sandboxPositiveDecimal = regexp.MustCompile(`^[1-9]\d{0,19}$`)
var sandboxImmutableImageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type sandboxRunNamespace struct {
	AgentID     string
	ConnectorID string
}

func recordSandboxCleanupDeviceKey(t *testing.T, keyID string) {
	t.Helper()
	directory := strings.TrimSpace(os.Getenv(sandboxCleanupIDDirEnv))
	if directory == "" {
		return
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		t.Fatalf("%s must be one exact absolute path", sandboxCleanupIDDirEnv)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is not one existing directory: %v", sandboxCleanupIDDirEnv, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s permissions are not private", sandboxCleanupIDDirEnv)
	}
	if !regexp.MustCompile(`^key_[A-Za-z0-9]{12}$`).MatchString(keyID) {
		t.Fatal("sandbox cleanup device key ID is malformed")
	}
	digest := sha256.Sum256([]byte(keyID))
	path := filepath.Join(directory, "device-key-"+hex.EncodeToString(digest[:]))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The parent directory and hashed leaf are validated above.
	if errors.Is(err, os.ErrExist) {
		raw, readErr := os.ReadFile(path) //nolint:gosec // Exact job-owned cleanup ID file.
		if readErr != nil || string(raw) != keyID {
			t.Fatalf("existing sandbox cleanup device key record is inconsistent: %v", readErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("create sandbox cleanup device key record: %v", err)
	}
	if _, err = file.WriteString(keyID); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("persist sandbox cleanup device key record: write %v, close %v", err, closeErr)
	}
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
	agentID := fmt.Sprintf("qurl-journey-v2-r%s-a%s-%s%s", runID, attempt, runtimeCode, labelCode)
	if len(agentID) > 64 {
		return sandboxRunNamespace{}, errors.New("qURL sharing agent identity exceeds the platform bound")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"qurl-cli-journey-v2", runID, attempt, runtimeName, label}, "\x00")))
	connectorID := "connector-cli-journey-v2-" + hex.EncodeToString(digest[:12])
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
	if !strings.HasPrefix(first.AgentID, "qurl-journey-v2-r32635672597-a2-") ||
		!strings.HasPrefix(first.ConnectorID, "connector-cli-journey-v2-") || len(first.AgentID) > 64 {
		t.Fatalf("namespace = %+v", first)
	}
	if first.ConnectorID != "connector-cli-journey-v2-de8ccdb9ceb99a0657d94412" {
		t.Fatalf("smoke Connector ID = %q; trusted cleanup derivation would drift", first.ConnectorID)
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

func TestRecordSandboxCleanupDeviceKeyIsExactAndIdempotent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directory execute bits are required for one owner.
		t.Fatal(err)
	}
	t.Setenv(sandboxCleanupIDDirEnv, directory)
	const keyID = "key_AbCdEf123456"
	recordSandboxCleanupDeviceKey(t, keyID)
	recordSandboxCleanupDeviceKey(t, keyID)
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cleanup device key records = %d, %v; want one", len(entries), err)
	}
	digest := sha256.Sum256([]byte(keyID))
	wantName := "device-key-" + hex.EncodeToString(digest[:])
	raw, err := os.ReadFile(filepath.Join(directory, wantName)) //nolint:gosec // Exact test-owned path.
	if err != nil || string(raw) != keyID || entries[0].Name() != wantName {
		t.Fatalf("cleanup device key record = %q/%q, %v", entries[0].Name(), raw, err)
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
