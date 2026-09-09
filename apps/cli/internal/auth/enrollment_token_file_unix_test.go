//go:build unix

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExternalEnrollmentToken(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadExternalEnrollmentTokenFileAcceptsExactPrivateFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		data string
	}{
		{name: "read only", mode: 0o400, data: "enrollment-value"},
		{name: "owner writable", mode: 0o600, data: "enrollment-value\n"},
		{name: "crlf", mode: 0o400, data: "enrollment-value\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeExternalEnrollmentToken(t, []byte(tc.data), tc.mode)
			got, err := ReadExternalEnrollmentTokenFile(path)
			if err != nil || got != "enrollment-value" {
				t.Fatalf("ReadExternalEnrollmentTokenFile() = (%q, %v)", got, err)
			}
		})
	}
}

func TestReadExternalEnrollmentTokenFileRejectsUnsafePathAndMetadata(t *testing.T) {
	valid := writeExternalEnrollmentToken(t, []byte("enrollment-value"), 0o400)
	for name, path := range map[string]string{
		"relative": filepath.Base(valid),
		"unclean":  filepath.Dir(valid) + string(filepath.Separator) + "child" + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(valid),
		"padded":   " " + valid,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadExternalEnrollmentTokenFile(path); err == nil {
				t.Fatal("unsafe token path was accepted")
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "token-link")
		if err := os.Symlink(valid, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadExternalEnrollmentTokenFile(link); err == nil {
			t.Fatal("symlink token was accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ReadExternalEnrollmentTokenFile(t.TempDir()); err == nil {
			t.Fatal("directory token was accepted")
		}
	})
	for _, mode := range []os.FileMode{0o000, 0o100, 0o200, 0o440, 0o600 | os.ModeSetuid, 0o601, 0o700} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			path := writeExternalEnrollmentToken(t, []byte("enrollment-value"), mode)
			if _, err := ReadExternalEnrollmentTokenFile(path); err == nil {
				t.Fatalf("unsafe token mode %04o was accepted", mode)
			}
		})
	}
}

func TestReadExternalEnrollmentTokenFileRejectsUnsafeBytesWithoutDisclosure(t *testing.T) {
	secret := "do-not-disclose-this-enrollment-token"
	for name, data := range map[string][]byte{
		"empty":              {},
		"embedded space":     []byte(secret + " with-space"),
		"second line ending": []byte(secret + "\n\n"),
		"bare carriage":      []byte(secret + "\r"),
		"invalid utf8":       {0xff, 0xfe},
		"oversize":           []byte(strings.Repeat("x", externalEnrollmentTokenMaxBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeExternalEnrollmentToken(t, data, 0o400)
			_, err := ReadExternalEnrollmentTokenFile(path)
			if err == nil {
				t.Fatal("unsafe token bytes were accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("token leaked in error: %v", err)
			}
		})
	}
}

func TestReadExternalEnrollmentTokenFileRejectsPathReplacement(t *testing.T) {
	path := writeExternalEnrollmentToken(t, []byte("first-value"), 0o400)
	replacement := writeExternalEnrollmentToken(t, []byte("second-value"), 0o400)
	original := statExternalEnrollmentTokenPath
	t.Cleanup(func() { statExternalEnrollmentTokenPath = original })
	statExternalEnrollmentTokenPath = func(name string) (os.FileInfo, error) {
		if err := os.Rename(replacement, name); err != nil {
			return nil, err
		}
		return os.Lstat(name)
	}
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestReadExternalEnrollmentTokenFileRejectsWrongOwnerWhenPrivileged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing fixture ownership requires root")
	}
	path := writeExternalEnrollmentToken(t, []byte("enrollment-value"), 0o400)
	if err := os.Chown(path, 1, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil {
		t.Fatal("foreign-owned token was accepted")
	}
	if err := os.Chown(path, 0, -1); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// TestReadExternalEnrollmentTokenFileRejectsSymlinkSwap pins the descriptor
// pinning against the classic swap: the path becomes a symlink to another
// private token between the no-follow open and the re-inspection. The
// opened descriptor is refused rather than read, and neither value is named.
func TestReadExternalEnrollmentTokenFileRejectsSymlinkSwap(t *testing.T) {
	path := writeExternalEnrollmentToken(t, []byte("first-secret-value"), 0o400)
	other := writeExternalEnrollmentToken(t, []byte("second-secret-value"), 0o400)
	original := statExternalEnrollmentTokenPath
	t.Cleanup(func() { statExternalEnrollmentTokenPath = original })
	statExternalEnrollmentTokenPath = func(name string) (os.FileInfo, error) {
		if err := os.Remove(name); err != nil {
			return nil, err
		}
		if err := os.Symlink(other, name); err != nil {
			return nil, err
		}
		return os.Lstat(name)
	}
	_, err := ReadExternalEnrollmentTokenFile(path)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("symlink swap error = %v, want the changed-while-opening refusal", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("token leaked in error: %v", err)
	}
}
