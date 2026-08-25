package matchedcohort

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

const customerProbeOutputLimit = 64 << 10

// BinaryGetProbe runs the selected released qurl binary for the customer GET
// outcomes. It passes only the private API-key file path. The parent never
// reads or exports the bearer value.
type BinaryGetProbe struct {
	Binary           string
	BinarySHA256     string
	APIEndpoint      string
	APIKeyFile       string
	DeploymentFile   string
	DeploymentSHA256 string
	StateRoot        string
	Expected         map[string][]byte
	Timeout          time.Duration
	beforeExec       func()
}

// Preflight validates every local authority and the released deployment
// before a caller starts either prepared NHP operation.
func (p *BinaryGetProbe) Preflight(first, second FixedIdentity) error { //nolint:gocritic // Identities are immutable authority rows.
	if err := p.validate([]FixedIdentity{first, second}); err != nil {
		return err
	}
	if _, err := qurl.LoadDeployment(p.DeploymentFile); err != nil {
		return fmt.Errorf("%w: customer probe deployment", errInvalidAuthority)
	}
	return nil
}

// Both performs the two independent customer GETs concurrently.
func (p *BinaryGetProbe) Both(ctx context.Context, first, second FixedIdentity) error { //nolint:gocritic // Identities are immutable authority rows.
	if err := p.validate([]FixedIdentity{first, second}); err != nil {
		return err
	}
	results := make(chan error, 2)
	identities := [2]FixedIdentity{first, second}
	for index := range identities {
		go func(identity FixedIdentity) { results <- p.get(ctx, identity) }(identities[index])
	}
	return errors.Join(<-results, <-results)
}

// Sibling performs one customer GET while the sibling session remains live.
func (p *BinaryGetProbe) Sibling(ctx context.Context, identity FixedIdentity) error { //nolint:gocritic // Identity is one immutable authority row.
	if err := p.validate([]FixedIdentity{identity}); err != nil {
		return err
	}
	return p.get(ctx, identity)
}

func (p *BinaryGetProbe) validate(identities []FixedIdentity) error { //nolint:gocyclo // Closed filesystem and identity checks stay in one fence.
	if p == nil || !cleanAbsolute(p.Binary) || !cleanAbsolute(p.APIKeyFile) || !cleanAbsolute(p.DeploymentFile) ||
		!cleanAbsolute(p.StateRoot) || !validText(p.APIEndpoint) || !strings.HasPrefix(p.APIEndpoint, "https://") ||
		p.Timeout <= 0 || p.Timeout > 30*time.Second {
		return fmt.Errorf("%w: binary customer probe", errInvalidAuthority)
	}
	for path, executable := range map[string]bool{p.Binary: true, p.APIKeyFile: false, p.DeploymentFile: false} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 ||
			(executable && info.Mode().Perm()&0o111 == 0) || (!executable && info.Mode().Perm()&0o077 != 0) {
			return fmt.Errorf("%w: customer probe file metadata", errInvalidAuthority)
		}
		if !singleLinkOwnedByRootOrCurrentUser(info) {
			return fmt.Errorf("%w: customer probe file ownership", errInvalidAuthority)
		}
	}
	if stableProbeFileDigest(p.Binary, true) != p.BinarySHA256 || stableProbeFileDigest(p.DeploymentFile, false) != p.DeploymentSHA256 {
		return fmt.Errorf("%w: customer probe file digest", errInvalidAuthority)
	}
	root, err := os.Lstat(p.StateRoot)
	if err != nil || !root.IsDir() || root.Mode().Perm() != 0o700 || root.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: customer probe state root", errInvalidAuthority)
	}
	for index := range identities {
		identity := identities[index]
		if !validText(identity.Label) || !validText(identity.CRID) || len(p.Expected[identity.Label]) == 0 {
			return fmt.Errorf("%w: customer probe identity", errInvalidAuthority)
		}
	}
	return nil
}

