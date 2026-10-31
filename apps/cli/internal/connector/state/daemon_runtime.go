package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const daemonRuntimeFile = "daemon-runtime-dir.json"
const maxDaemonRuntimeBytes = 4096

// ReadDaemonRuntimeDir reads the last daemon's socket directory without creating
// or repairing state. A missing marker leaves runtime selection unchanged.
func ReadDaemonRuntimeDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == filepath.Dir(filepath.Clean(dir)) {
		return "", errors.New("daemon state directory must be absolute and below the filesystem root")
	}
	path := filepath.Join(dir, daemonRuntimeFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDaemonRuntimeBytes {
		return "", errors.New("daemon runtime marker must be a bounded regular file")
	}
	if err := validateConnectorResourceFile(path, info); err != nil {
		return "", err
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		return "", errors.New("daemon runtime marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDaemonRuntimeBytes+1))
	if err != nil {
		return "", err
	}
	var runtimeDir string
	if len(data) > maxDaemonRuntimeBytes {
		return "", errors.New("daemon runtime marker exceeds its size limit")
	}
	if err := json.Unmarshal(data, &runtimeDir); err != nil {
		return "", fmt.Errorf("decode daemon runtime marker: %w", err)
	}
	if !validDaemonRuntimeDir(runtimeDir) {
		return "", errors.New("daemon runtime marker must name a clean absolute directory")
	}
	return runtimeDir, nil
}

// WriteDaemonRuntimeDir commits the socket location after the daemon binds it.
func WriteDaemonRuntimeDir(dir, runtimeDir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == filepath.Dir(filepath.Clean(dir)) {
		return errors.New("daemon state directory must be absolute and below the filesystem root")
	}
	if !validDaemonRuntimeDir(runtimeDir) {
		return errors.New("daemon runtime directory must be clean and absolute")
	}
	if err := EnsureDirMode(dir); err != nil {
		return err
	}
	old, err := ReadDaemonRuntimeDir(dir)
	if err != nil || old == runtimeDir {
		return err
	}
	data, err := json.Marshal(runtimeDir)
	if err != nil {
		return err
	}
	if len(data) > maxDaemonRuntimeBytes {
		return errors.New("daemon runtime directory exceeds its size limit")
	}
	return replaceConnectorResources(dir, filepath.Join(dir, daemonRuntimeFile), data)
}

func validDaemonRuntimeDir(dir string) bool {
	return filepath.IsAbs(dir) && dir == filepath.Clean(dir) && dir != filepath.Dir(dir) && !strings.ContainsRune(dir, 0) && utf8.ValidString(dir)
}
