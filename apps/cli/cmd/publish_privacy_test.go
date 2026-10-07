package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	privacyRemoteTarget = "https://example.com/data"
	privacyLocalTarget  = "http://127.0.0.1:3000"
	// The two rows publish can show, with the column padding collapsed.
	privateAccessRow = "Access: private — only you and the people you allow can open it"
	publicAccessRow  = "Access: public — anyone who has the CRID can open it"
)

// publishRows returns publish's text output with each run of spaces collapsed
// to one, so a row can be matched whatever the width of its label column.
func publishRows(stdout string) string {
	lines := strings.Split(stdout, "\n")
	for index, line := range lines {
		lines[index] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Join(lines, "\n")
}

// createRequests returns the body of every create request the mock received.
func createRequests(t *testing.T, srv *apitest.Server) []map[string]json.RawMessage {
	t.Helper()
	var bodies []map[string]json.RawMessage
	for _, request := range srv.Requests() {
		if request.Method != http.MethodPost || request.Path != "/v1/resources" {
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(request.Body, &body); err != nil {
			t.Fatalf("create request body %q: %v", request.Body, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// servingLocalPublish prepares the mock and the run options for a local
// publish that completes: the Connector resource request answers with the
// mock's one resource, and the sharing state goes from off to serving.
func servingLocalPublish(t *testing.T, srv *apitest.Server, createFound bool, flags ...string) *runOpts {
	t.Helper()
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "on", 1, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 1, "serving"))
	// The Connector request always finds the resource the create request
	// made a moment earlier, whatever the create answer said.
	resolver := resolvedLocalResource(srv, true)
	srv.SetPublishFoundExisting(createFound)
	return localPublish(t, srv, resolver, flags...)
}

// localPublish returns the run options for a local publish whose Connector
// resource request is answered by resolver. The registry already knows its
// owner, so the create request is the first request the publish sends.
func localPublish(t *testing.T, srv *apitest.Server, resolver localResourceResolver, flags ...string) *runOpts {
	t.Helper()
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &runOpts{
		args:          append([]string{"--endpoint", srv.URL, "publish", privacyLocalTarget}, flags...),
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: &recordingShareDaemon{}, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolver,
	}
}

// TestPublishIsPrivateUnlessPublic pins the default and the wire rule behind
// it, for a remote URL and for a local app: no flag asks for a private
// resource, --public for a public one, and the hidden --private changes
// nothing. The create request states the choice every time, including when
// it is the default, and the output says which it is.
func TestPublishIsPrivateUnlessPublic(t *testing.T) {
	for _, test := range []struct {
		name        string
		flags       []string
		wantPrivate bool
	}{
		{name: "no flag", wantPrivate: true},
		{name: "the hidden private flag", flags: []string{"--private"}, wantPrivate: true},
		{name: "the hidden private flag, spelled out", flags: []string{"--private=true"}, wantPrivate: true},
		{name: "public", flags: []string{"--public"}},
		{name: "public, spelled out", flags: []string{"--public=true"}},
		{name: "public turned off", flags: []string{"--public=false"}, wantPrivate: true},
	} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/local=%t", test.name, local), func(t *testing.T) {
				srv := apitest.NewServer(t)
				opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, test.flags...)}
				if local {
					opts = servingLocalPublish(t, srv, false, test.flags...)
				}
				res := runCLI(t, opts)
				if res.code != 0 {
					t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
				}
				creates := createRequests(t, srv)
				if len(creates) != 1 {
					t.Fatalf("publish sent %d create requests, want one", len(creates))
				}
				stated, present := creates[0]["private"]
				if want := strconv.FormatBool(test.wantPrivate); !present || string(stated) != want {
					t.Fatalf("create request stated private = %q (present %t), want %s", stated, present, want)
				}
				wantRow, otherRow := privateAccessRow, publicAccessRow
				if !test.wantPrivate {
					wantRow, otherRow = publicAccessRow, privateAccessRow
				}
				stdout := res.stdout.String()
				if rows := publishRows(stdout); !strings.Contains(rows, "\n"+wantRow+"\n") || strings.Contains(rows, otherRow) {
					t.Fatalf("publish output does not say which it is, want the row %q:\n%s", wantRow, stdout)
				}
				if !strings.HasPrefix(stdout, "Published\n") || !strings.HasSuffix(stdout, "\nCRID: "+srv.Key.CRID+"\n") {
					t.Fatalf("publish output lost its headline or its last CRID line:\n%s", stdout)
				}
			})
		}
	}
}

