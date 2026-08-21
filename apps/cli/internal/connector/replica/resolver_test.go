package replica

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// resolverEnv builds an injected env map with only the given entries, so the
// tests never read the process environment.
func resolverEnv(entries map[string]string) map[string]string {
	if entries == nil {
		return map[string]string{}
	}
	return entries
}

func TestResolveEnvBranch(t *testing.T) {
	t.Parallel()
	r := &Resolver{Env: resolverEnv(map[string]string{EnvReplicaID: "  Replica_One!  "})}
	got, meta, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "replicaone" {
		t.Fatalf("discriminator = %q, want normalized replicaone", got)
	}
	if meta.Source != SourceEnv || meta.Raw != "  Replica_One!  " {
		t.Fatalf("metadata = %+v, want env source with the raw override", meta)
	}
}

func TestResolveEnvNormalizeEmptyWarnsAndFallsThrough(t *testing.T) {
	t.Parallel()
	r := &Resolver{
		Env:      resolverEnv(map[string]string{EnvReplicaID: "___"}),
		Hostname: func() (string, error) { return "pod-7", nil },
	}
	got, meta, _ := r.Resolve(context.Background())
	if got != "pod-7" || meta.Source != SourceHostname {
		t.Fatalf("resolve = %q from %s, want hostname fallback pod-7", got, meta.Source)
	}
	warnings := r.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], EnvReplicaID) {
		t.Fatalf("warnings = %v, want one naming the dropped override", warnings)
	}
}

func TestResolveECSBranchExtractsTaskUUID(t *testing.T) {
	t.Parallel()
	var fetchedEndpoint string
	r := &Resolver{
		Env: resolverEnv(map[string]string{EnvECSContainerMetadataURI: "http://169.254.2.2/v4/abc"}),
		ECS: func(ctx context.Context, endpoint string) (string, error) {
			fetchedEndpoint = endpoint
			if _, ok := ctx.Deadline(); !ok {
				t.Error("ECS fetch context has no deadline; the boot-time fetch must be bounded")
			}
			return `{"TaskARN":"arn:aws:ecs:us-east-1:123:task/cluster/9f3e2b1a-uuid"}`, nil
		},
	}
	got, meta, _ := r.Resolve(context.Background())
	if got != Normalize("9f3e2b1a-uuid") || meta.Source != SourceECS {
		t.Fatalf("resolve = %q from %s, want normalized task UUID from ecs", got, meta.Source)
	}
	if fetchedEndpoint != "http://169.254.2.2/v4/abc" {
		t.Fatalf("fetched endpoint = %q, want the env-provided endpoint", fetchedEndpoint)
	}
}

func TestResolveECSFailureFallsThroughWithSoftError(t *testing.T) {
	t.Parallel()
	fetchErr := errors.New("metadata endpoint down")
	r := &Resolver{
		Env:      resolverEnv(map[string]string{EnvECSContainerMetadataURI: "http://169.254.2.2/v4/abc"}),
		ECS:      func(context.Context, string) (string, error) { return "", fetchErr },
		Hostname: func() (string, error) { return "pod-9", nil },
	}
	got, meta, _ := r.Resolve(context.Background())
	if got != "pod-9" || meta.Source != SourceHostname {
		t.Fatalf("resolve = %q from %s, want hostname after ECS failure", got, meta.Source)
	}
	if softErrs := r.Errors(); softErrs == nil || !errors.Is(softErrs, fetchErr) {
		t.Fatalf("Errors() = %v, want joined ECS fetch error", softErrs)
	}
}

func TestResolveHostnamePreferredOverMachineID(t *testing.T) {
	t.Parallel()
	r := &Resolver{
		Env:      resolverEnv(nil),
		Hostname: func() (string, error) { return "replica-host-3", nil },
		Machine:  func() (string, error) { return "0123456789abcdef", nil },
	}
	got, meta, _ := r.Resolve(context.Background())
	if got != "replica-host-3" || meta.Source != SourceHostname {
		t.Fatalf("resolve = %q from %s, want the per-replica hostname over host-keyed machine-id", got, meta.Source)
	}
}

func TestResolveMachineIDBranchHashesAndHidesRaw(t *testing.T) {
	t.Parallel()
	const machineID = "0123456789abcdef0123456789abcdef"
	r := &Resolver{
		Env:      resolverEnv(nil),
		Hostname: func() (string, error) { return "", errors.New("no hostname") },
		Machine:  func() (string, error) { return machineID, nil },
	}
	got, meta, _ := r.Resolve(context.Background())
	if meta.Source != SourceMachineID {
		t.Fatalf("source = %s, want machine-id", meta.Source)
	}
	if got != normalizeHashed(machineID) {
		t.Fatalf("discriminator = %q, want the hashed salt", got)
	}
	if strings.Contains(got, machineID) || meta.Raw != "" {
		t.Fatalf("machine-id leaked: salt %q raw %q; the stable host identifier must never surface", got, meta.Raw)
	}
}

