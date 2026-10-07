package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// keptPublicWarning is the warning of a publish that kept a target that was
// already published as public.
func keptPublicWarning(crid string) string {
	return "Warning: this target was published as public before, and it stays public: anyone who has the CRID can open it. " +
		"To make it private, delete it with `qurl delete " + crid + "` and publish again; the new resource gets a new CRID.\n"
}

// sentLog returns "METHOD path" for every request the mock received.
func sentLog(srv *apitest.Server) []string {
	requests := srv.Requests()
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return lines
}

// refusingLocalPublish returns the run options for a local publish whose
// Connector resource request must never run: the create that comes before it
// is expected to fail.
func refusingLocalPublish(t *testing.T, srv *apitest.Server, flags ...string) *runOpts {
	t.Helper()
	return localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
		if _, err := resolveID("agent-one"); err != nil {
			return nil, err
		}
		t.Error("the Connector resource request ran after the create failed")
		return nil, errors.New("unexpected Connector resource request")
	}, flags...)
}

// TestPublishWithNoPrivacyFlagKeepsAPublicTarget pins the publish of a person
// who never typed a privacy flag and whose target is already published as
// public, as a release that published as public by default left it. It is
// not an error on every run after the upgrade: the command exits 0 with the
// existing resource, says in the Access row that it is public, and warns
// that it stays public and how to make it private. The warning is in the
// document in text mode and on stderr for JSON and --quiet, whose stdout
// documents keep their shape.
//
// On the wire the first create states private, as every publish does, and
// only after the refusal is the same create sent without a privacy. It is so
// for a remote URL and for a local app, against the service and against an
// older one.
func TestPublishWithNoPrivacyFlagKeepsAPublicTarget(t *testing.T) {
	for _, olderService := range []bool{false, true} {
		for _, local := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("older_service=%t/local=%t/%v", olderService, local, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					if olderService {
						srv.PlayPublicByDefault()
					}
					srv.SetResourceAccess(false)
					srv.SetPublishFoundExisting(true)
					opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}}
					if local {
						opts = servingLocalPublish(t, srv, true)
					}
					opts.args = append(opts.args, mode...)
					res := runCLI(t, opts)
					if res.code != 0 {
						t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
					}

					creates := createRequests(t, srv)
					if len(creates) != 2 || string(creates[0]["private"]) != "true" {
						t.Fatalf("create requests = %v, want two, the first stating private", creates)
					}
					if _, stated := creates[1]["private"]; stated {
						t.Fatalf("the second create stated a privacy: %v", creates[1])
					}
					if log := sentLog(srv); log[0] != "POST /v1/resources" || log[1] != "POST /v1/resources" {
						t.Fatalf("requests = %v, want the two creates first", log)
					}

					stdout, stderr := res.stdout.String(), res.stderr.String()
					warning := keptPublicWarning(srv.Key.CRID)
					switch {
					case mode == nil:
						if !strings.HasPrefix(stdout, "Already published\n") || !strings.Contains(publishRows(stdout), "\n"+publicAccessRow+"\n") {
							t.Errorf("publish output lacks its headline or its public Access row:\n%s", stdout)
						}
						if !strings.HasSuffix(stdout, "\n\n"+warning+"\nCRID: "+srv.Key.CRID+"\n") {
							t.Errorf("publish output does not end with the warning and then the CRID alone:\n%s", stdout)
						}
						if stderr != "" || strings.Contains(stdout, "Delete it first") {
							t.Errorf("publish says more than the warning:\n%s%s", stdout, stderr)
						}
					case mode[0] == "--quiet":
						if stdout != srv.Key.CRID+"\n" || stderr != warning {
							t.Fatalf("--quiet = %q / %q", stdout, stderr)
						}
					default:
						var document struct {
							Private       *bool  `json:"private"`
							FoundExisting *bool  `json:"found_existing"`
							CRID          string `json:"crid"`
						}
						if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil {
							t.Fatalf("publish -o json: %v: %s", err, stdout)
						}
						if document.Private == nil || *document.Private || document.FoundExisting == nil || !*document.FoundExisting || document.CRID != srv.Key.CRID {
							t.Fatalf("publish -o json = %s", stdout)
						}
						if stderr != warning {
							t.Fatalf("stderr = %q, want the warning alone", stderr)
						}
					}
				})
			}
		}
	}
}

// TestPublishWithNoPrivacyFlagOfAPrivateTargetIsUnchanged pins the other
// targets a publish with no flag can find: one that is private, and none.
// Neither sends a second create or shows the warning.
func TestPublishWithNoPrivacyFlagOfAPrivateTargetIsUnchanged(t *testing.T) {
	for _, found := range []bool{true, false} {
		srv := apitest.NewServer(t)
		srv.SetPublishFoundExisting(found)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}})
		if res.code != 0 || !slices.Equal(sentLog(srv), []string{"POST /v1/resources"}) {
			t.Fatalf("found %t: exit %d, requests %v, stderr %s", found, res.code, sentLog(srv), res.stderr.String())
		}
		if strings.Contains(res.stdout.String()+res.stderr.String(), "Warning") || !strings.Contains(publishRows(res.stdout.String()), "\n"+privateAccessRow+"\n") {
			t.Fatalf("found %t: a private publish warns or lost its Access row:\n%s%s", found, res.stdout.String(), res.stderr.String())
		}
	}
}