// TestPublishJSONAndQuietKeepTheirDocuments pins the two script-facing
// outputs: JSON carries the private member for both kinds, and --quiet is
// the CRID alone, with nothing about privacy on either stream.
func TestPublishJSONAndQuietKeepTheirDocuments(t *testing.T) {
	for _, public := range []bool{false, true} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("public=%t/local=%t", public, local), func(t *testing.T) {
				var flags []string
				if public {
					flags = []string{"--public"}
				}
				run := func(extra ...string) *runResult {
					t.Helper()
					srv := apitest.NewServer(t)
					opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, flags...)}
					if local {
						opts = servingLocalPublish(t, srv, false, flags...)
					}
					opts.args = append(opts.args, extra...)
					res := runCLI(t, opts)
					if res.code != 0 {
						t.Fatalf("publish %v exit = %d, stderr: %s", extra, res.code, res.stderr.String())
					}
					if extra[0] == "--quiet" {
						if got, want := res.stdout.String(), srv.Key.CRID+"\n"; got != want {
							t.Fatalf("--quiet stdout = %q, want the CRID alone %q", got, want)
						}
					}
					return res
				}
				var document struct {
					Private *bool `json:"private"`
				}
				res := run("-o", "json")
				if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil {
					t.Fatalf("publish -o json emitted %q: %v", res.stdout.String(), err)
				}
				if document.Private == nil || *document.Private == public {
					t.Fatalf("publish -o json private = %v, want %t", document.Private, !public)
				}
				for _, res := range []*runResult{res, run("--quiet")} {
					if res.stderr.Len() != 0 || strings.Contains(res.stdout.String(), "Access") {
						t.Fatalf("a script-facing publish said more than its document: stdout %q stderr %q", res.stdout.String(), res.stderr.String())
					}
				}
			})
		}
	}
}

// TestPublishDoesNotDependOnTheServiceDefault runs a plain publish against a
// service that makes a public resource when privacy is not stated. The
// resource is private all the same, because the request states it.
func TestPublishDoesNotDependOnTheServiceDefault(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("local=%t", local), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.PlayPublicByDefault()
			opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}}
			if local {
				opts = servingLocalPublish(t, srv, false)
			}
			res := runCLI(t, opts)
			if res.code != 0 || !strings.Contains(publishRows(res.stdout.String()), "\n"+privateAccessRow+"\n") {
				t.Fatalf("publish against a service that is public by default: exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout.String(), res.stderr.String())
			}
		})
	}
}

// TestPublishFailsClosedWhenPrivacyIsNotConfirmed pins the check on the
// create answer through the command, in every output mode: a row with the
// other privacy, or with none, exits 10 with nothing on stdout and no CRID
// anywhere. A local publish also stops there: the Connector resource request
// is never made and sharing is never turned on.
func TestPublishFailsClosedWhenPrivacyIsNotConfirmed(t *testing.T) {
	for _, test := range []struct {
		name     string
		flags    []string
		answered any
		want     string
	}{
		{name: "private asked, public answered", answered: false, want: "did not confirm that this resource is private"},
		{name: "private asked, nothing answered", want: "did not confirm that this resource is private"},
		{name: "public asked, private answered", flags: []string{"--public"}, answered: true, want: "did not confirm that this resource is public"},
		{name: "public asked, nothing answered", flags: []string{"--public"}, want: "did not confirm that this resource is public"},
	} {
		for _, local := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("%s/local=%t/%v", test.name, local, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
						data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active"}
						if test.answered != nil {
							data["private"] = test.answered
						}
						apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": false})
					})
					opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, test.flags...)}
					connectorRequests := 0
					if local {
						opts = localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
							if _, err := resolveID("agent-one"); err != nil {
								return nil, err
							}
							connectorRequests++
							return nil, errors.New("the Connector resource request ran for a resource whose privacy was not confirmed")
						}, test.flags...)
					}
					opts.args = append(opts.args, mode...)
					res := runCLI(t, opts)
					if res.code != exitcode.ServerError {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.ServerError, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					stderr := res.stderr.String()
					if !strings.Contains(stderr, test.want) || !strings.Contains(stderr, "no CRID was printed") || !strings.Contains(stderr, "`qurl list`") {
						t.Fatalf("stderr does not say what happened and what to check: %s", stderr)
					}
					if strings.Contains(stderr, srv.Key.CRID) {
						t.Fatalf("stderr shows the CRID of a resource with unconfirmed privacy: %s", stderr)
					}
					if connectorRequests != 0 {
						t.Fatal("the Connector resource request ran after an unconfirmed create")
					}
					if requests := srv.Requests(); len(requests) != 1 {
						t.Fatalf("an unconfirmed create was followed by more requests: %#v", requests)
					}
				})
			}
		}
	}
}

