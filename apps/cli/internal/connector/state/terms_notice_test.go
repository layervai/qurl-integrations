package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTermsNoticeVersionUsesPrivateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if shown, err := TermsNoticeShown(dir, "2.0"); err != nil || shown {
		t.Fatalf("fresh = %v, %v", shown, err)
	}
	for _, version := range []string{"2.0", "3.0"} {
		if err := MarkTermsNoticeShown(dir, version); err != nil {
			t.Fatal(err)
		}
		if shown, err := TermsNoticeShown(dir, version); err != nil || !shown {
			t.Fatalf("recorded = %v, %v", shown, err)
		}
		if shown, err := TermsNoticeShown(dir, "next"); err != nil || shown {
			t.Fatalf("new version = %v, %v", shown, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, termsNoticeFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConnectorResourceFile(filepath.Join(dir, termsNoticeFile), info); err != nil {
		t.Fatal(err)
	}
	if err := MarkTermsNoticeShown(dir, "bad\nversion"); err == nil {
		t.Fatal("invalid version accepted")
	}
}

func TestTermsNoticeRefusesUnsafeMarker(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, termsNoticeFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := TermsNoticeShown(dir, "2.0"); err == nil {
		t.Fatal("directory accepted as notice state")
	}
	if err := MarkTermsNoticeShown(dir, "2.0"); err == nil {
		t.Fatal("unsafe notice state overwritten")
	}
}

func TestTermsNoticeDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, termsNoticeFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := MarkTermsNoticeShown(dir, "2.0"); err == nil {
		t.Fatal("symlink accepted")
	}
	body, err := os.ReadFile(target) // #nosec G304 -- this test created the fixed temporary target.
	if err != nil || string(body) != "unchanged" {
		t.Fatal("external file changed")
	}
}
