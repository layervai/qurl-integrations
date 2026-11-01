//go:build unix

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestAccountAPIKeyModeUsesExactPrivateFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "account-key")
	if err := os.WriteFile(file, []byte(testAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
			t.Error("wrong private-file authority")
		}
		if r.URL.Path == "/v1/me" {
			_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"api_key","scopes":["qurl:read"]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
		g.configDir = t.TempDir()
		g.lookupEnv = func(name string) (string, bool) {
			if name == auth.EnvAPIKeyFile {
				return file, true
			}
			return "", false
		}
		g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
			t.Fatal("file mode opened browser")
			return "", nil
		}
	})
	root.SetArgs([]string{"--endpoint", server.URL, "webhooks", "events", "--api-key-auth", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "[]\n" {
		t.Fatal("lost file-authorized catalog")
	}
}
