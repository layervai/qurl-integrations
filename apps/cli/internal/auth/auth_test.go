package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// probeStore records whether the credential store was consulted, so the
// hermetic-mode contract (QURL_API_KEY bypasses storage entirely) is
// asserted rather than assumed.
type probeStore struct {
	key     string
	touched bool
}

func (p *probeStore) Save(string) error { p.touched = true; return nil }
func (p *probeStore) Load() (string, error) {
	p.touched = true
	if p.key == "" {
		return "", ErrNoCredential
	}
	return p.key, nil
}
func (p *probeStore) Delete() error { p.touched = true; return nil }
func (p *probeStore) Name() string  { return "probe" }

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestResolveEnvironmentIsHermetic(t *testing.T) {
	store := &probeStore{key: "lv_test_storedvalue0123456789"}
	key, source, err := Resolve(lookupFrom(map[string]string{EnvAPIKey: "  lv_test_fromenvvalue123456789  "}), store)
	if err != nil {
		t.Fatal(err)
	}
	if key != "lv_test_fromenvvalue123456789" {
		t.Errorf("key = %q, want the trimmed env value", key)
	}
	if source != SourceEnvironment {
		t.Errorf("source = %q, want environment", source)
	}
	if store.touched {
		t.Error("hermetic mode must not touch the credential store")
	}
}

func TestResolveFallsBackToStore(t *testing.T) {
	store := &probeStore{key: "lv_test_storedvalue0123456789"}
	key, source, err := Resolve(lookupFrom(nil), store)
	if err != nil {
		t.Fatal(err)
	}
	if key != store.key || source != SourceStore {
		t.Errorf("key=%q source=%q, want the stored key", key, source)
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	_, _, err := Resolve(lookupFrom(nil), &probeStore{})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
	_, _, err = Resolve(lookupFrom(nil), nil)
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("nil store: err = %v, want ErrNoCredential", err)
	}
}

func TestValidateKeyShape(t *testing.T) {
	for _, key := range []string{
		"lv_live_abcdefghij0123456789",
		"lv_test_ABCDEFGHIJ0123456789xyz",
	} {
		if err := ValidateKeyShape(key); err != nil {
			t.Errorf("valid key rejected: %v", err)
		}
	}
	for name, key := range map[string]string{
		"wrong prefix": "at_abcdefghij0123456789",
		"no prefix":    "abcdefghij0123456789abcdef",
		"too short":    "lv_live_abc",
		"bad chars":    "lv_test_abcdefghij01234567!!",
		"empty":        "",
	} {
		if err := ValidateKeyShape(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)

	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty store Load = %v, want ErrNoCredential", err)
	}

	const key = "lv_test_roundtripvalue0123456789"
	if err := store.Save(key); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != key {
		t.Errorf("Load = %q, want %q", got, key)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "credential"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("credential file mode = %o, want 0600", perm)
		}
	}

	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("after delete: Load = %v, want ErrNoCredential", err)
	}
	if err := store.Delete(); err != nil {
		t.Errorf("double delete must be a no-op, got %v", err)
	}
}