func TestResolveRandomFallbackWarns(t *testing.T) {
	t.Parallel()
	r := &Resolver{
		Env:      resolverEnv(nil),
		Hostname: func() (string, error) { return "", errors.New("no hostname") },
		Machine:  func() (string, error) { return "", errors.New("no machine-id") },
	}
	got, meta, _ := r.Resolve(context.Background())
	if meta.Source != SourceRandomFallback || meta.Warning == "" {
		t.Fatalf("metadata = %+v, want random-fallback with a warning", meta)
	}
	if len(got) != MaxDiscriminatorLen || Normalize(got) != got {
		t.Fatalf("random salt = %q, want %d normalized hex characters", got, MaxDiscriminatorLen)
	}
}

func TestResolveEntropyOutageDegradesToSentinel(t *testing.T) {
	t.Parallel()
	r := &Resolver{
		Env:      resolverEnv(nil),
		Hostname: func() (string, error) { return "", errors.New("no hostname") },
		Machine:  func() (string, error) { return "", errors.New("no machine-id") },
		RandRead: func([]byte) (int, error) { return 0, errors.New("entropy exhausted") },
	}
	got, meta, _ := r.Resolve(context.Background())
	if got != "no-entropy" || meta.Source != SourceRandomFallback {
		t.Fatalf("resolve = %q from %s, want the no-entropy sentinel", got, meta.Source)
	}
	if !strings.Contains(meta.Warning, EnvReplicaID) {
		t.Fatalf("warning %q must name the operator override as the workaround", meta.Warning)
	}
}

func TestResolveIdempotentWithinResolver(t *testing.T) {
	t.Parallel()
	env := resolverEnv(map[string]string{EnvReplicaID: "salt-a"})
	r := &Resolver{Env: env}
	first, _, _ := r.Resolve(context.Background())
	env[EnvReplicaID] = "salt-b" // must not matter: the salt is latched
	second, _, _ := r.Resolve(context.Background())
	if first != second || first != "salt-a" {
		t.Fatalf("resolve drifted %q → %q; the salt is a boot-time property", first, second)
	}
}

func TestResolveConcurrentCallsShareOneResolution(t *testing.T) {
	t.Parallel()
	var hostnameCalls int
	var mu sync.Mutex
	r := &Resolver{
		Env: resolverEnv(nil),
		Hostname: func() (string, error) {
			mu.Lock()
			hostnameCalls++
			mu.Unlock()
			return "pod-x", nil
		},
	}
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, _ = r.Resolve(context.Background())
		}()
	}
	wg.Wait()
	for i, got := range results {
		if got != "pod-x" {
			t.Fatalf("results[%d] = %q, want pod-x", i, got)
		}
	}
	if hostnameCalls != 1 {
		t.Fatalf("hostname branch ran %d times, want once under the latch", hostnameCalls)
	}
}

func TestExtractECSTaskUUID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, want string
	}{
		{"canonical", `{"TaskARN":"arn:aws:ecs:us-east-1:1:task/cluster/uuid-1"}`, "uuid-1"},
		{"no cluster segment", `{"TaskARN":"arn:aws:ecs:task/uuid-2"}`, "uuid-2"},
		{"trailing slash", `{"TaskARN":"arn:aws:ecs:us-east-1:1:task/cluster/uuid-3/"}`, "uuid-3"},
		{"missing arn", `{"TaskARN":""}`, ""},
		{"no separator", `{"TaskARN":"just-a-string"}`, ""},
		{"only slashes", `{"TaskARN":"///"}`, ""},
		{"not json", `not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := extractECSTaskUUID(tc.body); got != tc.want {
				t.Fatalf("extractECSTaskUUID(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestResolveNilDefaultsDoNotPanic(t *testing.T) {
	t.Parallel()
	// Zero-value resolver with only the env neutralized: every reader takes
	// its production default. Whatever branch wins on this host, the resolver
	// must produce a non-empty normalized salt without panicking.
	r := &Resolver{Env: resolverEnv(nil)}
	got, meta, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == "" || Normalize(got) != got {
		t.Fatalf("resolve = %q from %s, want a non-empty normalized salt", got, meta.Source)
	}
}
