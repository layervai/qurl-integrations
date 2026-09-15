package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
)

// Use real issuer signatures and the production deployment-file verifier. Only
// the API, browser launch, and authenticated access grant are local test seams.
func TestCRIDBindingBeforeShareBrowserAndDownload(t *testing.T) {
	signer, err := qurl.GenerateLocalSigner("binding-test")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := json.Marshal(qurl.Deployment{
		Issuers: []qurl.ManifestIssuer{{Kid: signer.KID(), SPKIDERB64: base64.RawURLEncoding.EncodeToString(issuer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, deployment, 0o600); err != nil {
		t.Fatal(err)
	}
	opener := &consume.AccessOpener{LookupEnv: func(name string) (string, bool) { return path, name == qurl.EnvDeploymentPath }}
	for _, mode := range []string{"share", "browser", "file", "stdout"} {
		for _, kind := range []string{"match", "foreign-key", "unsigned", "wrong-issuer"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				valid := kind == "match"
				srv := apitest.NewServer(t)
				key := srv.Key.DER
				if kind == "foreign-key" {
					key = apitest.GenerateResourceKey(t).DER
				}
				linkSigner := signer
				if kind == "wrong-issuer" {
					linkSigner, err = qurl.GenerateLocalSigner(signer.KID())
					if err != nil {
						t.Fatal(err)
					}
				}
				link, err := qurl.CreatePortalWithParams(t.Context(), linkSigner, qurl.CreateParams{
					CellPublicKey: bytes.Repeat([]byte{4}, 32), ResourcePublicKey: key,
					RelayURL: "https://relay.example.com", JTI: "binding_test",
					IssuedAt: 1700000000, NotBefore: 1700000000, Expiry: 1700003600,
				})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "unsigned" {
					link = srv.URL + apitest.DownloadPath
				}
				srv.SetShareQURL(link) // The API still echoes the requested CRID.
				browser := &fakeBrowser{}
				dest := filepath.Join(t.TempDir(), "content")
				args := []string{"--endpoint", srv.URL, "get", srv.Key.CRID}
				switch mode {
				case "share":
					args[2] = "share"
				case "file":
					args = append(args, "--file", dest)
				case "stdout":
					args = append(args, "--file", "-")
				}
				grants := 0
				result := runCLI(t, &runOpts{args: args, tty: mode == "browser", browser: browser,
					verifyLink: opener.Verify,
					enterPortalGrant: func(context.Context, string) (consume.AccessGrant, error) {
						grants++
						return consume.AccessGrant{ContentURL: srv.URL + apitest.DownloadPath, OpenSeconds: 300, AuthorizeContentRequest: func(*http.Request) error { return nil }}, nil
					},
				})
				if valid {
					if result.code != 0 {
						t.Fatalf("matching link failed: %s", result.stderr.String())
					}
					if mode == "file" && string(readTestFile(t, dest)) != apitest.DefaultDownloadPayload {
						t.Fatal("wrong download")
					}
					if mode == "share" && strings.TrimSpace(result.stdout.String()) != link {
						t.Fatal("matching share link not printed")
					}
					if mode == "stdout" && result.stdout.String() != apitest.DefaultDownloadPayload {
						t.Fatal("matching payload not streamed")
					}
					if mode == "browser" && (len(browser.opened) != 1 || browser.opened[0] != link) {
						t.Fatal("matching link not opened")
					}
					return
				}
				if result.code != 12 {
					t.Fatalf("exit=%d: %s", result.code, result.stderr.String())
				}
				if result.stdout.Len() != 0 || grants != 0 || len(browser.opened) != 0 {
					t.Fatal("acted on a foreign-resource link")
				}
				mustNotExistCmd(t, dest)
				mustNotExistCmd(t, dest+".part")
				for _, request := range srv.Requests() {
					if request.Path == apitest.DownloadPath {
						t.Fatal("download requested before verification")
					}
				}
			})
		}
	}
}