func (p *BinaryGetProbe) get(parent context.Context, identity FixedIdentity) error { //nolint:gocritic // Identity is one immutable authority row.
	directory, err := os.MkdirTemp(p.StateRoot, "get-"+identity.Label+"-")
	if err != nil {
		return errors.New("create private customer probe directory")
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Private state directory must be owner-only and searchable.
		return errors.New("secure customer probe directory")
	}
	destination := filepath.Join(directory, "download")
	ctx, cancel := context.WithTimeout(parent, p.Timeout)
	defer cancel()
	executable, digest := openStableProbeFile(p.Binary, true)
	if executable == nil || digest != p.BinarySHA256 {
		return fmt.Errorf("customer GET %s executable authority changed", identity.Label)
	}
	defer func() { _ = executable.Close() }()
	deployment, deploymentDigest := openStableProbeFile(p.DeploymentFile, false)
	if deployment == nil || deploymentDigest != p.DeploymentSHA256 {
		return fmt.Errorf("customer GET %s deployment authority changed", identity.Label)
	}
	defer func() { _ = deployment.Close() }()
	if p.beforeExec != nil {
		p.beforeExec()
	}
	command, inheritedDeployment, err := commandForOpenedExecutable(ctx, executable, deployment,
		"--endpoint", p.APIEndpoint, "--quiet", "get", identity.CRID, "--file", destination)
	if err != nil {
		return fmt.Errorf("customer GET %s exact executable launch failed", identity.Label)
	}
	command.Env = probeEnvironment(p.APIEndpoint, p.APIKeyFile, inheritedDeployment, directory)
	stdout, stderr := &boundedProbeBuffer{}, &boundedProbeBuffer{}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("customer GET %s failed", identity.Label)
	}
	if stableProbeFileDigest(p.Binary, true) != p.BinarySHA256 || stableProbeFileDigest(p.DeploymentFile, false) != p.DeploymentSHA256 {
		return fmt.Errorf("customer GET %s authority changed during execution", identity.Label)
	}
	if stdout.overflow || stderr.overflow {
		return fmt.Errorf("customer GET %s output exceeded its bound", identity.Label)
	}
	got, err := os.ReadFile(destination) //nolint:gosec // Destination is one private fixed child.
	if err != nil || !bytes.Equal(got, p.Expected[identity.Label]) {
		return fmt.Errorf("customer GET %s returned different bytes", identity.Label)
	}
	return nil
}

func stableProbeFileDigest(path string, executable bool) string {
	file, digest := openStableProbeFile(path, executable)
	if file != nil {
		_ = file.Close()
	}
	return digest
}

func openStableProbeFile(path string, executable bool) (openedFile *os.File, digest string) { //nolint:gocyclo // One exact open-inode authority fence.
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o022 != 0 ||
		(executable && before.Mode().Perm() != 0o500 && before.Mode().Perm() != 0o555) {
		return nil, ""
	}
	if !singleLinkOwnedByRootOrCurrentUser(before) {
		return nil, ""
	}
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, ""
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, ""
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		return nil, ""
	}
	after, err := os.Lstat(path)
	openedAfter, openedErr := file.Stat()
	if err != nil || openedErr != nil || !os.SameFile(before, after) || !os.SameFile(opened, openedAfter) ||
		after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		openedAfter.Size() != opened.Size() || !openedAfter.ModTime().Equal(opened.ModTime()) {
		_ = file.Close()
		return nil, ""
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ""
	}
	return file, hex.EncodeToString(hasher.Sum(nil))
}

func probeEnvironment(endpoint, keyFile, deploymentFile, stateRoot string) []string {
	values := map[string]string{
		"LANG":              "C.UTF-8",
		"LC_ALL":            "C.UTF-8",
		"NO_COLOR":          "1",
		"QURL_API_KEY_FILE": keyFile,
		"QURL_DEPLOYMENT":   deploymentFile,
		"QURL_ENDPOINT":     endpoint,
		"XDG_STATE_HOME":    stateRoot,
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

type boundedProbeBuffer struct {
	mu       sync.Mutex
	value    bytes.Buffer
	overflow bool
}

func (b *boundedProbeBuffer) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := customerProbeOutputLimit - b.value.Len()
	if remaining < len(raw) {
		b.overflow = true
		if remaining > 0 {
			_, _ = b.value.Write(raw[:remaining])
		}
		return len(raw), nil
	}
	return b.value.Write(raw)
}
