package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// grantKey returns a well-formed device public key that differs for each
// seed. No device holds any of them.
func grantKey(seed byte) string {
	raw := make([]byte, devicePublicKeySize)
	for index := range raw {
		raw[index] = seed
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// grantRequests returns the body of every grant change the mock received, and
// fails the test when the command sent anything but those changes and reads
// of the resource.
func grantRequests(t *testing.T, srv *apitest.Server) []map[string]json.RawMessage {
	t.Helper()
	var bodies []map[string]json.RawMessage
	for _, request := range srv.Requests() {
		if request.Path != "/v1/resources/"+srv.Key.CRID {
			t.Fatalf("grants sent %s %s", request.Method, request.Path)
		}
		if request.Header.Get("Authorization") == "" {
			t.Errorf("%s %s lacks device authority", request.Method, request.Path)
		}
		if request.Method != http.MethodPatch {
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(request.Body, &body); err != nil {
			t.Fatalf("grant change body %q: %v", request.Body, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// grantsDocument is the part of `qurl grants -o json` these tests read.
type grantsDocument struct {
	AllowedDeviceKeys []string `json:"allowed_device_keys"`
	Private           *bool    `json:"private"`
	CRID              string   `json:"crid"`
}

func decodeGrants(t *testing.T, res *runResult) grantsDocument {
	t.Helper()
	var document grantsDocument
	if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil {
		t.Fatalf("grants -o json emitted %q: %v", res.stdout.String(), err)
	}
	if document.AllowedDeviceKeys == nil {
		t.Fatalf("grants -o json has no allowed_device_keys array: %s", res.stdout.String())
	}
	return document
}

// TestGrantsWithNoFlagPrintsTheList pins the read: `qurl grants <CRID>` shows
// the current list in text and in JSON, an empty list included, and changes
// nothing.
func TestGrantsWithNoFlagPrintsTheList(t *testing.T) {
	first, second := grantKey(1), grantKey(2)
	for _, keys := range [][]string{{first, second}, {}} {
		t.Run(fmt.Sprintf("%d keys", len(keys)), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetResourceAccess(true, keys...)

			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
			if res.code != 0 || res.stderr.Len() != 0 {
				t.Fatalf("grants -o json: exit %d, stderr %q", res.code, res.stderr.String())
			}
			document := decodeGrants(t, res)
			if !slices.Equal(document.AllowedDeviceKeys, keys) || document.Private == nil || !*document.Private || document.CRID != srv.Key.CRID {
				t.Fatalf("grants -o json = %+v, want the private resource with keys %v", document, keys)
			}

			res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID}})
			want := "Allowed device keys:  [" + strings.Join(keys, " ") + "]\n"
			if res.code != 0 || !strings.Contains(res.stdout.String(), want) || !strings.Contains(res.stdout.String(), "Private:") {
				t.Fatalf("grants text: exit %d, want the row %q:\n%s%s", res.code, want, res.stdout.String(), res.stderr.String())
			}

			if changes := grantRequests(t, srv); len(changes) != 0 {
				t.Fatalf("reading the list sent %d grant changes", len(changes))
			}
			for _, request := range srv.Requests() {
				if request.Method != http.MethodGet {
					t.Fatalf("reading the list sent %s %s", request.Method, request.Path)
				}
			}
		})
	}
}

// TestGrantsAddAndRemoveChangeSingleKeys pins the wire shape and the result
// of a change to single keys: one authenticated PATCH that carries the keys
// to add and the keys to remove and never the member that replaces the
// complete list, so a grant the command was not asked about cannot be lost.
// The output is the complete list the service answered with.
func TestGrantsAddAndRemoveChangeSingleKeys(t *testing.T) {
	kept, dropped, added, extra := grantKey(1), grantKey(2), grantKey(3), grantKey(4)
	for _, test := range []struct {
		name       string
		flags      []string
		wantAdd    []string
		wantRemove []string
		wantList   []string
	}{
		{name: "add one", flags: []string{"--add", added}, wantAdd: []string{added}, wantList: []string{kept, dropped, added}},
		{name: "add two", flags: []string{"--add", added, "--add", extra}, wantAdd: []string{added, extra}, wantList: []string{kept, dropped, added, extra}},
		{name: "remove one", flags: []string{"--remove", dropped}, wantRemove: []string{dropped}, wantList: []string{kept}},
		{name: "add and remove in one change", flags: []string{"--add", added, "--remove", dropped}, wantAdd: []string{added}, wantRemove: []string{dropped}, wantList: []string{kept, added}},
		{name: "add a key that is already allowed", flags: []string{"--add", kept}, wantAdd: []string{kept}, wantList: []string{kept, dropped}},
		{name: "remove a key that is not on the list", flags: []string{"--remove", added}, wantRemove: []string{added}, wantList: []string{kept, dropped}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetResourceAccess(true, kept, dropped)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}, test.flags...)})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			changes := grantRequests(t, srv)
			if len(changes) != 1 || len(srv.Requests()) != 1 {
				t.Fatalf("the change sent %d grant changes in %d requests, want one PATCH", len(changes), len(srv.Requests()))
			}
			if replaced, present := changes[0]["allowed_device_keys"]; present {
				t.Fatalf("a change to single keys sent the member that replaces the complete list: %s", replaced)
			}
			for member, want := range map[string][]string{"allowed_device_keys_add": test.wantAdd, "allowed_device_keys_remove": test.wantRemove} {
				var sent []string
				if raw, present := changes[0][member]; present {
					if err := json.Unmarshal(raw, &sent); err != nil {
						t.Fatalf("%s = %s: %v", member, raw, err)
					}
				}
				if !slices.Equal(sent, want) {
					t.Fatalf("%s = %v, want %v", member, sent, want)
				}
			}
			if document := decodeGrants(t, res); !slices.Equal(document.AllowedDeviceKeys, test.wantList) {
				t.Fatalf("resulting list = %v, want %v", document.AllowedDeviceKeys, test.wantList)
			}
		})
	}
}

