package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
)

// Regression for Desktop #541: failure must retain confirmed identity, not a share link.
// This exercises a confirmed create followed by native setup failure, not mint failure.
func TestFirstShareFailureReturnsKnownIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		flags    []string
		existing *bool
	}{
		{name: "private default", existing: new(false)},
		{name: "explicit public", flags: []string{"--public"}, existing: new(false)},
		{name: "existing resource", existing: new(true)},
		{name: "unknown provenance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			if test.existing == nil {
				srv.OmitPublishFoundExisting()
			} else {
				srv.SetPublishFoundExisting(*test.existing)
			}
			failure := errors.New("synthetic native setup failure after confirmed create")
			resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
				if _, err := resolveID("agent-one"); err != nil {
					return nil, err
				}
				return nil, failure
			}
			flags := append([]string{"--name", "Recovery fixture", "-o", "json"}, test.flags...)
			result := runCLI(t, localPublish(t, srv, resolver, flags...))
			if result.code == 0 || !strings.Contains(result.stderr.String(), failure.Error()) || len(createRequests(t, srv)) != 1 {
				t.Fatal("reproducer did not reach confirmed creation followed by the intended failure")
			}
			var receipt struct {
				CRID          string `json:"crid"`
				ResourceID    string `json:"resource_id"`
				Error         string `json:"error"`
				FoundExisting *bool  `json:"found_existing"`
			}
			if json.Unmarshal(result.stdout.Bytes(), &receipt) != nil || receipt.CRID != srv.Key.CRID || receipt.ResourceID != srv.Key.ResourceID || receipt.Error != "publication_setup_failed" {
				t.Fatalf("confirmed create followed by setup failure: exit=%d, stdoutBytes=%d; created identity is unavailable to caller", result.code, result.stdout.Len())
			}
			if (test.existing == nil) != (receipt.FoundExisting == nil) || (test.existing != nil && *test.existing != *receipt.FoundExisting) {
				t.Fatal("creation provenance changed")
			}
			var document map[string]any
			if err := json.Unmarshal(result.stdout.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"link", "qurl", "qurl_link", "resource_url", "target_url", "status"} {
				if _, ok := document[key]; ok {
					t.Fatalf("failed setup offered %s", key)
				}
			}
			for _, request := range srv.Requests() {
				if request.Method == http.MethodDelete {
					t.Fatal("failure deleted a resource")
				}
			}
		})
	}
}

func TestFirstShareFailureBeforeConfirmedCreateHasNoReceipt(t *testing.T) {
	for _, beforeCreate := range []bool{false, true} {
		t.Run(strconv.FormatBool(beforeCreate), func(t *testing.T) {
			srv := apitest.NewServer(t)
			resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
				if beforeCreate {
					return nil, errors.New("synthetic pre-create failure")
				}
				_, err := resolveID("agent-one")
				return nil, err
			}
			if !beforeCreate {
				srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"crid": "invalid", "resource_id": srv.Key.ResourceID, "private": true}, nil)
				})
			}
			result := runCLI(t, localPublish(t, srv, resolver, "-o", "json"))
			if result.code == 0 || result.stdout.Len() != 0 {
				t.Fatal("unconfirmed creation produced a receipt or success")
			}
		})
	}
}

func TestFirstShareSetupFailureKeepsTextAndQuietStdoutEmpty(t *testing.T) {
	for _, flags := range [][]string{nil, {"--quiet"}} {
		srv := apitest.NewServer(t)
		resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
			if _, err := resolveID("agent-one"); err != nil {
				return nil, err
			}
			return nil, errors.New("synthetic setup failure")
		}
		result := runCLI(t, localPublish(t, srv, resolver, flags...))
		if result.code == 0 || result.stdout.Len() != 0 || !strings.Contains(result.stderr.String(), srv.Key.CRID) {
			t.Fatal("text/quiet setup failure lost recovery hint or printed success data")
		}
	}
}

func TestFirstShareDaemonFailureKeepsKnownIdentity(t *testing.T) {
	srv := apitest.NewServer(t)
	opts := servingLocalPublish(t, srv, false, "-o", "json")
	opts.shareDaemon = &recordingShareDaemon{ensureErr: errors.New("synthetic daemon failure")}
	result := runCLI(t, opts)
	var document map[string]any
	if result.code == 0 || json.Unmarshal(result.stdout.Bytes(), &document) != nil || document["error"] != "publication_setup_failed" || document["crid"] != srv.Key.CRID {
		t.Fatal("daemon setup failure did not report the known resource")
	}
}

func TestFirstShareResourceMismatchRecoversOnlyConfirmedCreate(t *testing.T) {
	srv := apitest.NewServer(t)
	other := apitest.NewServer(t)
	resolver := resolvedLocalResource(other, true)
	result := runCLI(t, localPublish(t, srv, resolver, "-o", "json"))
	var receipt map[string]any
	if result.code == 0 || json.Unmarshal(result.stdout.Bytes(), &receipt) != nil || receipt["crid"] != srv.Key.CRID || receipt["resource_id"] != srv.Key.ResourceID || receipt["error"] != "publication_setup_failed" {
		t.Fatal("resource mismatch lost the confirmed API identity")
	}
	for _, request := range srv.Requests() {
		if request.Method == http.MethodDelete {
			t.Fatal("resource mismatch deleted a resource")
		}
	}
}
