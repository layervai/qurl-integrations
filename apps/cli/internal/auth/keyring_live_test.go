package auth

import (
	"errors"
	"os"
	"testing"
)

// TestKeyringLiveSmoke exercises the real OS keyring — wincred on Windows,
// the Keychain on macOS, gnome-keyring over D-Bus on Linux — with one
// set/get/delete of a throwaway value. It runs only under the CI matrix
// harness (QURL_TEST_HARNESS=1, exported by cli.yml's `cli / matrix` job,
// whose keychain-unlock and gnome-keyring-under-dbus setup steps arm
// themselves when apps/cli references a keyring). Everything behavioral is
// asserted against fakes and the file fallback elsewhere; this smoke only
// proves the platform glue actually reaches a live keyring on all three
// OSes.
//
// The throwaway value uses a dedicated service name so a developer opting in
// locally (QURL_TEST_HARNESS=1 go test ./...) cannot clobber a real stored
// credential.
func TestKeyringLiveSmoke(t *testing.T) {
	if os.Getenv("QURL_TEST_HARNESS") != "1" {
		t.Skip("live OS-keyring smoke runs only under the CI matrix harness (QURL_TEST_HARNESS=1)")
	}

	store := &keyringStore{fns: osKeyringFuncs(), service: "qURL CLI live-smoke"}
	const throwaway = "lv_test_livesmokelivesmokelivesmokelivesmoke0123456"

	t.Cleanup(func() {
		// Best-effort: never leave the throwaway item behind.
		_, _ = store.Delete()
	})

	if err := store.Save(throwaway); err != nil {
		t.Fatalf("live keyring Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("live keyring Load: %v", err)
	}
	if got != throwaway {
		t.Fatalf("live keyring round-trip = %q, want the throwaway value", got)
	}
	removed, err := store.Delete()
	if err != nil || !removed {
		t.Fatalf("live keyring Delete = %v, %v; want removal", removed, err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("after delete: Load = %v, want ErrNoCredential", err)
	}
}
