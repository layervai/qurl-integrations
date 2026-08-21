package replica

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// This file carries the runtime half of the package: the boot-time resolver
// chain that discovers a per-replica salt for this process. Resolution order,
// first non-empty wins:
//
//  1. Explicit env LAYERV_REPLICA_ID — operator override; deterministic salts
//     for tests, bare-metal deploys, and any orchestrator where neither task
//     metadata nor a per-replica hostname is unique (Kubernetes downward-API:
//     `valueFrom: {fieldRef: {fieldPath: metadata.name}}`).
//  2. ECS task UUID via ECS_CONTAINER_METADATA_URI_V4 — the task ARN's last
//     segment, fetched once at boot from the loopback metadata endpoint under
//     a 2s deadline. A replaced task gets a new UUID, which is exactly the
//     freshness the salt needs against a lingering prior registration.
//  3. Hostname (os.Hostname) — container/pod/VM hostnames are typically
//     unique per replica, the safe default for non-ECS orchestrators.
//  4. Machine ID from /etc/machine-id (then /var/lib/dbus/machine-id) —
//     STABLE PER HOST, therefore SHARED by co-located replicas; ranked after
//     hostname so multi-replica-per-host deployments do not re-introduce the
//     duplicate-registration collision this package exists to prevent. The
//     raw identifier is hashed before it becomes a salt and is never logged.
//  5. Random hex generated at process start; the resolver latches it for the
//     process lifetime and the metadata carries a warning because replicas
//     will re-salt across restarts.
//
// Every branch flows through Normalize, so the rendered proxy name stays
// DNS-safe regardless of source.

// EnvReplicaID is the explicit operator override. When set non-empty (after
// whitespace trim), it short-circuits the resolution chain. The LAYERV_*
// spelling is the cross-tool operator contract shared with the standalone
// qURL Connector; a bare REPLICA_ID would risk colliding with generic
// orchestrator tooling env.
const EnvReplicaID = "LAYERV_REPLICA_ID"

// EnvECSContainerMetadataURI is the AWS-injected ECS task metadata endpoint
// (v4 shape). Present on Fargate and EC2-backed ECS; absent means not on ECS.
const EnvECSContainerMetadataURI = "ECS_CONTAINER_METADATA_URI_V4"

// Source identifies which branch of the resolver produced the salt, surfaced
// in Metadata so the caller can log it once per process.
type Source string

// The resolver-chain sources, in resolution order.
const (
	SourceEnv            Source = "env"
	SourceECS            Source = "ecs"
	SourceHostname       Source = "hostname"
	SourceMachineID      Source = "machine-id"
	SourceRandomFallback Source = "random-fallback"
)

// Metadata describes why a particular discriminator was returned, so the
// boot-time log line can carry the source label.
type Metadata struct {
	// Source is the branch of the resolution chain that produced the salt.
	Source Source

	// Raw is the pre-normalization value the source produced, logged for
	// operator triage. Deliberately empty for SourceMachineID: the stable
	// host identifier must not be exposed, only its hashed salt.
	Raw string

	// Warning is set when a non-stable branch fired (the random fallback).
	// The caller surfaces it at warn level so "this process is using a
	// non-stable salt" is visible outside debug noise.
	Warning string
}

// MachineIDReader is injected for tests; nil resolves to the default reader
// (/etc/machine-id, then /var/lib/dbus/machine-id).
type MachineIDReader func() (string, error)

// HostnameReader is injected for tests; nil resolves to os.Hostname.
type HostnameReader func() (string, error)

// ECSFetcher is injected for tests; nil resolves to the default fetcher (a
// 2s-bounded GET on ${ECS_CONTAINER_METADATA_URI_V4}/task).
type ECSFetcher func(ctx context.Context, endpoint string) (string, error)

// Resolver wires the optional injection points and caches the resolved salt
// for the process. The zero value is the production resolver; construct one
// at boot and reuse it — the salt is a boot-time property and must not drift
// mid-process, or the registrations rendered from it would drift too.
type Resolver struct {
	Env      map[string]string // non-nil replaces os.LookupEnv (tests)
	Machine  MachineIDReader   // nil → default machine-id file reader
	Hostname HostnameReader    // nil → os.Hostname
	ECS      ECSFetcher        // nil → default ECS metadata fetcher
	RandRead func(b []byte) (int, error)

	once       sync.Once
	mu         sync.Mutex
	cached     string
	cachedMeta Metadata
	softErrors []error
	warnings   []string
}