// TestPublishOfATargetWithTheOtherPrivacyIsAConflict pins the refusal through
// the command when a flag asked for what the existing resource is not, for a
// remote URL and for a local app: exit 7, nothing on stdout, what exists, and
// what to do. The privacy that exists is named for the code the service sends
// now; the answer of an older service does not say what differs, so the
// message names neither. A local publish makes no Connector resource request
// and never turns sharing on. A publish with no privacy flag at all is not
// this case: it keeps a public resource that exists.
func TestPublishOfATargetWithTheOtherPrivacyIsAConflict(t *testing.T) {
	older := []string{"its privacy or its allowed devices differ from what this command asked for", "privacy is fixed when a resource is first published", "Run the command again without --allow-device-key, with --public if the resource is public"}
	public := []string{"already published as public", "privacy is fixed when a resource is first published", "run the command again with --public", "delete it with `qurl delete <CRID>`"}
	for _, test := range []struct {
		name            string
		flags           []string
		existingPrivate bool
		want            []string
	}{
		{name: "private asked, public exists", flags: []string{"--private"}, want: public},
		{name: "a device list asked, public exists", flags: []string{"--allow-device-key", goldenDevicePublicKey}, want: public},
		{name: "not public asked, public exists", flags: []string{"--public=false"}, want: public},
		{
			name: "public asked, private exists", flags: []string{"--public"}, existingPrivate: true,
			want: []string{"already published as private", "privacy is fixed when a resource is first published", "run the command again without --public", "delete it with `qurl delete <CRID>`"},
		},
	} {
		for _, olderService := range []bool{false, true} {
			for _, local := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/older_service=%t/local=%t", test.name, olderService, local), func(t *testing.T) {
					srv := apitest.NewServer(t)
					want := test.want
					if olderService {
						srv.PlayPublicByDefault()
						want = older
					}
					srv.SetResourceAccess(test.existingPrivate)
					srv.SetPublishFoundExisting(true)
					opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, test.flags...)}
					if local {
						opts = localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
							if _, err := resolveID("agent-one"); err != nil {
								return nil, err
							}
							t.Error("the Connector resource request ran after the create was refused")
							return nil, errors.New("unexpected Connector resource request")
						}, test.flags...)
					}
					res := runCLI(t, opts)
					if res.code != exitcode.Conflict {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.Conflict, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					for _, part := range want {
						if !strings.Contains(res.stderr.String(), part) {
							t.Errorf("stderr lacks %q:\n%s", part, res.stderr.String())
						}
					}
					if olderService && (strings.Contains(res.stderr.String(), "as public") || strings.Contains(res.stderr.String(), "as private")) {
						t.Errorf("stderr names a privacy the older answer did not state:\n%s", res.stderr.String())
					}
					for _, serviceText := range []string{"Invalid Input", "Privacy Mismatch", "allowed_device_keys", "HTTP 400"} {
						if strings.Contains(res.stderr.String(), serviceText) {
							t.Errorf("stderr shows the service's own wording %q:\n%s", serviceText, res.stderr.String())
						}
					}
					if requests := srv.Requests(); len(requests) != 1 {
						t.Fatalf("a refused create was followed by more requests: %#v", requests)
					}
				})
			}
		}
	}
}

