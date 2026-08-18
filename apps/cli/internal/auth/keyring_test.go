package auth

import (
	"errors"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

// fakeFuncs builds keyringFuncs over an in-memory map, optionally failing
// every call with err (the unavailability mode).
func fakeFuncs(store map[string]string, err error) keyringFuncs {
	key := func(service, user string) string { return service + "\x00" + user }
	return keyringFuncs{
		set: func(service, user, password string) error {
			if err != nil {
				return err
			}
			store[key(service, user)] = password
			return nil
		},
		get: func(service, user string) (string, error) {
			if err != nil {
				return "", err
			}
			v, ok := store[key(service, user)]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return v, nil
		},
		del: func(service, user string) error {
			if err != nil {
				return err
			}
			if _, ok := store[key(service, user)]; !ok {
				return keyring.ErrNotFound
			}
			delete(store, key(service, user))
			return nil
		},
	}
}

// TestKeyringStoreRoundTrip drives the store over fake library functions:
// hit, miss (ErrNoCredential), delete-reports-removal, idempotent re-delete.
func TestKeyringStoreRoundTrip(t *testing.T) {
	store := &keyringStore{fns: fakeFuncs(map[string]string{}, nil), service: "test-service"}

	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty keyring Load = %v, want ErrNoCredential", err)
	}

	if err := store.Save(testKeyStored); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got != testKeyStored {
		t.Fatalf("Load = %q, %v", got, err)
	}

	removed, err := store.Delete()
	if err != nil || !removed {
		t.Fatalf("Delete = %v, %v; want removal", removed, err)
	}
	removed, err = store.Delete()
	if err != nil || removed {
		t.Errorf("re-delete must be a silent no-op, got removed=%v err=%v", removed, err)
	}
	if store.Name() != wantKeyringName {
		t.Errorf("Name = %q", store.Name())
	}
}

// TestKeyringStoreUnavailability pins the classification contract Chain keys
// on: library failures other than not-found are NOT ErrNoCredential (Load)
// and are surfaced as errors (Save/Delete).
func TestKeyringStoreUnavailability(t *testing.T) {
	down := errors.New("dbus: session bus not available")
	store := &keyringStore{fns: fakeFuncs(nil, down), service: "test-service"}

	_, err := store.Load()
	if err == nil || errors.Is(err, ErrNoCredential) {
		t.Errorf("unavailable Load = %v; must not read as the empty-keyring signal", err)
	}
	if !errors.Is(err, down) {
		t.Errorf("Load must keep the cause reachable, got %v", err)
	}
	if err := store.Save(testKeyStored); !errors.Is(err, down) {
		t.Errorf("Save = %v, want the library failure surfaced", err)
	}
	if removed, err := store.Delete(); removed || !errors.Is(err, down) {
		t.Errorf("Delete = %v, %v; want the library failure surfaced", removed, err)
	}
}

// TestNewStoreBuildsTheProductionChain proves the production constructor
// wires a usable chain (construction never touches the OS keyring; only
// operations do, and this test performs none against it).
func TestNewStoreBuildsTheProductionChain(t *testing.T) {
	chain := NewStore(t.TempDir(), nil)
	if chain == nil || chain.Name() != "credential store" {
		t.Fatalf("NewStore chain = %+v", chain)
	}
	if NewKeyringStore().Name() != wantKeyringName {
		t.Error("NewKeyringStore must name the OS keyring backend")
	}
}
