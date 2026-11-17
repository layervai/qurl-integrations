//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/windows"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestWindowsExternalLoginRejectsInvalidPathsBeforeNamespace(t *testing.T) {
	for _, tokenPath := range []string{"", "relative-token", `C:\tokens\child\..\token`, ` C:\tokens\token`, `C:\tokens\token `} {
		t.Run(tokenPath, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "fresh-state")
			res := runCLI(t, &runOpts{
				args: []string{"--supervision", "external", "login", "--enrollment-token-file", tokenPath},
				env:  externalLoginEnv(), shareStateDir: stateDir, openNativeRuntime: refuseNativeRuntime(t),
			})
			if res.code != 2 || !strings.Contains(res.stderr.String(), "absolute, clean path") {
				t.Fatalf("exit=%d stderr=%q", res.code, res.stderr.String())
			}
			if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected path touched namespace: %v", err)
			}
		})
	}
}

func TestWindowsExternalLoginRejectsInheritedTokenACL(t *testing.T) {
	srv := apitest.NewServer(t)
	path := filepath.Join(t.TempDir(), "enrollment-token")
	const token = "one-shot-windows-token"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	// Elevated runners may otherwise assign Administrators as the owner.
	// Change only ownership, preserving the inherited DACL under test.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	called := false
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", path},
		env:  externalLoginEnv(), shareStateDir: filepath.Join(t.TempDir(), "state"),
		// The runtime mode is recorded before enrollment. Exercise the real
		// credential provider without claiming the unreleased local-key backend works.
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			called = true
			_, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{})
			return nil, err
		},
	})
	if !called || res.code != 4 || !strings.Contains(res.stderr.String(), "protected private access and exactly one link") {
		t.Fatalf("provider called=%t exit=%d stderr=%q", called, res.code, res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), token) {
		t.Fatal("rejected token was echoed")
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("rejected credential reached the service")
	}
}

// This pins the current connector dependency's unsupported Windows transport.
// Update it to successful enrollment when the Windows local-key release is pinned.
func TestWindowsUnsupportedEnrollmentLeavesRetryableExternalNamespace(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	// A clean absolute missing path proves the lazy token reader is never reached.
	tokenPath := filepath.Join(t.TempDir(), "missing-token")
	env := externalLoginEnv()
	for key, value := range env {
		// The real connector reads process environment, unlike the CLI harness.
		t.Setenv(key, value)
	}
	var marker []byte
	for attempt := 0; attempt < 2; attempt++ {
		res := runCLI(t, &runOpts{
			args: []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
			env:  env, shareStateDir: stateDir,
			// Stub only Hub bootstrap resolution; use the real connector opener.
			openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				return connectorshare.OpenNativeRuntime(ctx, cfg)
			},
		})
		if res.code != 3 || !strings.Contains(res.stderr.String(), "local-key inherited-descriptor transport is unsupported") {
			t.Fatalf("attempt %d: exit=%d stderr=%q", attempt, res.code, res.stderr.String())
		}
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Name() != ".connector_resources.lock" || entries[1].Name() != connectorstate.RuntimeModeFile {
			t.Fatalf("unexpected state after failed enrollment: %v", entries)
		}
		current, err := os.ReadFile(filepath.Join(stateDir, connectorstate.RuntimeModeFile)) // #nosec G304 -- Fixed marker name under this test's private temporary directory.
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			marker = current
		} else if !bytes.Equal(marker, current) {
			t.Fatal("retry changed the supervision policy marker")
		}
		if len(srv.Requests()) != 0 {
			t.Fatal("unsupported enrollment contacted the service")
		}
	}
}