// TestLocalPublishStatesPrivacyBeforeTheConnectorRequest pins the order and
// the shape of the create request a local publish sends: one Connector
// find-or-create for the chosen ID, with privacy stated and the first device
// list when there is one, answered before the Connector resource request is
// made.
func TestLocalPublishStatesPrivacyBeforeTheConnectorRequest(t *testing.T) {
	const key = "cHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHA="
	for _, test := range []struct {
		name        string
		flags       []string
		wantPrivate bool
		wantKeys    int
	}{
		{name: "no flag", wantPrivate: true},
		{name: "public", flags: []string{"--public"}},
		{name: "a device list", flags: []string{"--allow-device-key", key}, wantPrivate: true, wantKeys: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stop := errors.New("stop at the Connector resource request")
			var resolvedID string
			res := runCLI(t, localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
				id, err := resolveID("agent-one")
				if err != nil {
					return nil, err
				}
				resolvedID = id
				if len(createRequests(t, srv)) != 1 {
					t.Error("the Connector resource request ran before the create request was answered")
				}
				return nil, stop
			}, test.flags...))
			if !strings.Contains(res.stderr.String(), stop.Error()) {
				t.Fatalf("publish did not reach the Connector resource request: %s", res.stderr.String())
			}
			creates := createRequests(t, srv)
			if len(creates) != 1 {
				t.Fatalf("local publish sent %d create requests, want one", len(creates))
			}
			var body struct {
				Allowed      []string `json:"allowed_device_keys"`
				Private      *bool    `json:"private"`
				Type         string   `json:"type"`
				Slug         string   `json:"slug"`
				FindOrCreate bool     `json:"find_or_create"`
				TargetURL    string   `json:"target_url"`
			}
			raw, err := json.Marshal(creates[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if body.Private == nil || *body.Private != test.wantPrivate {
				t.Fatalf("create request stated private = %v, want %t", body.Private, test.wantPrivate)
			}
			if body.Type != "tunnel" || body.Slug == "" || body.Slug != resolvedID || !body.FindOrCreate || body.TargetURL != "" || len(body.Allowed) != test.wantKeys {
				t.Fatalf("create request = %+v, want a Connector find-or-create for %q with %d allowed devices", body, resolvedID, test.wantKeys)
			}
		})
	}
}

// TestLocalPublishReportsWhatTheCreateAnswerSays pins where a local publish
// reads whether the resource already existed. The Connector resource request
// always finds the resource the create request made a moment earlier, so its
// own answer would call every first publish "already published"; the create
// answer is the one that knows.
func TestLocalPublishReportsWhatTheCreateAnswerSays(t *testing.T) {
	for _, createFound := range []bool{false, true} {
		t.Run(fmt.Sprintf("create_found_existing=%t", createFound), func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, servingLocalPublish(t, srv, createFound))
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			wantHeadline := "Published\n"
			if createFound {
				wantHeadline = "Already published\n"
			}
			if !strings.HasPrefix(res.stdout.String(), wantHeadline) {
				t.Fatalf("headline does not follow the create answer (found_existing %t):\n%s", createFound, res.stdout.String())
			}

			srv = apitest.NewServer(t)
			opts := servingLocalPublish(t, srv, createFound)
			opts.args = append(opts.args, "-o", "json")
			res = runCLI(t, opts)
			if want := fmt.Sprintf(`"found_existing": %t`, createFound); res.code != 0 || !strings.Contains(res.stdout.String(), want) {
				t.Fatalf("publish -o json = exit %d, want %s:\n%s", res.code, want, res.stdout.String())
			}
		})
	}
}

// TestLocalPublishRefusesAResourceWhosePrivacyWasNeverStated pins the guard
// behind the rule that no publish depends on the service's default: when the
// Connector resource request answers without the create request having been
// made, the publish stops before sharing is turned on.
func TestLocalPublishRefusesAResourceWhosePrivacyWasNeverStated(t *testing.T) {
	srv := apitest.NewServer(t)
	found := false
	res := runCLI(t, localPublish(t, srv, func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
		return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
			ResourcePublicKey: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: "local-test",
			ConnectorRoutingID: "c-" + strings.Repeat("a", 52), KnockResourceID: "q_catalog_key",
		}, FoundExisting: &found}, nil
	}))
	if res.code != exitcode.ServerError || !strings.Contains(res.stderr.String(), "before its privacy was stated") {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("a publish with unstated privacy sent sharing requests: %#v", requests)
	}
}