// Resolve walks the resolution chain and returns the discriminator plus
// metadata. Latched via sync.Once: subsequent calls return the same value
// regardless of environment changes. The error return is always nil today —
// every branch either produces a value or falls through, and the random
// fallback degrades a crypto/rand failure to a sentinel salt plus warning
// rather than failing boot; the signature keeps the error for a future
// hard-fail mode.
func (r *Resolver) Resolve(ctx context.Context) (string, Metadata, error) {
	r.once.Do(func() {
		r.cached, r.cachedMeta = r.resolveOnce(ctx)
	})
	return r.cached, r.cachedMeta, nil
}

// Errors returns the soft errors accumulated across earlier resolution
// branches (for example an ECS metadata fetch that timed out before the
// resolver fell through to hostname), joined for errors.Is inspection, or nil.
func (r *Resolver) Errors() error {
	r.mu.Lock()
	errs := append([]error(nil), r.softErrors...)
	r.mu.Unlock()
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// Warnings returns non-fatal operator-facing warnings accumulated while
// resolving, typically that an explicit override was ignored because it
// normalized empty.
func (r *Resolver) Warnings() []string {
	r.mu.Lock()
	warnings := append([]string(nil), r.warnings...)
	r.mu.Unlock()
	if len(warnings) == 0 {
		return nil
	}
	return warnings
}

func (r *Resolver) appendSoftError(err error) {
	r.mu.Lock()
	r.softErrors = append(r.softErrors, err)
	r.mu.Unlock()
}

func (r *Resolver) appendWarning(warning string) {
	r.mu.Lock()
	r.warnings = append(r.warnings, warning)
	r.mu.Unlock()
}

func (r *Resolver) lookupEnv(key string) (string, bool) {
	if r.Env != nil {
		v, ok := r.Env[key]
		return v, ok
	}
	return os.LookupEnv(key)
}

func (r *Resolver) machineID() (string, error) {
	if r.Machine != nil {
		return r.Machine()
	}
	return defaultMachineIDReader()
}

func (r *Resolver) hostname() (string, error) {
	if r.Hostname != nil {
		return r.Hostname()
	}
	return os.Hostname()
}

func (r *Resolver) ecs(ctx context.Context, endpoint string) (string, error) {
	if r.ECS != nil {
		return r.ECS(ctx, endpoint)
	}
	return defaultECSFetcher(ctx, endpoint)
}

func (r *Resolver) randomHex(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("invalid random length %d", n)
	}
	buf := make([]byte, (n+1)/2)
	read := rand.Read
	if r.RandRead != nil {
		read = r.RandRead
	}
	readN, err := read(buf)
	if err != nil {
		return "", err
	}
	if readN != len(buf) {
		return "", io.ErrUnexpectedEOF
	}
	out := hex.EncodeToString(buf)
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// resolveOnce is the single-pass body invoked under sync.Once. It never
// returns an error: exhausting every branch lands on the random fallback,
// which itself degrades a crypto/rand outage to a sentinel plus warning.
func (r *Resolver) resolveOnce(ctx context.Context) (string, Metadata) {
	if v, ok := r.lookupEnv(EnvReplicaID); ok {
		if trimmed := Normalize(v); trimmed != "" {
			return trimmed, Metadata{Source: SourceEnv, Raw: v}
		}
		if strings.TrimSpace(v) != "" {
			r.appendWarning(EnvReplicaID + " dropped after normalization; falling through to resolver chain")
		}
	}

	if endpoint, ok := r.lookupEnv(EnvECSContainerMetadataURI); ok && strings.TrimSpace(endpoint) != "" {
		if salt, meta, ok := r.resolveECS(ctx, strings.TrimSpace(endpoint)); ok {
			return salt, meta
		}
	}

	// Hostname is preferred over machine-id because container/pod/VM
	// hostnames are typically unique per replica, whereas machine-id is
	// host-keyed and shared by co-located replicas — see the file header.
	if h, err := r.hostname(); err == nil {
		if trimmed := Normalize(h); trimmed != "" {
			return trimmed, Metadata{Source: SourceHostname, Raw: h}
		}
	} else {
		r.appendSoftError(fmt.Errorf("hostname read: %w", err))
	}

	if id, err := r.machineID(); err == nil {
		if trimmed := normalizeHashed(id); trimmed != "" {
			// Raw stays empty: never expose the stable host identifier,
			// only the hashed salt derived from it.
			return trimmed, Metadata{Source: SourceMachineID}
		}
	} else {
		r.appendSoftError(fmt.Errorf("machine-id read: %w", err))
	}

	rnd, err := r.randomHex(MaxDiscriminatorLen)
	if err != nil {
		r.appendSoftError(fmt.Errorf("random fallback: %w", err))
		// A crypto/rand outage breaks far more than this salt; degrade to a
		// fixed sentinel and warn loudly rather than failing boot. The
		// sentinel is deliberately NOT unique — if entropy fails on several
		// replicas at once they will collide identically, and the only
		// workaround is the operator override (the one subsystem that could
		// produce a unique salt here is the one that is broken).
		return "no-entropy", Metadata{
			Source:  SourceRandomFallback,
			Warning: fmt.Sprintf("crypto/rand failed: %v — using non-unique sentinel salt; %s is the only available workaround until entropy recovers", err, EnvReplicaID),
		}
	}
	return rnd, Metadata{
		Source:  SourceRandomFallback,
		Raw:     rnd,
		Warning: "no stable identity source found; using a random per-process salt — replicas will re-salt across restarts (set " + EnvReplicaID + " or run where task metadata or per-replica hostnames exist)",
	}
}

// resolveECS runs the bounded metadata fetch and UUID extraction for the ECS
// branch. False means fall through to the next branch (soft errors recorded).
func (r *Resolver) resolveECS(ctx context.Context, endpoint string) (string, Metadata, bool) {
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	raw, err := r.ecs(fetchCtx, endpoint)
	cancel()
	if err != nil {
		r.appendSoftError(fmt.Errorf("ecs metadata fetch: %w", err))
		return "", Metadata{}, false
	}
	uuid := extractECSTaskUUID(raw)
	if uuid == "" {
		r.appendSoftError(errors.New("ecs metadata missing usable TaskARN UUID"))
		return "", Metadata{}, false
	}
	trimmed := Normalize(uuid)
	if trimmed == "" {
		r.appendSoftError(fmt.Errorf("ecs metadata task UUID normalized empty: %q", uuid))
		return "", Metadata{}, false
	}
	return trimmed, Metadata{Source: SourceECS, Raw: uuid}, true
}

// normalizeHashed converts a stable identifier that must not be rendered
// directly (machine-id) into a salt: hash first, then normalize the digest.
func normalizeHashed(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return Normalize(shortHash(raw, MaxDiscriminatorLen))
}

// extractECSTaskUUID parses the JSON returned by the task metadata endpoint
// and returns the last URI segment of the TaskARN (the per-task UUID), or ""
// when the payload is unusable so the resolver falls through.
func extractECSTaskUUID(body string) string {
	var payload struct {
		TaskARN string `json:"TaskARN"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}
	// TaskARN shape: arn:aws:ecs:<region>:<account>:task/<cluster>/<uuid> —
	// the UUID is the last '/'-separated segment.
	taskARN := strings.TrimRight(payload.TaskARN, "/")
	if taskARN == "" {
		return ""
	}
	if i := strings.LastIndex(taskARN, "/"); i >= 0 && i+1 < len(taskARN) {
		return taskARN[i+1:]
	}
	return ""
}

// defaultMachineIDReader reads /etc/machine-id then /var/lib/dbus/machine-id
// and returns the first found. Other platforms lack these files and fall
// through to the hostname branch above them in the chain, which keeps this a
// pure-stdlib reader with no per-OS dependency.
func defaultMachineIDReader() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(path) //nolint:gosec // G304: fixed well-known system paths, no operator input.
		if err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", errors.New("no machine-id file found")
}

// ecsMetadataClient is a dedicated client so a future caller that mutates
// http.DefaultClient cannot bleed into the metadata fetch. Zero-value client:
// the caller-supplied context carries the only deadline.
var ecsMetadataClient = &http.Client{}

func defaultECSFetcher(ctx context.Context, endpoint string) (string, error) {
	url := strings.TrimRight(endpoint, "/") + "/task"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := ecsMetadataClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ecs metadata: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
