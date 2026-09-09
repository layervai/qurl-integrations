package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const externalEnrollmentTokenMaxBytes = 16 << 10

var statExternalEnrollmentTokenPath = os.Lstat

// ReadExternalEnrollmentTokenFile reads the Desktop one-shot credential from
// one exact private file. It is intentionally stricter than the headless
// projected-secret reader: symlinks and group access are never accepted.
func ReadExternalEnrollmentTokenFile(path string) (string, error) { //nolint:gocyclo // One descriptor-pinning security decision stays together.
	if err := ValidateExternalEnrollmentTokenPath(path); err != nil {
		return "", err
	}
	before, err := os.Lstat(path)
	if err != nil || !validExternalEnrollmentTokenInfo(before) {
		return "", errors.New("external enrollment token must be one owner-readable private regular file")
	}
	if before.Size() <= 0 || before.Size() > externalEnrollmentTokenMaxBytes {
		return "", fmt.Errorf("external enrollment token file must contain between 1 and %d bytes", externalEnrollmentTokenMaxBytes)
	}
	file, err := openExternalEnrollmentTokenNoFollow(path)
	if err != nil {
		return "", errors.New("open external enrollment token without following links")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !validExternalEnrollmentTokenInfo(opened) ||
		opened.Mode() != before.Mode() || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) ||
		validateOpenExternalEnrollmentToken(file, opened) != nil {
		return "", errors.New("external enrollment token changed while opening")
	}
	current, err := statExternalEnrollmentTokenPath(path)
	if err != nil || !os.SameFile(opened, current) || !validExternalEnrollmentTokenInfo(current) {
		return "", errors.New("external enrollment token changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, externalEnrollmentTokenMaxBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > externalEnrollmentTokenMaxBytes || int64(len(raw)) != opened.Size() {
		clear(raw)
		return "", errors.New("read exact bounded external enrollment token bytes")
	}
	defer clear(raw)
	openedAfter, openedErr := file.Stat()
	after, lstatErr := os.Lstat(path)
	if openedErr != nil || lstatErr != nil || !os.SameFile(opened, openedAfter) || !os.SameFile(opened, after) ||
		openedAfter.Mode() != opened.Mode() || openedAfter.Size() != opened.Size() || !openedAfter.ModTime().Equal(opened.ModTime()) ||
		after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		!validExternalEnrollmentTokenInfo(openedAfter) || !validExternalEnrollmentTokenInfo(after) ||
		validateOpenExternalEnrollmentToken(file, openedAfter) != nil {
		return "", errors.New("external enrollment token changed while reading")
	}
	if !utf8.Valid(raw) {
		return "", errors.New("external enrollment token is not valid UTF-8")
	}
	tokenBytes := raw
	if tokenBytes[len(tokenBytes)-1] == '\n' {
		tokenBytes = tokenBytes[:len(tokenBytes)-1]
		if len(tokenBytes) > 0 && tokenBytes[len(tokenBytes)-1] == '\r' {
			tokenBytes = tokenBytes[:len(tokenBytes)-1]
		}
	}
	if len(tokenBytes) == 0 {
		return "", errors.New("external enrollment token must contain one non-empty value with at most one trailing line ending")
	}
	for remaining := tokenBytes; len(remaining) > 0; {
		r, size := utf8.DecodeRune(remaining)
		if unicode.IsSpace(r) {
			return "", errors.New("external enrollment token must contain one non-empty value with at most one trailing line ending")
		}
		remaining = remaining[size:]
	}
	return string(tokenBytes), nil
}

// ValidateExternalEnrollmentTokenPath checks only public path metadata. The
// token file itself remains unopened until native registration invokes the
// enrollment provider.
func ValidateExternalEnrollmentTokenPath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\r\n") ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("external enrollment token file must be an absolute, clean path")
	}
	return nil
}

func validExternalEnrollmentTokenInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	permission := info.Mode().Perm()
	return permission == 0o400 || permission == 0o600
}