// TestGrantsClearReplacesTheListWithAnEmptyOne pins the one change that still
// replaces the complete list: --clear sends an empty list and nothing else.
func TestGrantsClearReplacesTheListWithAnEmptyOne(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, grantKey(1), grantKey(2))
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--clear", "-o", "json"}})
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	changes := grantRequests(t, srv)
	if len(changes) != 1 || len(changes[0]) != 1 || string(changes[0]["allowed_device_keys"]) != "[]" {
		t.Fatalf("--clear sent %v, want exactly one empty replacement list", changes)
	}
	if document := decodeGrants(t, res); len(document.AllowedDeviceKeys) != 0 || !strings.Contains(res.stdout.String(), `"allowed_device_keys": []`) {
		t.Fatalf("--clear output did not confirm an empty list: %s", res.stdout.String())
	}

	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--clear"}})
	if res.code != 0 || !strings.Contains(res.stdout.String(), "Allowed device keys:") || !strings.Contains(res.stdout.String(), "[]") {
		t.Fatalf("--clear text lacks the empty list: %s / %s", res.stdout.String(), res.stderr.String())
	}
}

// TestGrantsFailWhenTheAnswerDoesNotShowTheChange pins the check on the
// answer. A service from before single grants could be added or removed
// ignores the request and answers with a success status and the list as it
// was. That must be a failure with nothing on stdout, in every output mode,
// and so must any other answer that lacks an added key or still has a removed
// one.
func TestGrantsFailWhenTheAnswerDoesNotShowTheChange(t *testing.T) {
	kept, added := grantKey(1), grantKey(3)
	answerWith := func(srv *apitest.Server, keys ...string) {
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true,
				"allowed_device_keys": keys, "type": "url", "status": "active",
			}, nil)
		})
	}
	for _, test := range []struct {
		name    string
		flags   []string
		prepare func(*apitest.Server)
	}{
		{name: "older service, add", flags: []string{"--add", added}, prepare: (*apitest.Server).PlayNoSingleGrantEdits},
		{name: "older service, remove", flags: []string{"--remove", kept}, prepare: (*apitest.Server).PlayNoSingleGrantEdits},
		{name: "older service, add and remove", flags: []string{"--add", added, "--remove", kept}, prepare: (*apitest.Server).PlayNoSingleGrantEdits},
		{name: "the added key is missing", flags: []string{"--add", added}, prepare: func(srv *apitest.Server) { answerWith(srv, kept) }},
		{name: "one of two added keys is missing", flags: []string{"--add", added, "--add", grantKey(4)}, prepare: func(srv *apitest.Server) { answerWith(srv, kept, added) }},
		{name: "the removed key is still there", flags: []string{"--remove", kept}, prepare: func(srv *apitest.Server) { answerWith(srv, kept) }},
		{name: "added but not removed", flags: []string{"--add", added, "--remove", kept}, prepare: func(srv *apitest.Server) { answerWith(srv, kept, added) }},
		{name: "an answer with no list", flags: []string{"--add", added}, prepare: func(srv *apitest.Server) { answerWith(srv) }},
	} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("%s/%v", test.name, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.SetResourceAccess(true, kept)
				test.prepare(srv)
				args := append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, test.flags...)
				res := runCLI(t, &runOpts{args: append(args, mode...)})
				if res.code != exitcode.ServerError {
					t.Fatalf("exit = %d, want %d; stdout %q stderr %q", res.code, exitcode.ServerError, res.stdout.String(), res.stderr.String())
				}
				mustEmptyStdout(t, res)
				for _, want := range []string{"this service cannot add or remove single device grants yet", "Run `qurl grants <CRID>` to see the list as it is now"} {
					if !strings.Contains(res.stderr.String(), want) {
						t.Errorf("stderr lacks %q: %s", want, res.stderr.String())
					}
				}
			})
		}
	}

	// On that older service a change that asks for what is already true is
	// still reported correctly: the answer shows the list in the state that
	// was asked for.
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, kept)
	srv.PlayNoSingleGrantEdits()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--add", kept, "--remove", added, "-o", "json"}})
	if res.code != 0 || !slices.Equal(decodeGrants(t, res).AllowedDeviceKeys, []string{kept}) {
		t.Fatalf("a change that was already true: exit %d, stdout %s, stderr %s", res.code, res.stdout.String(), res.stderr.String())
	}
}