// scriptUnaskedPublicResource makes the mock play an older service in the
// race: the first create is refused because the target exists, and the
// second, which states no privacy, is answered with a public resource the
// answer says was just made. createdAt is the creation time in that answer,
// left out when empty.
func scriptUnaskedPublicResource(t *testing.T, srv *apitest.Server, createdAt string) {
	t.Helper()
	srv.PlayPublicByDefault()
	srv.Script(http.MethodPost, "/v1/resources",
		func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", apitest.LegacyAccessSettingsDetail)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": false, "status": "active"}
			if createdAt != "" {
				data["created_at"] = createdAt
			}
			apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": false})
		})
}

// TestPublishNeverKeepsAPublicResourceThatWasJustMade pins the one answer to
// the second create that must not be kept. Between the two creates the
// existing resource went away, and an older service answers a create that
// states no privacy by making a public resource. Nobody asked for one: the
// command deletes it, prints no CRID anywhere, and fails. A local publish
// stops before its Connector resource request. The creation time in the
// answer is the moment the command ran, which is what lets it delete.
func TestPublishNeverKeepsAPublicResourceThatWasJustMade(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, deleteFails := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("local=%t/delete_fails=%t/%v", local, deleteFails, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					scriptUnaskedPublicResource(t, srv, fixedNow.Format(time.RFC3339))
					if deleteFails {
						srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
							apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
						})
					}
					opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}}
					if local {
						opts = refusingLocalPublish(t, srv)
					}
					opts.args = append(opts.args, mode...)
					res := runCLI(t, opts)
					if res.code != exitcode.ServerError {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.ServerError, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					stderr := res.stderr.String()
					want := []string{"the service made a new public resource for it", "Nobody asked for a public one", "so the command deleted it and printed no CRID", "Run the command again to publish the target as private"}
					if deleteFails {
						want = []string{"the service made a new public resource for it", "the command could not delete it, so no CRID was printed", "Run `qurl list` to find it, delete it with `qurl delete <CRID>`"}
					}
					for _, part := range want {
						if !strings.Contains(stderr, part) {
							t.Errorf("stderr lacks %q:\n%s", part, stderr)
						}
					}
					if strings.Contains(stderr, srv.Key.CRID) {
						t.Errorf("stderr names the CRID of a resource nobody asked for:\n%s", stderr)
					}
					if log := sentLog(srv); !slices.Equal(log, []string{"POST /v1/resources", "POST /v1/resources", "DELETE /v1/resources/" + srv.Key.CRID}) {
						t.Fatalf("requests = %v, want the two creates and one delete", log)
					}
				})
			}
		}
	}
}

// TestPublishDeletesNothingItCannotShowItMade pins the second thing the
// command needs before it deletes: a creation time in the answer that is no
// older than the command's first create. A delete is final, and the answer's
// word that the resource is new is one member of one answer. When the
// creation time is older, or is not there, the resource may be the one that
// was published before: nothing is deleted, no CRID is printed, the exit
// code is 10, and the message sends the publisher to `qurl list`.
func TestPublishDeletesNothingItCannotShowItMade(t *testing.T) {
	for name, createdAt := range map[string]string{
		"a day before the command":       fixedNow.Add(-24 * time.Hour).Format(time.RFC3339),
		"two minutes before the command": fixedNow.Add(-2 * time.Minute).Format(time.RFC3339),
		"no creation time":               "",
	} {
		for _, local := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("%s/local=%t/%v", name, local, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					scriptUnaskedPublicResource(t, srv, createdAt)
					opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}}
					if local {
						opts = refusingLocalPublish(t, srv)
					}
					opts.args = append(opts.args, mode...)
					res := runCLI(t, opts)
					if res.code != exitcode.ServerError {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.ServerError, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					stderr := res.stderr.String()
					for _, part := range []string{"the service did not confirm that this resource is private, so no CRID was printed", "run `qurl list` to check"} {
						if !strings.Contains(stderr, part) {
							t.Errorf("stderr lacks %q:\n%s", part, stderr)
						}
					}
					if strings.Contains(stderr, srv.Key.CRID) || strings.Contains(stderr, "deleted it") {
						t.Errorf("stderr names the CRID or claims a delete:\n%s", stderr)
					}
					if log := sentLog(srv); !slices.Equal(log, []string{"POST /v1/resources", "POST /v1/resources"}) {
						t.Fatalf("requests = %v, want the two creates and no delete", log)
					}
				})
			}
		}
	}
}

