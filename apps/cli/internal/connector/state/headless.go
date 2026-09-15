package state

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	headlessConfigVersion  = 2
	headlessConfigMaxBytes = 1 << 20
	enrollmentMaxBytes     = 16 << 10
)

var statPinnedReadOnlyPath = os.Stat

type pinnedFilePolicy uint8

const (
	nonSecretReadOnly pinnedFilePolicy = iota
	bearerCredential
)

// HeadlessConfig is the non-secret, declarative bootstrap consumed by
// `qurl daemon run`. It uses the same LocalShare shape as interactive qurl so
// container and per-user installs converge on one registry and one runtime.
type HeadlessConfig struct {
	Version int          `yaml:"version"`
	OwnerID string       `yaml:"owner_id"`
	Shares  []LocalShare `yaml:"shares"`
}

// LoadHeadlessConfig reads a strict, versioned configuration file. Config is
// non-secret but must not be writable by another local user while qurl reads
// it. Read-only Kubernetes ConfigMap projection symlinks are supported.
func LoadHeadlessConfig(path string) (*HeadlessConfig, error) {
	data, err := readPinnedFile(path, headlessConfigMaxBytes, nonSecretReadOnly)
	if err != nil {
		return nil, fmt.Errorf("read headless share config: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("headless share config is not valid UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config HeadlessConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode headless share config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("headless share config contains more than one YAML document")
		}
		return nil, fmt.Errorf("decode trailing headless share config: %w", err)
	}
	if config.Version != headlessConfigVersion {
		return nil, fmt.Errorf("headless share config version %d is unsupported", config.Version)
	}
	if !validLocalOwnerID(config.OwnerID) {
		return nil, errors.New("headless share config account owner is invalid")
	}
	if len(config.Shares) != 1 {
		return nil, errors.New("headless share config requires exactly one share")
	}
	for i := range config.Shares {
		share := &config.Shares[i]
		if err := ValidateLocalShareDefinition(share); err != nil {
			return nil, fmt.Errorf("headless share config shares[%d]: %w", i, err)
		}
		if share.DesiredState != "on" {
			return nil, fmt.Errorf("headless share config shares[%d] must have desired_state on", i)
		}
	}
	return &config, nil
}

// ReadEnrollmentCredential reads a first-boot credential from a read-only
// regular file or Kubernetes projected-secret symlink. A single trailing line ending is accepted for Docker/Kubernetes
// secret files; embedded whitespace is rejected so a malformed secret cannot
// be interpreted differently by downstream enrollment code.
func ReadEnrollmentCredential(path string) (string, error) {
	data, err := readPinnedFile(path, enrollmentMaxBytes, bearerCredential)
	if err != nil {
		return "", fmt.Errorf("read enrollment credential: %w", err)
	}
	if !utf8.Valid(data) {
		return "", errors.New("enrollment credential is not valid UTF-8")
	}
	credential := strings.TrimSuffix(string(data), "\n")
	credential = strings.TrimSuffix(credential, "\r")
	if credential == "" {
		return "", errors.New("enrollment credential is empty")
	}
	if strings.IndexFunc(credential, unicode.IsSpace) >= 0 {
		return "", errors.New("enrollment credential contains whitespace")
	}
	return credential, nil
}

func readPinnedFile(path string, limit int64, policy pinnedFilePolicy) ([]byte, error) {
	if err := validatePinnedFilePath(path); err != nil {
		return nil, err
	}
	if err := validatePinnedFileParent(path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink == 0 && !before.Mode().IsRegular() {
		return nil, errors.New("file must be a regular file or projected-secret symlink")
	}
	// os.Open intentionally follows one Kubernetes projected-volume path. The
	// opened descriptor is pinned and compared with the current resolved target
	// below, so an atomic Secret/ConfigMap projection update either yields one
	// complete version or fails closed; the credential is never read by path a
	// second time. Writable targets remain forbidden.
	file, err := os.Open(path) //nolint:gosec // path is an explicit hidden-daemon flag and the opened file is fully validated below.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New("resolved file must be regular and not writable by group or other users")
	}
	writableByAnotherUser, err := pinnedFileWritableByAnotherUser(file, opened)
	if err != nil {
		return nil, err
	}
	if writableByAnotherUser {
		return nil, errors.New("resolved file must be regular and not writable by group or other users")
	}
	if policy == bearerCredential {
		// The enrollment token is a bearer secret, unlike share.yaml. Permit
		// owner-read or a dedicated readable group (needed for Kubernetes
		// projected Secrets with fsGroup), but never world access, group write,
		// or any non-owner execute bit. Ownership/group membership is checked
		// against the opened descriptor on supported Unix targets.
		if opened.Mode().Perm()&0o137 != 0 || !sensitiveFileReadableByProcess(opened) {
			return nil, errors.New("resolved enrollment credential must be readable only by its owner or a dedicated process group")
		}
	}
	if opened.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	after, err := statPinnedReadOnlyPath(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, errors.New("file changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func validatePinnedFilePath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\r\n") ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("file path must be an absolute, clean path")
	}
	return nil
}
