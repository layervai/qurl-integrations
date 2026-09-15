package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goosWindows = "windows"

const (
	testKeyStored = "lv_test_storedvaluestoredvaluestoredvalue0123456789"
	testKeyEnv    = "lv_test_fromenvvaluefromenvvaluefromenvvalue0123456"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestResolveEnvironmentIsHermetic(t *testing.T) {
	key, source, err := Resolve(lookupFrom(map[string]string{EnvAPIKey: "  " + testKeyEnv + "  "}))
	if err != nil {
		t.Fatal(err)
	}
	if key != testKeyEnv {
		t.Errorf("key = %q, want the trimmed env value", key)
	}
	if source != SourceEnvironment {
		t.Errorf("source = %q, want environment", source)
	}
}

func TestResolveEnvironmentFileIsStrictPrivateAndHermetic(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "api-key")
	if err := os.WriteFile(path, []byte(testKeyEnv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectAPIKeyTestFile(t, path)
	key, source, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: path}))
	if err != nil || key != testKeyEnv || source != SourceEnvironmentFile {
		t.Fatalf("file credential = %q %q %v", key, source, err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: path})); err != nil {
		t.Fatalf("0400 file rejected: %v", err)
	}
	if _, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKey: testKeyStored, EnvAPIKeyFile: path})); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("inline and file conflict = %v", err)
	}
	key, source, err = Resolve(lookupFrom(map[string]string{EnvAPIKey: " ", EnvAPIKeyFile: path}))
	if err != nil || key != testKeyEnv || source != SourceEnvironmentFile {
		t.Fatalf("blank inline with file credential = %q %q %v", key, source, err)
	}

	crlfPath := filepath.Join(directory, "api-key-crlf")
	if err := os.WriteFile(crlfPath, []byte(testKeyEnv+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectAPIKeyTestFile(t, crlfPath)
	key, source, err = Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: crlfPath}))
	if err != nil || key != testKeyEnv || source != SourceEnvironmentFile {
		t.Fatalf("CRLF file credential = %q %q %v", key, source, err)
	}
}

func TestResolveEmptyEnvironmentFileIsUnset(t *testing.T) {
	for name, fileValue := range map[string]string{
		"empty":      "",
		"whitespace": " \t ",
	} {
		t.Run(name+" with inline key", func(t *testing.T) {
			key, source, err := Resolve(lookupFrom(map[string]string{
				EnvAPIKey: testKeyEnv, EnvAPIKeyFile: fileValue,
			}))
			if err != nil || key != testKeyEnv || source != SourceEnvironment {
				t.Fatalf("credential = %q %q %v", key, source, err)
			}
		})
		t.Run(name+" without key", func(t *testing.T) {
			_, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: fileValue}))
			if !errors.Is(err, ErrNoCredential) {
				t.Fatalf("credential error = %v, want ErrNoCredential", err)
			}
		})
	}
}

func TestResolveEnvironmentFileRejectsAuthorityMutationUnion(t *testing.T) {
	directory := t.TempDir()
	base := filepath.Join(directory, "base")
	if err := os.WriteFile(base, []byte(testKeyEnv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		body []byte
		mode os.FileMode
	}{
		{"missing LF", []byte(testKeyEnv), 0o600},
		{"double LF", []byte(testKeyEnv + "\n\n"), 0o600},
		{"bare CR", []byte(testKeyEnv + "\r"), 0o600},
		{"double CRLF", []byte(testKeyEnv + "\r\n\r\n"), 0o600},
		{"leading space", []byte(" " + testKeyEnv + "\n"), 0o600},
		{"trailing space", []byte(testKeyEnv + " \n"), 0o600},
		{"tab", []byte(testKeyEnv + "\t\n"), 0o600},
		{"embedded control", append([]byte(testKeyEnv[:20]), append([]byte{0}, []byte(testKeyEnv[20:]+"\n")...)...), 0o600},
		{"group readable", []byte(testKeyEnv + "\n"), 0o440},
		{"world readable", []byte(testKeyEnv + "\n"), 0o644},
		{"no permissions", []byte(testKeyEnv + "\n"), 0o000},
		{"owner write only", []byte(testKeyEnv + "\n"), 0o200},
		{"owner executable", []byte(testKeyEnv + "\n"), 0o700},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if runtime.GOOS == goosWindows && mutation.mode != 0o600 {
				t.Skip("POSIX mode mutation is a Unix contract")
			}
			path := filepath.Join(directory, strings.ReplaceAll(mutation.name, " ", "-"))
			if err := os.WriteFile(path, mutation.body, mutation.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mutation.mode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: path})); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("mutation = %v", err)
			}
		})
	}
	for name, path := range map[string]string{
		"relative": "api-key",
		"symlink":  filepath.Join(directory, "symlink"),
		"hardlink": filepath.Join(directory, "hardlink"),
	} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "symlink":
				if err := os.Symlink(base, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "hardlink":
				if err := os.Link(base, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: path})); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("mutation = %v", err)
			}
		})
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	_, _, err := Resolve(lookupFrom(nil))
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
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
	if err := ValidateKeyShape(testKeyStored + "xY7_-z"); err != nil {
		t.Errorf("longer-than-today's-mint key rejected: %v", err)
	}
	for name, key := range map[string]string{
		"wrong prefix":  "at_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKL",
		"no prefix":     "abcdefghij0123456789abcdef",
		"too short":     "lv_live_abcdefghij0123456789",
		"truncated":     testKeyStored[:keyLength-1],
		"bad chars":     "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDE!!",
		"padding chars": "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDE==",
		"empty":         "",
	} {
		if err := ValidateKeyShape(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
}

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