// TestPublishWithADeviceListThatDiffersIsAConflict pins, through the command,
// that a target already published with another list of allowed devices is a
// conflict with a next step, exit 7, and never "the service answered outside
// its contract". That holds for the service's code, for an answer that
// accepted the request and returned the stored list of a resource that
// existed, and for the older service's answer, for a remote URL and for a
// local app. Only a resource that was just made with another list is the
// answer the service should not have given, exit 10.
func TestPublishWithADeviceListThatDiffersIsAConflict(t *testing.T) {
	const otherKey = goldenSecondDevicePublicKey
	devices := []string{
		"this target is already published, and its allowed devices differ from the ones this command named",
		"Run the command again without --allow-device-key",
		"`qurl grants <CRID> --add <public-key>` or `--remove <public-key>`",
	}
	neither := []string{
		"its privacy or its allowed devices differ from what this command asked for",
		"Run the command again without --allow-device-key",
		"`qurl grants <CRID> --add <public-key>` or `--remove <public-key>`",
	}
	accepted := func(t *testing.T, srv *apitest.Server, found bool) {
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "status": "active", "allowed_device_keys": []string{otherKey},
			}, map[string]any{"found_existing": found})
		})
	}
	for _, test := range []struct {
		name     string
		prepare  func(*testing.T, *apitest.Server)
		wantCode int
		want     []string
	}{
		{
			name: "the service's code",
			prepare: func(_ *testing.T, srv *apitest.Server) {
				srv.SetResourceAccess(true, otherKey)
				srv.SetPublishFoundExisting(true)
			},
			wantCode: exitcode.Conflict, want: devices,
		},
		{
			name:     "an accepted request for a resource that existed",
			prepare:  func(t *testing.T, srv *apitest.Server) { accepted(t, srv, true) },
			wantCode: exitcode.Conflict, want: devices,
		},
		{
			name: "the older service",
			prepare: func(_ *testing.T, srv *apitest.Server) {
				srv.PlayPublicByDefault()
				srv.SetResourceAccess(true, otherKey)
				srv.SetPublishFoundExisting(true)
			},
			wantCode: exitcode.Conflict, want: neither,
		},
		{
			name:     "a resource that was just made",
			prepare:  func(t *testing.T, srv *apitest.Server) { accepted(t, srv, false) },
			wantCode: exitcode.ServerError, want: []string{"did not confirm the requested device grants"},
		},
	} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/local=%t", test.name, local), func(t *testing.T) {
				srv := apitest.NewServer(t)
				test.prepare(t, srv)
				flags := []string{"--allow-device-key", goldenDevicePublicKey}
				opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, flags...)}
				if local {
					opts = refusingLocalPublish(t, srv, flags...)
				}
				res := runCLI(t, opts)
				if res.code != test.wantCode {
					t.Fatalf("exit = %d, want %d; stderr: %s", res.code, test.wantCode, res.stderr.String())
				}
				mustEmptyStdout(t, res)
				for _, part := range test.want {
					if !strings.Contains(res.stderr.String(), part) {
						t.Errorf("stderr lacks %q:\n%s", part, res.stderr.String())
					}
				}
				if test.wantCode == exitcode.Conflict && strings.Contains(res.stderr.String(), "did not confirm") {
					t.Errorf("a conflict reads as an answer outside the contract:\n%s", res.stderr.String())
				}
				if log := sentLog(srv); !slices.Equal(log, []string{"POST /v1/resources"}) {
					t.Fatalf("requests = %v, want the one create", log)
				}
			})
		}
	}
}

// TestPublishCopySaysWhatHappensToATargetPublishedBefore pins the note for
// people who upgrade, in help and in the README: a target that an earlier
// release published is still public, a publish with no privacy flag keeps it
// and warns, and a flag that asks for what the resource is not is refused.
// The README shows the warning as the command prints it.
func TestPublishCopySaysWhatHappensToATargetPublishedBefore(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	res := runCLI(t, &runOpts{args: []string{"publish", "--help"}})
	if res.code != 0 {
		t.Fatalf("publish help exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	help := collapse(res.stdout.String())
	for _, want := range []string{
		"Publishing a target again reuses its resource, with the privacy it has.",
		"If you published a target with a release that made resources public by default, that resource is still public.",
		"Publishing it again with no privacy flag keeps it, and warns you that it stays public.",
		`To make it private, delete it with "qurl delete <CRID>" and publish again; the new resource gets a new CRID.`,
		"A flag that asks for what the existing resource is not is refused: --allow-device-key for a public resource, --public for a private one.",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("publish help lacks %q:\n%s", want, res.stdout.String())
		}
	}

	readme := collapse(strings.ReplaceAll(readCLIREADME(t), "`", ""))
	for _, want := range []string{
		"**If you published with an earlier release.**",
		"those resources are still public",
		"qurl publish for such a target with no privacy flag keeps working",
		collapse(strings.ReplaceAll(keptPublicWarning("<CRID>"), "`", "")),
		"The warning is part of the text output, and goes to stderr with -o json and --quiet; JSON says private: false.",
		"A publish never turns a public resource private, and it never makes a new public resource unless you pass --public.",
		"--allow-device-key with a list other than the one the resource has",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
}
