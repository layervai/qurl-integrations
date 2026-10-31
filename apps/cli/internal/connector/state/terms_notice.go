package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const termsNoticeFile = "terms-notice-version"

// TermsNoticeShown checks the version last shown in this private user state.
func TermsNoticeShown(dir, version string) (bool, error) {
	if err := EnsureDirMode(dir); err != nil {
		return false, err
	}
	path := filepath.Join(dir, termsNoticeFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("terms notice state must be a regular non-symlink file")
	}
	if err := validateConnectorResourceFile(path, info); err != nil {
		return false, err
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false, errors.New("terms notice state changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return false, err
	}
	return string(data) == version+"\n", nil
}

// MarkTermsNoticeShown records a displayed notice without touching credentials.
func MarkTermsNoticeShown(dir, version string) error {
	if version == "" || strings.ContainsAny(version, "\r\n") || len(version) > 63 {
		return errors.New("invalid Terms version")
	}
	shown, err := TermsNoticeShown(dir, version)
	if err != nil || shown {
		return err
	}
	return replaceConnectorResources(dir, filepath.Join(dir, termsNoticeFile), []byte(version+"\n"))
}
