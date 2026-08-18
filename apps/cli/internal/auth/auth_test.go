package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

// goosWindows gates POSIX permission assertions off on Windows.
const goosWindows = "windows"

// wantKeyringName is the keyring backend's user-facing name, asserted and
// mimicked by the package's fakes.
const wantKeyringName = "OS keyring"

// Shape-valid test keys: prefix + 43 characters of the URL-safe base-64
// alphabet (the pinned 51-character wire format).
const (
	testKeyStored = "lv_test_storedvaluestoredvaluestoredvalue0123456789"
	testKeyEnv    = "lv_test_fromenvvaluefromenvvaluefromenvvalue0123456"
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
func (p *probeStore) Delete() (bool, error) { p.touched = true; return false, nil }
func (p *probeStore) Name() string          { return "probe" }

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestResolveEnvironmentIsHermetic(t *testing.T) {
	store := &probeStore{key: testKeyStored}
	key, source, err := Resolve(lookupFrom(map[string]string{EnvAPIKey: "  " + testKeyEnv + "  "}), store)
	if err != nil {
		t.Fatal(err)
	}
	if key != testKeyEnv {
		t.Errorf("key = %q, want the trimmed env value", key)
	}
	if source != SourceEnvironment {
		t.Errorf("source = %q, want environment", source)
	}
	if store.touched {
		t.Error("hermetic mode must not touch the credential store")
	}
}

// TestResolveEnvironmentBypassesWholeChain extends the hermetic probe through
// the real Chain: with QURL_API_KEY set, neither the keyring nor the file
// backend is consulted — storage is bypassed entirely, not merely unread.
func TestResolveEnvironmentBypassesWholeChain(t *testing.T) {
	keyringProbe := &probeStore{key: testKeyStored}
	fileProbe := &probeStore{key: testKeyStored}
	chain := NewChain(keyringProbe, fileProbe, func() {
		t.Error("hermetic mode must not report a file-fallback read")
	})

	key, source, err := Resolve(lookupFrom(map[string]string{EnvAPIKey: testKeyEnv}), chain)
	if err != nil {
		t.Fatal(err)
	}
	if key != testKeyEnv || source != SourceEnvironment {
		t.Errorf("key=%q source=%q, want the env value", key, source)
	}
	if keyringProbe.touched {
		t.Error("hermetic mode must not touch the keyring backend")
	}
	if fileProbe.touched {
		t.Error("hermetic mode must not touch the file backend")
	}
}

func TestResolveFallsBackToStore(t *testing.T) {
	store := &probeStore{key: testKeyStored}
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
	for name, key := range map[string]string{
		"live":              "lv_live_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
		"test":              testKeyStored,
		"url-safe alphabet": "lv_live_abc-def_ghij0123456789ABCDEFGHIJKLMNOPQRSTU",
	} {
		if len(key) != keyLength {
			t.Fatalf("%s: fixture is %d chars, want %d", name, len(key), keyLength)
		}
		if err := ValidateKeyShape(key); err != nil {
			t.Errorf("%s: valid key rejected: %v", name, err)
		}
	}
	for name, key := range map[string]string{
		"wrong prefix":  "at_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKL",
		"no prefix":     "abcdefghij0123456789abcdef",
		"too short":     "lv_live_abcdefghij0123456789",
		"too long":      testKeyStored + "x",
		"bad chars":     "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDE!!",
		"padding chars": "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDE==",
		"empty":         "",
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

	if err := store.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != testKeyStored {
		t.Errorf("Load = %q, want %q", got, testKeyStored)
	}

	if runtime.GOOS != goosWindows {
		info, err := os.Stat(filepath.Join(dir, "token"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("credential file mode = %o, want 0600", perm)
		}
	}

	removed, err := store.Delete()
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Delete of a stored key must report removal")
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("after delete: Load = %v, want ErrNoCredential", err)
	}
	removed, err = store.Delete()
	if err != nil || removed {
		t.Errorf("double delete must be a silent no-op, got removed=%v err=%v", removed, err)
	}
}

// TestFileStoreMatchesSDKFileCredentials is the compatibility pin: the file
// `qurl login` writes must be readable by qurl-go's FileCredentials — same
// path shape, same JSON document, same permissions. Rather than mirroring
// the SDK's parser by hand, this authorizes a request through the real SDK
// provider against the file the store just wrote.
//
// Skipped on Windows: the SDK's private-state reader enforces POSIX 0600
// permissions, which Windows file modes cannot express.
func TestFileStoreMatchesSDKFileCredentials(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("the SDK's credential reader enforces POSIX permissions")
	}
	dir := t.TempDir()
	store := NewFileStore(dir)
	if err := store.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}

	// TODO(upstream-contract): filepath must equal qurl-go's
	// UserIssuerStatePath (".config/qurl/token") relative to the config dir.
	provider := qurl.FileCredentials(filepath.Join(dir, "token"))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.layerv.ai/v1/me", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Authorize(context.Background(), req); err != nil {
		t.Fatalf("SDK FileCredentials rejected the store's file: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer "+testKeyStored; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestFileStoreDiagnosesForeignDocuments pins the read behavior for files
// this CLI did not write: raw text is a format error, and an
// authorization-style document from another LayerV tool is refused rather
// than misused.
func TestFileStoreDiagnosesForeignDocuments(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	path := filepath.Join(dir, "token")

	if err := os.WriteFile(path, []byte(testKeyStored+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("raw text file: err = %v, want ErrInvalidKey", err)
	}

	if err := os.WriteFile(path, []byte(`{"authorization":"Bearer something-else"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("authorization document: err = %v, want ErrInvalidKey", err)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("empty document: err = %v, want ErrNoCredential", err)
	}
}

// fakeKeyring is the unit-test keyring backend. state selects behavior:
// available (empty or holding a key) or unavailable (every call errors the
// way a missing D-Bus session or locked keychain does).
type fakeKeyring struct {
	key         string
	unavailable bool
	saves       int
}

var errKeyringDown = errors.New("no keyring daemon on this bus")

func (f *fakeKeyring) Name() string { return wantKeyringName }
func (f *fakeKeyring) Save(key string) error {
	if f.unavailable {
		return errKeyringDown
	}
	f.key = key
	f.saves++
	return nil
}
func (f *fakeKeyring) Load() (string, error) {
	if f.unavailable {
		return "", errKeyringDown
	}
	if f.key == "" {
		return "", ErrNoCredential
	}
	return f.key, nil
}
func (f *fakeKeyring) Delete() (bool, error) {
	if f.unavailable {
		return false, errKeyringDown
	}
	if f.key == "" {
		return false, nil
	}
	f.key = ""
	return true, nil
}

func TestChainLoadPrefersKeyring(t *testing.T) {
	warns := 0
	chain := NewChain(&fakeKeyring{key: testKeyStored}, &probeStore{key: "lv_test_wrongwrongwrongwrongwrongwrongwrongwrong012"}, func() { warns++ })

	key, backend, err := chain.LoadFrom()
	if err != nil {
		t.Fatal(err)
	}
	if key != testKeyStored || backend != BackendKeyring {
		t.Errorf("key=%q backend=%q, want the keyring's key", key, backend)
	}
	if warns != 0 {
		t.Errorf("keyring reads must not warn, warned %d times", warns)
	}
}

func TestChainEmptyKeyringIsAuthoritative(t *testing.T) {
	fileProbe := &probeStore{key: testKeyStored}
	chain := NewChain(&fakeKeyring{}, fileProbe, nil)

	_, _, err := chain.LoadFrom()
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
	if fileProbe.touched {
		t.Error("an available-but-empty keyring is authoritative; the file fallback must not be consulted")
	}
}

func TestChainUnavailableKeyringFallsBackToFileAndWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	file := NewFileStore(dir)
	if err := file.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	warns := 0
	chain := NewChain(&fakeKeyring{unavailable: true}, file, func() { warns++ })

	for i := 0; i < 3; i++ {
		key, backend, err := chain.LoadFrom()
		if err != nil {
			t.Fatal(err)
		}
		if key != testKeyStored || backend != BackendFile {
			t.Errorf("key=%q backend=%q, want the file fallback", key, backend)
		}
	}
	if warns != 1 {
		t.Errorf("file-fallback reads must warn exactly once per chain, warned %d times", warns)
	}

	// Unavailable keyring plus no file: the file store's guidance surfaces.
	empty := NewChain(&fakeKeyring{unavailable: true}, NewFileStore(t.TempDir()), nil)
	if _, _, err := empty.LoadFrom(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}

func TestChainSavePrefersKeyringAndClearsStaleFile(t *testing.T) {
	dir := t.TempDir()
	file := NewFileStore(dir)
	if err := file.Save("lv_test_stalestalestalestalestalestalestalestale012"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKeyring{}
	chain := NewChain(fake, file, nil)

	backend, err := chain.SaveTo(testKeyStored)
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendKeyring || fake.key != testKeyStored {
		t.Errorf("backend=%q keyring=%q, want the keyring to take the save", backend, fake.key)
	}
	if _, err := file.Load(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("stale fallback file must be cleared on a keyring save, Load = %v", err)
	}
}

func TestChainSaveFallsBackToFileWhenKeyringUnavailable(t *testing.T) {
	dir := t.TempDir()
	file := NewFileStore(dir)
	chain := NewChain(&fakeKeyring{unavailable: true}, file, nil)

	backend, err := chain.SaveTo(testKeyStored)
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendFile {
		t.Errorf("backend = %q, want file", backend)
	}
	got, err := file.Load()
	if err != nil || got != testKeyStored {
		t.Errorf("file fallback Load = %q, %v", got, err)
	}
	if runtime.GOOS != goosWindows {
		info, err := os.Stat(filepath.Join(dir, "token"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("fallback file mode = %o, want 0600", perm)
		}
	}
}

// TestChainRemoveAllClearsEveryBackend pins logout's storage contract: both
// backends are cleared, the ones that held a key are reported, repeating is a
// no-op, and an unavailable keyring counts as not-holding while the file is
// still cleared.
func TestChainRemoveAllClearsEveryBackend(t *testing.T) {
	dir := t.TempDir()
	file := NewFileStore(dir)
	if err := file.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKeyring{key: testKeyStored}
	chain := NewChain(fake, file, nil)

	removed, err := chain.RemoveAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != BackendKeyring || removed[1] != BackendFile {
		t.Errorf("removed = %v, want [keyring file]", removed)
	}
	if fake.key != "" {
		t.Error("keyring not cleared")
	}
	if _, err := file.Load(); !errors.Is(err, ErrNoCredential) {
		t.Error("file not cleared")
	}

	removed, err = chain.RemoveAll()
	if err != nil || len(removed) != 0 {
		t.Errorf("second RemoveAll = %v, %v; want the idempotent nothing", removed, err)
	}

	downFile := NewFileStore(t.TempDir())
	if err := downFile.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	down := NewChain(&fakeKeyring{unavailable: true}, downFile, nil)
	removed, err = down.RemoveAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != BackendFile {
		t.Errorf("removed = %v, want just the file when the keyring is unavailable", removed)
	}
}

func TestChainImplementsCredentialStore(t *testing.T) {
	var _ CredentialStore = NewChain(&fakeKeyring{}, &probeStore{}, nil)

	chain := NewChain(&fakeKeyring{}, NewFileStore(t.TempDir()), nil)
	if err := chain.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	key, err := chain.Load()
	if err != nil || key != testKeyStored {
		t.Errorf("Load = %q, %v", key, err)
	}
	removed, err := chain.Delete()
	if err != nil || !removed {
		t.Errorf("Delete = %v, %v; want removal", removed, err)
	}
}

// TestValidateKeyShapeFixtureLengthsAgree guards the fixtures above: every
// shape-valid literal in this file is exactly the pinned wire length, so a
// future format change cannot leave stale fixtures silently passing.
func TestValidateKeyShapeFixtureLengthsAgree(t *testing.T) {
	for _, key := range []string{testKeyStored, testKeyEnv} {
		if len(key) != keyLength {
			t.Errorf("fixture %q is %d chars, want %d", key, len(key), keyLength)
		}
		if !strings.HasPrefix(key, "lv_test_") {
			t.Errorf("fixture %q must carry the test prefix", key)
		}
	}
}