// TestGrantsUsageErrorsSendNothing pins the command lines that cannot be one
// change to the list. Each is a usage error before any request. The flag that
// used to replace the complete list is among them, and its message names
// --add.
func TestGrantsUsageErrorsSendNothing(t *testing.T) {
	key, other := grantKey(1), grantKey(2)
	tooMany := make([]string, 0, 2*257)
	for seed := range 257 {
		raw := make([]byte, devicePublicKeySize)
		raw[0], raw[1] = byte(seed), byte(seed>>8)
		tooMany = append(tooMany, "--add", base64.StdEncoding.EncodeToString(raw))
	}
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "the replace flag", flags: []string{"--allow-device-key", key}, want: "--allow-device-key no longer replaces the list: use --add <public-key>"},
		{name: "the replace flag with add", flags: []string{"--allow-device-key", key, "--add", other}, want: "use --add <public-key>"},
		{name: "the replace flag with clear", flags: []string{"--clear", "--allow-device-key", key}, want: "use --add <public-key>"},
		{name: "clear with add", flags: []string{"--clear", "--add", key}, want: msgGrantsClearWithEdit},
		{name: "clear with remove", flags: []string{"--clear", "--remove", key}, want: msgGrantsClearWithEdit},
		{name: "add a value that is not a public key", flags: []string{"--add", "invalid"}, want: "--add requires unique canonical"},
		{name: "remove a value that is neither a public key nor a device id", flags: []string{"--remove", "invalid"}, want: msgGrantsRemoveInvalid},
		{name: "add the same key twice", flags: []string{"--add", key, "--add", key}, want: "--add requires unique canonical"},
		{name: "remove the same key twice", flags: []string{"--remove", key, "--remove", key}, want: msgGrantsRemoveInvalid},
		{name: "add and remove the same key", flags: []string{"--add", key, "--remove", key}, want: msgGrantsAddAndRemove},
		{name: "more keys than a list can hold", flags: tooMany, want: "--add accepts at most 256 keys"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, test.flags...)})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), test.want) {
				t.Fatalf("exit = %d, stderr = %q; want the usage error %q", res.code, res.stderr.String(), test.want)
			}
			mustEmptyStdout(t, res)
			if got := len(srv.Requests()); got != 0 {
				t.Fatalf("a refused command line sent %d requests", got)
			}
		})
	}
	help := runCLI(t, &runOpts{args: []string{"grants", "--help"}})
	if help.code != 0 || strings.Contains(help.stdout.String(), "--allow-device-key string") || !strings.Contains(help.stdout.String(), "--add stringArray") || !strings.Contains(help.stdout.String(), "--remove stringArray") {
		t.Fatalf("grants help must list --add and --remove and not the replace flag:\n%s", help.stdout.String())
	}
}

