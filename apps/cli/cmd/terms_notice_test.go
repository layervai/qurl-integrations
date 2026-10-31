package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestFirstPublishAndShareTermsNotice(t *testing.T) {
	for _, command := range []string{"publish", "share"} {
		for _, format := range []string{"text", "json", "quiet"} {
			t.Run(command+"/"+format, func(t *testing.T) {
				srv := apitest.NewServer(t)
				configDir := t.TempDir()
				args := []string{"--endpoint", srv.URL}
				if format == "json" {
					args = append(args, "-o", "json")
				}
				if format == "quiet" {
					args = append(args, "--quiet")
				}
				operand := "https://example.com/app"
				if command == "share" {
					operand = srv.Key.CRID
				}
				args = append(args, command, operand)
				for attempt := 0; attempt < 2; attempt++ {
					result := runCLI(t, &runOpts{args: args, configDir: configDir, firstTermsNotice: true})
					if result.code != 0 {
						t.Fatalf("exit %d: %s", result.code, result.stderr.String())
					}
					if format == "json" {
						var document map[string]any
						if err := json.Unmarshal(result.stdout.Bytes(), &document); err != nil {
							t.Fatal(err)
						}
						if attempt == 0 && document["terms_notice"] != termsNotice {
							t.Fatalf("missing JSON notice: %v", document)
						}
						if attempt == 1 && document["terms_notice"] != nil {
							t.Fatal("JSON notice repeated")
						}
						if strings.Contains(result.stderr.String(), termsNotice) {
							t.Fatal("JSON notice leaked to stderr")
						}
					} else {
						if got := strings.Count(result.stderr.String(), termsNotice); got != 1-attempt {
							t.Fatalf("notice count = %d", got)
						}
						if strings.Contains(result.stdout.String(), "Terms") {
							t.Fatal("notice polluted stdout")
						}
					}
				}
				if shown, err := connectorstate.TermsNoticeShown(filepath.Join(configDir, "connector-state"), termsVersion); err != nil || !shown {
					t.Fatalf("notice not saved: %v, %v", shown, err)
				}
			})
		}
	}
}

func TestFailedJSONPublishDoesNotConsumeTermsNotice(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusForbidden, "forbidden", "Forbidden", "not allowed")
	})
	dir := t.TempDir()
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "publish", "https://example.com/app"}, configDir: dir, firstTermsNotice: true})
	if result.code == 0 {
		t.Fatal("failed API publish succeeded")
	}
	if shown, err := connectorstate.TermsNoticeShown(filepath.Join(dir, "connector-state"), termsVersion); err != nil || shown {
		t.Fatalf("undelivered notice consumed: %v, %v", shown, err)
	}
}

func TestTermsNoticeStateFailureDoesNotFailPublish(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			srv := apitest.NewServer(t)
			dir := t.TempDir()
			if err := connectorstate.EnsureDirMode(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "terms-notice-version"), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--endpoint", srv.URL, "--quiet", "-o", format, "publish", "https://example.com/app"}
			result := runCLI(t, &runOpts{args: args, firstTermsNotice: true, shareStateDir: dir})
			if result.code != 0 {
				t.Fatalf("notice state blocked publish: %d %s", result.code, result.stderr.String())
			}
			if format == "text" {
				if result.stdout.String() != srv.Key.CRID+"\n" || !strings.Contains(result.stderr.String(), termsNotice) {
					t.Fatal("notice changed scripted text output")
				}
			} else {
				var document map[string]any
				if err := json.Unmarshal(result.stdout.Bytes(), &document); err != nil {
					t.Fatal(err)
				}
				if document["terms_notice"] != termsNotice {
					t.Fatal("notice state failure lost JSON notice")
				}
			}
		})
	}
}

func TestTermsNoticeJSONIsRecordedAtOutputRatherThanCommandExit(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	opts := &globalOpts{streams: &output.Streams{Out: &stdout, Err: &stderr}, resolvedFormat: output.FormatJSON, resolveShareStateDir: func(string) (string, error) { return dir, nil }}
	opts.beginTermsNotice()
	writeErr := errors.New("synthetic stdout failure")
	if err := opts.finishTermsNotice(writeErr); !errors.Is(err, writeErr) {
		t.Fatal("stdout error lost")
	}
	if shown, err := connectorstate.TermsNoticeShown(dir, termsVersion); err != nil || shown {
		t.Fatal("failed output consumed notice")
	}
	if err := opts.finishTermsNotice(opts.printer().Publish(&qurlapi.Published{ResourceID: "res-test", TargetURL: "https://example.test"})); err != nil {
		t.Fatal(err)
	}
	if shown, err := connectorstate.TermsNoticeShown(dir, termsVersion); err != nil || !shown {
		t.Fatal("successful output did not record notice immediately")
	}
	if opts.pendingTermsNotice != "" {
		t.Fatal("later result in this invocation would repeat notice")
	}
	if !strings.Contains(stdout.String(), "terms_notice") || stderr.Len() != 0 {
		t.Fatal("JSON notice used wrong stream")
	}
}

func TestTermsNoticeBrokenStderrIsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	reader, writer := io.Pipe()
	_ = reader.Close()
	defer func() { _ = writer.Close() }()
	opts := &globalOpts{streams: &output.Streams{Out: io.Discard, Err: writer}, resolvedFormat: output.FormatText, resolveShareStateDir: func(string) (string, error) { return dir, nil }}
	opts.beginTermsNotice()
	if shown, err := connectorstate.TermsNoticeShown(dir, termsVersion); err != nil || shown {
		t.Fatal("undelivered stderr notice was recorded")
	}
}

func TestFirstLocalBackgroundPublishCarriesTermsNotice(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := foregroundLifecycleStateDir(t, "qurl-terms-bg-")
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "on", 1, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 1, "serving"))
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "publish", "http://127.0.0.1:3000"}, firstTermsNotice: true, shareRegistry: registry, shareStateDir: stateDir, shareDaemon: &recordingShareDaemon{}, preflightTarget: func(context.Context, string, int) error { return nil }, localResource: resolvedLocalResource(srv, false)})
	if res.code != 0 {
		t.Fatalf("local publish = %d %s", res.code, res.stderr.String())
	}
	var document map[string]any
	if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document["terms_notice"] != termsNotice {
		t.Fatal("local JSON lost notice")
	}
	if shown, err := connectorstate.TermsNoticeShown(stateDir, termsVersion); err != nil || !shown {
		t.Fatal("local notice not recorded")
	}
}
