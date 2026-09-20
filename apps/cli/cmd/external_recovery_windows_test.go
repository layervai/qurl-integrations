package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func TestExternalRecoveryTokenFileIsUnsupportedOnWindows(t *testing.T) {
	called := false
	result := runCLI(t, &runOpts{
		args: []string{"login", "--recovery-token-file", filepath.Join(t.TempDir(), "token"), "--supervision", "external"},
		env:  externalLoginEnv(), shareStateDir: filepath.Join(t.TempDir(), "absent"),
		openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			called = true
			return nil, errors.New("unexpected native open")
		},
	})
	if result.code != exitcode.Usage || called || !strings.Contains(result.stderr.String(), "--recovery-token-file") || !strings.Contains(result.stderr.String(), "unsupported on this platform") {
		t.Fatalf("Windows token-file recovery: exit=%d opened=%t stderr=%s", result.code, called, result.stderr.String())
	}
}
