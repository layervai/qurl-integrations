package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type fakeFileInfo struct {
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return "stdin" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func withTokenInputHooks(t *testing.T, mode os.FileMode, password []byte, passwordErr error) {
	t.Helper()
	origStat := statStdin
	origRead := readPassword
	statStdin = func() (os.FileInfo, error) { return fakeFileInfo{mode: mode}, nil }
	readPassword = func(int) ([]byte, error) { return password, passwordErr }
	t.Cleanup(func() {
		statStdin = origStat
		readPassword = origRead
	})
}

func TestReadTokenInputSources(t *testing.T) {
	t.Run("argument wins", func(t *testing.T) {
		withTokenInputHooks(t, 0, nil, nil)
		token, err := readToken(&cobra.Command{}, []string{"at_arg"})
		if err != nil {
			t.Fatalf("readToken arg: %v", err)
		}
		if token != "at_arg" {
			t.Errorf("token = %q, want at_arg", token)
		}
	})

	t.Run("piped stdin trims whitespace", func(t *testing.T) {
		withTokenInputHooks(t, 0, nil, nil)
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("  at_stdin  \n"))
		token, err := readToken(cmd, nil)
		if err != nil {
			t.Fatalf("readToken stdin: %v", err)
		}
		if token != "at_stdin" {
			t.Errorf("token = %q, want at_stdin", token)
		}
	})

	t.Run("empty piped stdin errors", func(t *testing.T) {
		withTokenInputHooks(t, 0, nil, nil)
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("\n"))
		if _, err := readToken(cmd, nil); err == nil {
			t.Fatal("expected empty stdin error")
		}
	})

	t.Run("interactive hidden input trims", func(t *testing.T) {
		withTokenInputHooks(t, os.ModeCharDevice, []byte(" at_hidden \n"), nil)
		cmd := &cobra.Command{}
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		token, err := readToken(cmd, nil)
		if err != nil {
			t.Fatalf("readToken interactive: %v", err)
		}
		if token != "at_hidden" {
			t.Errorf("token = %q, want at_hidden", token)
		}
		if !strings.Contains(stderr.String(), "Access token:") {
			t.Errorf("expected prompt on stderr, got %q", stderr.String())
		}
	})

	t.Run("interactive read error wraps", func(t *testing.T) {
		withTokenInputHooks(t, os.ModeCharDevice, nil, errors.New("boom"))
		cmd := &cobra.Command{}
		cmd.SetErr(&bytes.Buffer{})
		_, err := readToken(cmd, nil)
		if err == nil || !strings.Contains(err.Error(), "read token: boom") {
			t.Fatalf("expected wrapped read error, got %v", err)
		}
	})
}
