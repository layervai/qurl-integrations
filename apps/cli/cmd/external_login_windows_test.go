//go:build windows

package main

import (
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
		// credential provider directly to isolate the token ACL check.
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