// TestGrantsShowTheServiceRefusal pins that a change the service refuses is
// an error with the service's reason and nothing on stdout: here a list that
// is full.
func TestGrantsShowTheServiceRefusal(t *testing.T) {
	full := make([]string, 0, 256)
	for seed := range 256 {
		raw := make([]byte, devicePublicKeySize)
		raw[0], raw[1] = byte(seed), 0xff
		full = append(full, base64.StdEncoding.EncodeToString(raw))
	}
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, full...)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--add", grantKey(7)}})
	if res.code != exitcode.InvalidInput || !strings.Contains(res.stderr.String(), "at most 256 allowed device keys") {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	// Taking one off the full list and adding another fits, as one change.
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--add", grantKey(7), "--remove", full[0], "-o", "json"}})
	if res.code != 0 || len(decodeGrants(t, res).AllowedDeviceKeys) != 256 {
		t.Fatalf("a swap on a full list: exit %d, stderr %s", res.code, res.stderr.String())
	}
}

func TestDeviceGrantLimit(t *testing.T) {
	if err := validateAllowedDeviceKeys(make([]string, 257)); err == nil || !strings.Contains(err.Error(), "--allow-device-key accepts at most 256 keys") {
		t.Fatalf("grant cap not enforced: %v", err)
	}
}

func TestPublishWithADeviceListConfirmsOutput(t *testing.T) {
	srv := apitest.NewServer(t)
	key := grantKey(1)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com", "--allow-device-key", key, "-o", "json"}})
	if res.code != 0 || !strings.Contains(res.stdout.String(), `"private": true`) {
		t.Fatalf("publish with a device list failed: %s", res.stderr.String())
	}
	// The list set at creation is the list grants reads back.
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
	if res.code != 0 || !slices.Equal(decodeGrants(t, res).AllowedDeviceKeys, []string{key}) {
		t.Fatalf("grants after publish: exit %d, stdout %s, stderr %s", res.code, res.stdout.String(), res.stderr.String())
	}
}

// A grant command on a public resource succeeds, so the note has to say that
// the list changes nothing about who can use the resource. It follows a read
// and every kind of change. A private resource gets no such note.
func TestPublicGrantsExplainThatGrantsHaveNoEffect(t *testing.T) {
	const note = "This resource is public, so device grants have no effect on it. They apply to a private resource, which is what `qurl publish` creates unless you pass --public."
	for _, private := range []bool{false, true} {
		for _, flags := range [][]string{nil, {"--clear"}, {"--add", grantKey(1)}, {"--remove", grantKey(2)}} {
			t.Run(fmt.Sprintf("private=%t/%v", private, flags), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.SetResourceAccess(private, grantKey(2))
				args := append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}, flags...)
				res := runCLI(t, &runOpts{args: args})
				stderr := res.stderr.String()
				noted := strings.Contains(stderr, note)
				if res.code != 0 || noted == private {
					t.Fatalf("exit %d, no-effect note shown=%t: %s", res.code, noted, stderr)
				}
				if strings.Contains(stderr, "who can request links") {
					t.Fatalf("the note still makes a claim about who can request links: %s", stderr)
				}
				if document := decodeGrants(t, res); document.Private == nil || *document.Private != private {
					t.Fatalf("grants -o json private = %v, want %t", document.Private, private)
				}
			})
		}
	}
}

