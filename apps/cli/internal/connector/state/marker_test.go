package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The function-level marker tests run against a bare temp directory on every
// platform; the Store-level bracketing is covered in state_test.go.

func TestMarkerAbsentIsNotPresent(t *testing.T) {
	t.Parallel()
	marker, present, err := loadRefreshMarker(t.TempDir())
	if err != nil || present || marker != (RefreshMarker{}) {
		t.Fatalf("loadRefreshMarker(empty dir) = (%+v, %v, %v), want zero/absent/nil", marker, present, err)
	}
}

func TestMarkerRequestSetsFreshUnattempted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	before := time.Now().Unix()
	if err := requestRefresh(dir, "  sustained knock failures  "); err != nil {
		t.Fatal(err)
	}
	marker, present, err := loadRefreshMarker(dir)
	if err != nil || !present {
		t.Fatalf("armed marker = (present=%v, err=%v)", present, err)
	}
	if marker.Version != refreshMarkerVersion || marker.Attempted {
		t.Fatalf("armed marker = %+v, want version %d, unattempted", marker, refreshMarkerVersion)
	}
	if marker.Reason != "sustained knock failures" {
		t.Fatalf("reason = %q, want trimmed", marker.Reason)
	}
	if marker.SetAtUnix < before || marker.SetAtUnix > time.Now().Unix() {
		t.Fatalf("SetAtUnix = %d outside [%d, now]", marker.SetAtUnix, before)
	}
}

func TestMarkerRequestIsEpisodeIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := requestRefresh(dir, "first"); err != nil {
		t.Fatal(err)
	}
	if err := markRefreshAttempted(dir); err != nil {
		t.Fatal(err)
	}
	// The second arming must not reset Attempted=false — that would re-open
	// the bounded Hub-refresh episode the anti-storm rule closes.
	if err := requestRefresh(dir, "second"); err != nil {
		t.Fatal(err)
	}
	marker, present, err := loadRefreshMarker(dir)
	if err != nil || !present || !marker.Attempted || marker.Reason != "first" {
		t.Fatalf("marker after re-arm = (%+v, present=%v, err=%v), want attempted 'first' episode untouched", marker, present, err)
	}
}

func TestMarkerMarkAttemptedFlipsAndPersistsFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := requestRefresh(dir, "why"); err != nil {
		t.Fatal(err)
	}
	before, _, err := loadRefreshMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := markRefreshAttempted(dir); err != nil {
		t.Fatal(err)
	}
	after, present, err := loadRefreshMarker(dir)
	if err != nil || !present {
		t.Fatalf("marker after attempt = (present=%v, err=%v)", present, err)
	}
	if !after.Attempted || after.Reason != before.Reason || after.SetAtUnix != before.SetAtUnix {
		t.Fatalf("attempted marker = %+v, want flipped bit with preserved reason/set_at (%+v)", after, before)
	}
	// Idempotent.
	if err := markRefreshAttempted(dir); err != nil {
		t.Fatal(err)
	}
}

func TestMarkerMarkAttemptedAbsentIsNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := markRefreshAttempted(dir); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := loadRefreshMarker(dir); present {
		t.Fatal("markRefreshAttempted on absent marker created one")
	}
}

func TestMarkerClearRemovesAndAbsentIsSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := clearRefreshMarker(dir); err != nil {
		t.Fatalf("clear on absent marker = %v, want nil (absence is the desired post-condition)", err)
	}
	if err := requestRefresh(dir, "armed"); err != nil {
		t.Fatal(err)
	}
	if err := clearRefreshMarker(dir); err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadRefreshMarker(dir); err != nil || present {
		t.Fatalf("marker after clear = (present=%v, err=%v), want absent", present, err)
	}
}

func TestMarkerCorruptJSONIsInvalidMarkerError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, RefreshMarkerFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil { //nolint:gosec // G306: the marker is a documented non-secret 0644 breadcrumb.
		t.Fatal(err)
	}
	if _, _, err := loadRefreshMarker(dir); !errors.Is(err, errInvalidRefreshMarker) {
		t.Fatalf("corrupt marker error = %v, want errInvalidRefreshMarker", err)
	}
}

func TestMarkerStrictSchemaRejectsAmbiguity(t *testing.T) {
	t.Parallel()
	valid := `{"version":1,"reason":"r","attempted":false,"set_at_unix":1700000000}`
	cases := map[string]string{
		"duplicate field":      `{"version":1,"version":1,"attempted":false,"set_at_unix":1700000000}`,
		"unknown field":        `{"version":1,"attempted":false,"set_at_unix":1700000000,"extra":1}`,
		"missing version":      `{"attempted":false,"set_at_unix":1700000000}`,
		"missing attempted":    `{"version":1,"set_at_unix":1700000000}`,
		"missing set_at":       `{"version":1,"attempted":false}`,
		"wrong version":        `{"version":2,"attempted":false,"set_at_unix":1700000000}`,
		"nonpositive set_at":   `{"version":1,"attempted":false,"set_at_unix":0}`,
		"not an object":        `[1,2,3]`,
		"trailing JSON":        valid + `{}`,
		"control-char reason":  `{"version":1,"reason":"a\tb","attempted":false,"set_at_unix":1700000000}`,
		"untrimmed reason":     `{"version":1,"reason":" padded ","attempted":false,"set_at_unix":1700000000}`,
		"oversized reason":     `{"version":1,"reason":"` + strings.Repeat("r", refreshMarkerReasonMaxBytes+1) + `","attempted":false,"set_at_unix":1700000000}`,
		"non-string field key": `{"version":1,"attempted":"yes","set_at_unix":1700000000}`,
	}
	if _, err := decodeRefreshMarker([]byte(valid)); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeRefreshMarker([]byte(raw)); err == nil {
				t.Fatalf("decodeRefreshMarker accepted %s: %s", name, raw)
			}
		})
	}
}

