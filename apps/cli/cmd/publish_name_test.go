package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// sentPublishNames returns the name member of every create request.
func sentPublishNames(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	var names []string
	for _, req := range srv.Requests() {
		if req.Method != "POST" || req.Path != "/v1/resources" {
			continue
		}
		var body struct {
			Name *string `json:"name"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Name == nil {
			names = append(names, "<absent>")
		} else {
			names = append(names, *body.Name)
		}
	}
	return names
}

func TestPublishRemoteSendsTheTrimmedName(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "publish", "https://example.com/data", "--name", "  Reports API  "}})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if got := sentPublishNames(t, srv); len(got) != 1 || got[0] != "Reports API" {
		t.Fatalf("sent names = %q", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil || doc["name"] != "Reports API" {
		t.Fatalf("document = %s (%v)", res.stdout.String(), err)
	}
}

func TestPublishNameIsRequiredBeforeAnyRequest(t *testing.T) {
	for name, args := range map[string][]string{
		"remote without --name": {"publish", "https://example.com/data"},
		"empty --name":          {"publish", "https://example.com/data", "--name", " "},
		"two lines":             {"publish", "https://example.com/data", "--name", "a\nb"},
		"too long":              {"publish", "https://example.com/data", "--name", strings.Repeat("n", 101)},
		"format character":      {"publish", "https://example.com/data", "--name", "a\u200bb"},
		"line separator":        {"publish", "https://example.com/data", "--name", "a\u2028b"},
		"invalid UTF-8":         {"publish", "https://example.com/data", "--name", "a\xffb"},
		"empty local --name":    {"publish", "http://127.0.0.1:3000", "--name", ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, args...)})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), "--name") {
				t.Fatalf("exit %d: %s", res.code, res.stderr.String())
			}
			if reqs := srv.Requests(); len(reqs) != 0 {
				t.Fatalf("sent %d requests before refusing", len(reqs))
			}
		})
	}
}

func TestPublishRemoteAsksForANameInATerminal(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "publish", "https://example.com/data"},
		stdin: strings.NewReader("Typed name\n"), inTTY: true, tty: true,
	})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), msgPublishNamePrompt) {
		t.Fatalf("no prompt: %q", res.stderr.String())
	}
	if got := sentPublishNames(t, srv); len(got) != 1 || got[0] != "Typed name" {
		t.Fatalf("sent names = %q", got)
	}
}

func TestPublishNameDefaultsForALocalApp(t *testing.T) {
	local := &publishTarget{kind: publishTargetLocal}
	newCmd := func(args ...string) *cobra.Command {
		cmd := &cobra.Command{}
		cmd.Flags().String("name", "", "")
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	var stderr bytes.Buffer
	opts := &globalOpts{
		lookupEnv: func(string) (string, bool) { return "", false },
		streams:   &output.Streams{Out: io.Discard, Err: &stderr, ErrIsTTY: true},
	}
	got, err := publishName(opts, newCmd(), "", "my-dashboard", local)
	if err != nil || got != "my-dashboard" {
		t.Fatalf("--id default = %q, %v", got, err)
	}
	if want := fmt.Sprintf(msgPublishNameDefault, "my-dashboard"); !strings.Contains(stderr.String(), want) {
		t.Fatalf("a terminal is not shown the default: %q", stderr.String())
	}
	t.Chdir(t.TempDir())
	dir, _ := filepath.Abs(".")
	if got, err = publishName(opts, newCmd(), "", "", local); err != nil || got != filepath.Base(dir) {
		t.Fatalf("directory default = %q, %v", got, err)
	}
	if got, err = publishName(opts, newCmd("--name", "Mine"), "Mine", "my-dashboard", local); err != nil || got != "Mine" {
		t.Fatalf("--name = %q, %v", got, err)
	}
}

func TestDirectoryPublishNameSkipsHomeAndRoot(t *testing.T) {
	at := func(dir string) func() (string, error) { return func() (string, error) { return dir, nil } }
	home := at(filepath.FromSlash("/Users/ana"))
	for dir, want := range map[string]string{
		filepath.FromSlash("/Users/ana/projects/tipsplit"): "tipsplit",
		filepath.FromSlash("/Users/ana/projects/ spaced "): "spaced",
		filepath.FromSlash("/Users/ana"):                   "",
		string(filepath.Separator):                         "",
	} {
		if got := directoryPublishName(at(dir), home); got != want {
			t.Errorf("directoryPublishName(%q) = %q, want %q", dir, got, want)
		}
	}
	failing := func() (string, error) { return "", errors.New("gone") }
	if got := directoryPublishName(failing, home); got != "" {
		t.Errorf("unreadable directory = %q", got)
	}
}

func TestPublishFlagErrorsComeBeforeTheNamePrompt(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "publish", "https://example.com/data", "--foreground"},
		stdin: strings.NewReader("never read\n"), inTTY: true, tty: true,
	})
	if res.code != exitcode.Usage || strings.Contains(res.stderr.String(), msgPublishNamePrompt) {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
}

func TestValidPublicNameRefusesSurroundingSpace(t *testing.T) {
	for _, name := range []string{" a", "a ", "\ta"} {
		if validPublicName(name) {
			t.Errorf("validPublicName(%q) = true", name)
		}
	}
}