// TestGrantsCopyTeachesAddAndRemove pins the copy for the change of form:
// help and README teach --add, --remove and --clear, and the form that
// replaced the complete list appears once, in the README, as removed.
func TestGrantsCopyTeachesAddAndRemove(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	readme := collapse(strings.ReplaceAll(readCLIREADME(t), "`", ""))
	for _, want := range []string{
		"qurl grants <CRID> --add <public-key>",
		"qurl grants <CRID> --remove <public-key>",
		"qurl grants <CRID> --clear",
		"| qurl grants <CRID> | Show or change the devices and people allowed to open a private resource |",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not teach %q", want)
		}
	}
	const replaceForm = "qurl grants <CRID> --allow-device-key"
	if got := strings.Count(readme, replaceForm); got != 1 || !strings.Contains(readme, replaceForm+" <public-key>. That form is removed") {
		t.Errorf("README shows the replace form %d times; want once, as removed", got)
	}

	res := runCLI(t, &runOpts{args: []string{"grants", "--help"}})
	if res.code != 0 {
		t.Fatalf("grants help exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	help := collapse(res.stdout.String())
	for _, want := range []string{
		"With no flag, the command prints both lists",
		"--add allows a device and --remove takes one off the list.",
		"applied as one change",
		"--clear takes every public key off the list. It does not remove approved people.",
		`To set the first list when you publish, use "qurl publish --allow-device-key".`,
		"have no effect on a public one",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("grants help lacks %q:\n%s", want, res.stdout.String())
		}
	}
	if strings.Contains(strings.ToLower(help), "replace") {
		t.Errorf("grants help still describes replacing the list:\n%s", res.stdout.String())
	}
}

// TestGrantsEditThatLostToOtherWritersIsUnavailableAndNotRetried pins the
// answer the service gives when a change to single grants lost every attempt
// against other changes to the same resource: HTTP 503 with a short
// Retry-After, and nothing changed. The command reports it with the
// unavailable exit code and the service's own text, prints nothing on stdout,
// and sends the request exactly once: sending it again is the caller's
// decision.
func TestGrantsEditThatLostToOtherWritersIsUnavailableAndNotRetried(t *testing.T) {
	const detail = "The device grants were being changed by other requests. Nothing was changed; send the request again."
	for _, flags := range [][]string{{"--add", grantKey(3)}, {"--remove", grantKey(1)}, {"--add", grantKey(3), "--remove", grantKey(1)}} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("%v/%v", flags, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.SetResourceAccess(true, grantKey(1))
				srv.ScriptRepeat(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, 3, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", "1")
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", detail)
				})
				var sleeps []time.Duration
				args := append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, flags...)
				res := runCLI(t, &runOpts{args: append(args, mode...), sleeps: &sleeps})
				if res.code != exitcode.Unavailable {
					t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.Unavailable, res.stderr.String())
				}
				mustEmptyStdout(t, res)
				if !strings.Contains(res.stderr.String(), detail) || !strings.Contains(res.stderr.String(), "HTTP 503") {
					t.Fatalf("stderr does not carry the service's text: %s", res.stderr.String())
				}
				if got := len(srv.Requests()); got != 1 || len(sleeps) != 0 {
					t.Fatalf("the change was sent %d times with %d waits, want once and no wait", got, len(sleeps))
				}
			})
		}
	}
}