func TestMarkerRequestRejectsUnboundedReason(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, reason := range map[string]string{
		"oversized":    strings.Repeat("x", refreshMarkerReasonMaxBytes+1),
		"control char": "line\nbreak",
		"invalid utf8": string([]byte{0xff, 0xfe}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := requestRefresh(dir, reason); err == nil {
				t.Fatalf("requestRefresh accepted %s reason", name)
			}
			if _, present, _ := loadRefreshMarker(dir); present {
				t.Fatal("rejected reason still armed a marker")
			}
		})
	}
}

func TestMarkerEmptyAndOversizeFilesAreErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, RefreshMarkerFile)
	if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // G306: non-secret 0644 breadcrumb.
		t.Fatal(err)
	}
	if _, _, err := loadRefreshMarker(dir); !errors.Is(err, errInvalidRefreshMarker) {
		t.Fatalf("empty marker error = %v, want errInvalidRefreshMarker", err)
	}
	big := append([]byte(`{"version":1,"reason":"`), []byte(strings.Repeat("a", refreshMarkerFileMaxBytes))...)
	big = append(big, []byte(`","attempted":false,"set_at_unix":1700000000}`)...)
	if err := os.WriteFile(path, big, 0o644); err != nil { //nolint:gosec // G306: non-secret 0644 breadcrumb.
		t.Fatal(err)
	}
	if _, _, err := loadRefreshMarker(dir); !errors.Is(err, errInvalidRefreshMarker) {
		t.Fatalf("oversize marker error = %v, want errInvalidRefreshMarker", err)
	}
}

func TestMarkerRequestLeavesExistingCorruptUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, RefreshMarkerFile)
	corrupt := []byte("{torn")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil { //nolint:gosec // G306: non-secret 0644 breadcrumb.
		t.Fatal(err)
	}
	// Presence-gated, not decode-gated: the corrupt file marks an open
	// episode; arming must not overwrite it.
	if err := requestRefresh(dir, "new episode"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: fixed name under the test's own temp dir.
	if err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("marker after presence-gated arm = (%q, %v), want untouched corrupt bytes", got, err)
	}
}

func TestMarkerRejectsAndNeverOverwritesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"attempted":false,"set_at_unix":1700000000}`), 0o644); err != nil { //nolint:gosec // G306: non-secret test fixture.
		t.Fatal(err)
	}
	link := filepath.Join(dir, RefreshMarkerFile)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, _, err := loadRefreshMarker(dir); !errors.Is(err, errInvalidRefreshMarker) {
		t.Fatalf("symlink marker load = %v, want errInvalidRefreshMarker", err)
	}
	// The presence gate counts the symlink as an open episode; arming leaves
	// it alone rather than following or replacing it.
	if err := requestRefresh(dir, "arm over symlink"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("marker path after arm = (%v, %v), want the symlink untouched", info, err)
	}
	// markRefreshAttempted surfaces the invalid marker instead of rewriting
	// through the link.
	if err := markRefreshAttempted(dir); !errors.Is(err, errInvalidRefreshMarker) {
		t.Fatalf("markRefreshAttempted over symlink = %v, want errInvalidRefreshMarker", err)
	}
}

func TestMarkerOnDiskShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := requestRefresh(dir, "shape"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, RefreshMarkerFile)) //nolint:gosec // G304: fixed name under the test's own temp dir.
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("marker is not JSON: %v (%q)", err, raw)
	}
	for _, key := range []string{"version", "reason", "attempted", "set_at_unix"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("marker JSON missing %q: %q", key, raw)
		}
	}
	if len(decoded) != 4 {
		t.Fatalf("marker JSON has %d fields, want exactly 4: %q", len(decoded), raw)
	}
}

func TestMarkerConcurrentRequestsNeverResetAttemptedEpisode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := requestRefresh(dir, "episode"); err != nil {
		t.Fatal(err)
	}
	if err := markRefreshAttempted(dir); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				// Concurrent re-arms against an attempted episode must all be
				// presence-gated no-ops.
				if err := requestRefresh(dir, "storm"); err != nil {
					t.Errorf("requestRefresh = %v", err)
				}
			}
		}()
	}
	wg.Wait()
	marker, present, err := loadRefreshMarker(dir)
	if err != nil || !present || !marker.Attempted || marker.Reason != "episode" {
		t.Fatalf("marker after concurrent arms = (%+v, present=%v, err=%v), want the attempted episode untouched", marker, present, err)
	}
}
