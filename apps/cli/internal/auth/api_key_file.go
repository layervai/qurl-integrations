package auth

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const apiKeyEnvironmentFileMaxBytes = 4 << 10

// The account API-key file is an explicit, short-lived workstation or CI
// bootstrap authority. Unlike the headless daemon's enrollment file, it does
// not need Kubernetes projected-secret symlinks or group-readable fsGroup
// access. Refuse both here so another local principal cannot replace the
// credential between validation and enrollment.
func readAPIKeyEnvironmentFile(path string) (string, error) { //nolint:gocyclo // One exact owner-only file and byte fence stays together.
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\r\n") ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: %s must name one exact absolute private file", ErrInvalidKey, EnvAPIKeyFile)
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || !validAPIKeyEnvironmentFileMode(before.Mode()) ||
		before.Size() <= 1 || before.Size() > apiKeyEnvironmentFileMaxBytes {
		return "", fmt.Errorf("%w: %s is not one bounded owner-private regular file", ErrInvalidKey, EnvAPIKeyFile)
	}
	if err := validateAPIKeyFilePathPlatform(path, before); err != nil {
		return "", fmt.Errorf("%w: %s has unsafe ownership or link authority: %w", ErrInvalidKey, EnvAPIKeyFile, err)
	}
	file, err := openAPIKeyFileNoFollow(path)
	if err != nil {
		return "", fmt.Errorf("%w: cannot open %s without following links", ErrInvalidKey, EnvAPIKeyFile)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !validAPIKeyEnvironmentFileMode(opened.Mode()) ||
		opened.Mode() != before.Mode() || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) ||
		validateOpenAPIKeyFilePlatform(file, opened) != nil {
		return "", fmt.Errorf("%w: %s changed while opening", ErrInvalidKey, EnvAPIKeyFile)
	}
	raw, err := io.ReadAll(io.LimitReader(file, apiKeyEnvironmentFileMaxBytes+1))
	if err != nil || len(raw) <= 1 || len(raw) > apiKeyEnvironmentFileMaxBytes || int64(len(raw)) != opened.Size() {
		return "", fmt.Errorf("%w: cannot read exact bounded %s bytes", ErrInvalidKey, EnvAPIKeyFile)
	}
	openedAfter, openedErr := file.Stat()
	after, lstatErr := os.Lstat(path)
	if openedErr != nil || lstatErr != nil || !os.SameFile(opened, openedAfter) || !os.SameFile(before, after) ||
		openedAfter.Mode() != opened.Mode() || openedAfter.Size() != opened.Size() || !openedAfter.ModTime().Equal(opened.ModTime()) ||
		after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		validateOpenAPIKeyFilePlatform(file, openedAfter) != nil || validateAPIKeyFilePathPlatform(path, after) != nil {
		return "", fmt.Errorf("%w: %s changed while reading", ErrInvalidKey, EnvAPIKeyFile)
	}
	if raw[len(raw)-1] != '\n' {
		return "", fmt.Errorf("%w: %s must contain exact key bytes plus one LF or CRLF", ErrInvalidKey, EnvAPIKeyFile)
	}
	keyBytes := raw[:len(raw)-1]
	if len(keyBytes) > 0 && keyBytes[len(keyBytes)-1] == '\r' {
		keyBytes = keyBytes[:len(keyBytes)-1]
	}
	if bytesContainWhitespaceOrControl(keyBytes) {
		return "", fmt.Errorf("%w: %s must contain exact key bytes plus one LF or CRLF", ErrInvalidKey, EnvAPIKeyFile)
	}
	key := string(keyBytes)
	if err := ValidateKeyShape(key); err != nil {
		return "", err
	}
	return key, nil
}

func bytesContainWhitespaceOrControl(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	for _, value := range raw {
		if value <= 0x20 || value == 0x7f {
			return true
		}
	}
	return false
}
