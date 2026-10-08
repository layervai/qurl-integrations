package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/clitest"
)

// goldenCase drives one command through the three output variants. Variants:
// tty (color, terminals attached), plain (piped), json (-o json, piped).
type goldenCase struct {
	name     string
	args     func(srv *apitest.Server) []string
	prepare  func(srv *apitest.Server)
	env      func(srv *apitest.Server) map[string]string
	variants []string
	wantCode int
	// stdin is piped input (login's key); empty means an empty pipe.
	stdin string
	// linkSite is the origin this install knows as its link site; empty
	// means it knows none, as with the deployment a release ships.
	linkSite string
	// chdirTemp runs the variant in a fresh temp working directory, so
	// cases whose output embeds a relative --file path stay deterministic
	// and leave nothing behind in the repo tree.
	chdirTemp bool
	// setup seeds the (possibly temp) working directory before the run.
	setup func(t *testing.T)
	// stdoutGolden/stderrGolden select which streams are golden-compared;
	// a stream not selected must be byte-empty.
	stdoutGolden bool
	stderrGolden bool
}

func goldenVariants() []string { return []string{"tty", "plain", "json"} }

// goldenDevicePublicKey and goldenSecondDevicePublicKey are well-formed
// device public keys for the cases that name one. No device holds them.
const (
	goldenDevicePublicKey       = "cHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHA="
	goldenSecondDevicePublicKey = "cXFxcXFxcXFxcXFxcXFxcXFxcXFxcXFxcXFxcXFxcXE="
	// goldenThirdDevice is a device id for the cases that need a third
	// approved person. No device has it.
	goldenThirdDevice = "keep-keep-keep-keep"
)

// TestGoldens pins the rendered bytes of every implemented command across
// TTY, plain, and JSON projections, for success and error anatomies alike.
func TestGoldens(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	otherCRID := apitest.DeriveCRID(t, []byte("a-different-resource-key"), apitest.VersionTest)

	cases := []goldenCase{
		{
			name:         "publish",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// Publishing a URL that already has an active resource: the
			// text document says so itself, so stderr stays empty.
			name:         "publish_existing",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			variants:     []string{"tty", "plain"},
			stdoutGolden: true,
		},
		{
			// The JSON document only gains found_existing: true; the replay
			// note is a stderr status line.
			name:         "publish_existing",
			args:         func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			variants:     []string{"json"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// --public: the Access row says so in plain words and the JSON
			// document says private: false. Nothing else differs.
			name: "publish_public",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--public"}
			},
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// The target is already published as public, as a release that
			// published as public by default left it, and this publish names
			// no privacy: the resource is kept, the Access row says public,
			// and the document warns that it stays public.
			name: "publish_kept_public",
			args: func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare: func(srv *apitest.Server) {
				srv.SetResourceAccess(false)
				srv.SetPublishFoundExisting(true)
			},
			variants:     []string{"tty", "plain"},
			stdoutGolden: true,
		},
		{
			// The script-facing form of the same publish: the JSON document
			// says private: false, kept_public: true and found_existing:
			// true, and the warning is on stderr.
			name: "publish_kept_public_script",
			args: func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare: func(srv *apitest.Server) {
				srv.SetResourceAccess(false)
				srv.SetPublishFoundExisting(true)
			},
			variants:     []string{"json"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// The target is already published as public and this publish
			// names devices to allow, which only a private resource has:
			// exit 7, what exists, and the two things the publisher can do.
			// Nothing on stdout.
			name: "error_publish_existing_public",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-device-key", goldenDevicePublicKey}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetResourceAccess(false)
				srv.SetPublishFoundExisting(true)
			},
			variants:     []string{"tty", "plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// The target is already published as private with another list
			// of allowed devices: exit 7, and the commands that change it.
			name: "error_publish_other_devices",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-device-key", goldenDevicePublicKey}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetResourceAccess(true, goldenSecondDevicePublicKey)
				srv.SetPublishFoundExisting(true)
			},
			variants:     []string{"plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// The other direction: --public for a target that is private.
			name: "error_publish_existing_private",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--public"}
			},
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			variants:     []string{"plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// An older service refuses a different device list with the answer
			// it gives a privacy difference, so the message claims neither.
			name: "error_publish_access_differs",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-device-key", goldenDevicePublicKey}
			},
			prepare: func(srv *apitest.Server) {
				srv.PlayPublicByDefault()
				srv.SetPublishFoundExisting(true)
			},
			variants:     []string{"plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// The create answer does not say the resource is private: exit 10,
			// no CRID anywhere, and what the publisher should check.
			name: "error_publish_unconfirmed",
			args: func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active",
					}, nil)
				})
			},
			variants:     []string{"plain"},
			wantCode:     10,
			stderrGolden: true,
		},
		{
			// With no flag, grants prints the resource with its current list.
			name:         "grants",
			args:         func(srv *apitest.Server) []string { return []string{"grants", srv.Key.CRID} },
			prepare:      func(srv *apitest.Server) { srv.SetResourceAccess(true, goldenDevicePublicKey) },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// Adding one device prints the complete list that results.
			name: "grants_add",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--add", goldenSecondDevicePublicKey}
			},
			prepare:      func(srv *apitest.Server) { srv.SetResourceAccess(true, goldenDevicePublicKey) },
			variants:     []string{"plain", "json"},
			stdoutGolden: true,
		},
		{
			// On a public resource the list is shown and a note says that it
			// has no effect.
			name:         "grants_public",
			args:         func(srv *apitest.Server) []string { return []string{"grants", srv.Key.CRID} },
			prepare:      func(srv *apitest.Server) { srv.SetResourceAccess(false) },
			variants:     []string{"plain"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// A service from before single grants answers with the list as it
			// was: exit 10 and nothing on stdout.
			name: "error_grants_unconfirmed",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--add", goldenDevicePublicKey}
			},
			prepare:      func(srv *apitest.Server) { srv.PlayNoSingleGrantEdits() },
			variants:     []string{"plain"},
			wantCode:     10,
			stderrGolden: true,
		},
		{
			// The flag that replaced the complete list is a usage error that
			// names --add.
			name: "error_grants_replace_flag",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--allow-device-key", goldenDevicePublicKey}
			},
			variants:     []string{"plain"},
			wantCode:     2,
			stderrGolden: true,
		},
		{
			// Publishing with access requests on, on an install that knows
			// its link site: the document says what to send to people and
			// what happens next, before the CRID line.
			name: "publish_requests",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			linkSite:     testLinkSite,
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// The same on an install that does not know its link site: the
			// CRID is what to send, and no address is named.
			name: "publish_requests_no_site",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			variants:     []string{"plain", "json"},
			stdoutGolden: true,
		},
		{
			// The target is already published as a private resource with
			// access requests off: the publish turns them on, and the
			// document says so in one line before what to send to people.
			name: "publish_requests_existing",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			linkSite:     testLinkSite,
			variants:     []string{"tty", "plain"},
			stdoutGolden: true,
		},
		{
			// The JSON document keeps its shape; the one line is on stderr.
			name: "publish_requests_existing",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			prepare:      func(srv *apitest.Server) { srv.SetPublishFoundExisting(true) },
			linkSite:     testLinkSite,
			variants:     []string{"json"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// The same target, and the change that turns access requests on
			// is refused for now: the failure's own exit code, what exists,
			// why, and the command that tries again with the CRID in it.
			name: "error_publish_requests_not_turned_on",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetPublishFoundExisting(true)
				srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
				})
			},
			linkSite:     testLinkSite,
			variants:     []string{"tty", "plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// A service without access requests that refuses the setting it
			// does not know: exit 11, the message every access-request
			// command gives there, and that nothing was published.
			name: "error_publish_requests_refused",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			prepare:      func(srv *apitest.Server) { srv.PlayStrictWithoutAccessRequests() },
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// The same service, asked to turn access requests on for a
			// resource.
			name: "error_requests_on_refused",
			args: func(srv *apitest.Server) []string {
				return []string{"requests", srv.Key.CRID, "--on"}
			},
			prepare:      func(srv *apitest.Server) { srv.PlayStrictWithoutAccessRequests() },
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// A service from before access requests: exit 11 and no CRID.
			name: "error_publish_requests_unsupported",
			args: func(*apitest.Server) []string {
				return []string{"publish", "https://example.com/data", "--allow-requests"}
			},
			prepare:      func(srv *apitest.Server) { srv.PlayNoAccessRequests() },
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// The pending requests of every resource, with no code, ending
			// with the line on how a person is let in.
			name:         "requests",
			args:         func(*apitest.Server) []string { return []string{"requests"} },
			prepare:      twoRequests,
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// The same listing from a service that says there may be more
			// requests than it sent: a line after the rows that says what to
			// do, and has_more in the document.
			name: "requests_more",
			args: func(*apitest.Server) []string { return []string{"requests"} },
			prepare: func(srv *apitest.Server) {
				twoRequests(srv)
				srv.SetAccessRequestsHasMore(true)
			},
			variants:     []string{"plain", "json"},
			stdoutGolden: true,
		},
		{
			name:         "requests_one",
			args:         func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} },
			prepare:      twoRequests,
			variants:     []string{"plain"},
			stdoutGolden: true,
		},
		{
			// Nothing pending: a note on stderr and nothing on stdout.
			name:         "requests_none",
			args:         func(*apitest.Server) []string { return []string{"requests"} },
			variants:     []string{"plain"},
			stderrGolden: true,
		},
		{
			name:         "requests_on",
			args:         func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--on"} },
			linkSite:     testLinkSite,
			variants:     []string{"plain", "json"},
			stdoutGolden: true,
		},
		{
			name: "requests_off",
			args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--off"} },
			prepare: func(srv *apitest.Server) {
				srv.SetAccessRequests(true)
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, otherRequester)
			},
			variants:     []string{"plain"},
			stdoutGolden: true,
		},
		{
			name:         "error_requests_unsupported",
			args:         func(*apitest.Server) []string { return []string{"requests"} },
			prepare:      func(srv *apitest.Server) { srv.PlayNoAccessRequests() },
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// An approval: who now has access, and the command that takes it
			// away again.
			name:         "approve",
			args:         func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, "482 913"} },
			prepare:      twoRequests,
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// A code that is not pending for the resource: exit 5 and a
			// message about the code.
			name:         "error_approve_not_pending",
			args:         func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, "000000"} },
			prepare:      twoRequests,
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// A value that can never be a code: exit 8 before any request.
			name:         "error_approve_code",
			args:         func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, "Ana Lopez"} },
			variants:     []string{"plain"},
			wantCode:     8,
			stderrGolden: true,
		},
		{
			// A denial by the device id the listing shows.
			name:         "deny",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requesterDevice} },
			prepare:      twoRequests,
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "deny",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requesterDevice} },
			prepare:      twoRequests,
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// A denial by a code its publisher was given.
			name:         "deny_code",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, "482913"} },
			prepare:      twoRequests,
			variants:     []string{"plain"},
			stderrGolden: true,
		},
		{
			name:         "deny_code",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, "482913"} },
			prepare:      twoRequests,
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// A device id with no pending request: exit 5 and a message
			// about the device id.
			name:         "error_deny_not_pending",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, goldenThirdDevice} },
			prepare:      twoRequests,
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// A value that is neither a device id nor a code: exit 8 before
			// any request.
			name:         "error_deny_request",
			args:         func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, "Ana Lopez"} },
			variants:     []string{"plain"},
			wantCode:     8,
			stderrGolden: true,
		},
		{
			// grants with approved people beside the device keys.
			name: "grants_people",
			args: func(srv *apitest.Server) []string { return []string{"grants", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.SetResourceAccess(true, goldenDevicePublicKey)
				srv.SetAccessRequests(true)
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, "")
			},
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// Removing a device id that is not on the list: exit 5, and the
			// message says that nothing was removed.
			name: "error_grants_remove_unknown",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", otherDevice}
			},
			prepare:      func(srv *apitest.Server) { srv.AddApprovedPerson(requesterDevice, requesterName) },
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// Two device ids, the second not on the list. The list is read
			// before any access is taken away, so nothing was removed, and
			// the message says who still has access.
			name: "error_grants_remove_one_unknown",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}
			},
			prepare:      func(srv *apitest.Server) { srv.AddApprovedPerson(requesterDevice, requesterName) },
			variants:     []string{"tty", "plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// The same in JSON mode: the outcome is also a document, with
			// every device id in one of its three arrays.
			name: "error_grants_remove_one_unknown_script",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}
			},
			prepare:      func(srv *apitest.Server) { srv.AddApprovedPerson(requesterDevice, requesterName) },
			variants:     []string{"json"},
			wantCode:     5,
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// A person is gone by the time they are removed, after another
			// was removed: the message says from whom access was taken away,
			// who was not found and who still has access.
			name: "error_grants_remove_part_way",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice, "--remove", goldenThirdDevice}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, otherRequester)
				srv.AddApprovedPerson(goldenThirdDevice, "")
				srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusNotFound, "not_found", "Not Found", "no such approved person")
				})
			},
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// The script-facing form of the same outcome.
			name: "error_grants_remove_part_way_script",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice, "--remove", goldenThirdDevice}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, otherRequester)
				srv.AddApprovedPerson(goldenThirdDevice, "")
				srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusNotFound, "not_found", "Not Found", "no such approved person")
				})
			},
			variants:     []string{"json"},
			wantCode:     5,
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// The service answers a removal as made, and its list still
			// shows the person: who lost access, and who still has it.
			name: "error_grants_remove_still_listed",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, otherRequester)
				srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})
			},
			variants:     []string{"plain"},
			wantCode:     10,
			stderrGolden: true,
		},
		{
			// The script-facing form of the same outcome.
			name: "error_grants_remove_still_listed_script",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.AddApprovedPerson(otherDevice, otherRequester)
				srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				})
			},
			variants:     []string{"json"},
			wantCode:     10,
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// Every person is removed, and then the change to the public
			// keys fails: who lost access, why the key change failed, and
			// the command that makes the key change alone.
			name: "error_grants_remove_keys_failed",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--add", goldenDevicePublicKey}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
				})
			},
			variants:     []string{"tty", "plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			// The script-facing form of the same outcome.
			name: "error_grants_remove_keys_failed_script",
			args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--add", goldenDevicePublicKey}
			},
			prepare: func(srv *apitest.Server) {
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
				})
			},
			variants:     []string{"json"},
			wantCode:     11,
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// A terminal gets the publisher and creation date with the link on
			// stdout; JSON carries them in the document. Neither writes stderr.
			name:         "share",
			args:         func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			variants:     []string{"tty", "json"},
			stdoutGolden: true,
		},
		{
			// Piped: stdout stays exactly the bare link, and the publisher is
			// one notice line on stderr.
			name:         "share",
			args:         func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			variants:     []string{"plain"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// The publisher set no name: the fixed text stands in for it, and
			// the status word is unchanged.
			name:         "share_unnamed",
			args:         func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare:      func(srv *apitest.Server) { srv.SetPublisherName("") },
			variants:     []string{"tty", "json"},
			stdoutGolden: true,
		},
		{
			name:         "share_unnamed",
			args:         func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare:      func(srv *apitest.Server) { srv.SetPublisherName("") },
			variants:     []string{"plain"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// status on a URL resource: the sharing-state read answers that
			// this is not a Connector, then the resource detail carries the
			// publisher and creation date.
			name:         "status_url",
			args:         func(srv *apitest.Server) []string { return []string{"status", srv.Key.CRID} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "publisher",
			args:         func(*apitest.Server) []string { return []string{"publisher"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			// Setting a name prints the row recipients will see; the
			// confirmation is a status note on stderr.
			name:         "publisher_set",
			args:         func(*apitest.Server) []string { return []string{"publisher", "set", "Northwind Labs"} },
			variants:     []string{"tty", "plain"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			name:         "publisher_set",
			args:         func(*apitest.Server) []string { return []string{"publisher", "set", "Northwind Labs"} },
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			name:         "publisher_clear",
			args:         func(*apitest.Server) []string { return []string{"publisher", "clear"} },
			variants:     []string{"plain"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// The service refuses a name: exit 8 with its reason, and the
			// name itself is not echoed.
			name:         "error_publisher_name",
			args:         func(*apitest.Server) []string { return []string{"publisher", "set", "Acme Verified"} },
			variants:     []string{"plain"},
			wantCode:     8,
			stderrGolden: true,
		},
		{
			name:         "list",
			args:         func(*apitest.Server) []string { return []string{"list"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "delete",
			args:         func(srv *apitest.Server) []string { return []string{"delete", srv.Key.CRID, "--yes"} },
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "delete",
			args:         func(srv *apitest.Server) []string { return []string{"delete", srv.Key.CRID, "--yes"} },
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// The share operator's not-found, which share and get both get,
			// has its own hint: a 404 for a CRID also covers a device that is
			// not allowed.
			name: "error_share_notfound",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/share",
					apitest.HandlerNotFound404(t, "resource_not_found"))
			},
			variants:     []string{"tty", "plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			// Every other route's not-found keeps the hint they all share.
			// status stands in for them here.
			name: "error_notfound",
			args: func(srv *apitest.Server) []string { return []string{"status", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/resources/"+key.CRID+"/sharing",
					apitest.HandlerNotFound404(t, "not_found"))
			},
			variants:     []string{"tty", "plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_revoked",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/share", apitest.HandlerRevoked400(t))
			},
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_retired",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/share", apitest.HandlerTombstoned410(t))
			},
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
		{
			name: "error_dark503",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/share", apitest.HandlerDark503(t))
			},
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			name: "error_connector_stopped",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+key.CRID+"/share", apitest.HandlerConnectorStopped503(t))
			},
			variants:     []string{"plain"},
			wantCode:     11,
			stderrGolden: true,
		},
		{
			name: "error_verify_mismatch",
			args: func(srv *apitest.Server) []string { return []string{"share", srv.Key.CRID} },
			prepare: func(srv *apitest.Server) {
				srv.SetShareCRID(otherCRID)
			},
			variants:     []string{"plain"},
			wantCode:     12,
			stderrGolden: true,
		},
		{
			// Legacy injected account-client error rendering, not default enrollment.
			name: "error_nokey",
			args: func(*apitest.Server) []string { return []string{"list"} },
			env: func(srv *apitest.Server) map[string]string {
				return map[string]string{"QURL_ENDPOINT": srv.URL}
			},
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			name:         "whoami",
			args:         func(*apitest.Server) []string { return []string{"whoami"} },
			variants:     goldenVariants(),
			stdoutGolden: true,
		},
		{
			name:         "login",
			args:         func(*apitest.Server) []string { return []string{"login"} },
			stdin:        testAPIKey + "\n",
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name:         "login",
			args:         func(*apitest.Server) []string { return []string{"login"} },
			stdin:        testAPIKey + "\n",
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// login with a key the platform does not recognize: exit 4.
			name: "error_login_invalid",
			args: func(*apitest.Server) []string { return []string{"login"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
			},
			stdin:        testAPIKey + "\n",
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			// login with an expired key: exit 4 and the new-key remedy.
			name: "error_login_expired",
			args: func(*apitest.Server) []string { return []string{"login"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyExpired401(t))
			},
			stdin:        testAPIKey + "\n",
			variants:     []string{"plain"},
			wantCode:     4,
			stderrGolden: true,
		},
		{
			// A frozen account is an account-standing condition (exit 6 with
			// the standing message), not a generic forbidden.
			name: "error_frozen",
			args: func(*apitest.Server) []string { return []string{"whoami"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAccountFrozen403(t))
			},
			variants:     []string{"plain"},
			wantCode:     6,
			stderrGolden: true,
		},
		{
			name: "error_scope",
			args: func(*apitest.Server) []string { return []string{"whoami"} },
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerInsufficientScope403(t))
			},
			variants:     []string{"plain"},
			wantCode:     6,
			stderrGolden: true,
		},
		{
			name: "error_ratelimited",
			args: func(*apitest.Server) []string { return []string{"list"} },
			prepare: func(srv *apitest.Server) {
				srv.ScriptRepeat(http.MethodGet, "/v1/resources", 3, apitest.Handler429(t, 2))
			},
			variants:     []string{"plain"},
			wantCode:     9,
			stderrGolden: true,
		},
		{
			// Browser mode: the verified link plus expiry on stdout, the
			// launch note on stderr. TTY-only by contract — the piped
			// variant of a bare get is error_get_piped below.
			name:         "get_browser",
			args:         func(srv *apitest.Server) []string { return []string{"get", srv.Key.CRID} },
			variants:     []string{"tty"},
			stdoutGolden: true,
			stderrGolden: true,
		},
		{
			// Download mode: the confirmation goes to stderr; stdout stays
			// data-free. The relative --file path keeps the message
			// deterministic under chdirTemp.
			name: "get_file",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetShareQURL(srv.URL + apitest.DownloadPath)
			},
			chdirTemp:    true,
			variants:     []string{"tty", "plain"},
			stderrGolden: true,
		},
		{
			name: "get_file",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetShareQURL(srv.URL + apitest.DownloadPath)
			},
			chdirTemp:    true,
			variants:     []string{"json"},
			stdoutGolden: true,
		},
		{
			// The §16.2 refusal: piped stdout with no --file.
			name:         "error_get_piped",
			args:         func(srv *apitest.Server) []string { return []string{"get", srv.Key.CRID} },
			variants:     []string{"plain"},
			wantCode:     2,
			stderrGolden: true,
		},
		{
			// Overwrite refusal (exit 7, the Conflict row) fires before any
			// request; the golden pins the --force remedy wording.
			name: "error_get_exists",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			chdirTemp: true,
			setup: func(t *testing.T) {
				if err := os.WriteFile("out.bin", []byte("already here"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			variants:     []string{"plain"},
			wantCode:     7,
			stderrGolden: true,
		},
		{
			// Expiry that outlives the single automatic refresh: two 410s
			// from the link host, exit 5.
			name: "error_get_expired",
			args: func(srv *apitest.Server) []string {
				return []string{"get", srv.Key.CRID, "--file", "out.bin"}
			},
			prepare: func(srv *apitest.Server) {
				srv.SetShareQURL(srv.URL + apitest.DownloadPath)
				srv.ScriptRepeat(http.MethodGet, apitest.DownloadPath, 2,
					func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
			},
			chdirTemp:    true,
			variants:     []string{"plain"},
			wantCode:     5,
			stderrGolden: true,
		},
	}

	// Anchor the golden tree before any case changes the working directory.
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatalf("locate golden dir: %v", err)
	}

	for _, tc := range cases {
		for _, variant := range tc.variants {
			t.Run(tc.name+"_"+variant, func(t *testing.T) {
				if tc.chdirTemp {
					t.Chdir(t.TempDir())
				}
				if tc.setup != nil {
					tc.setup(t)
				}
				srv := apitest.NewServerWithKey(t, apitest.FixedResourceKey(t))
				if tc.prepare != nil {
					tc.prepare(srv)
				}

				args := tc.args(srv)
				env := map[string]string{"QURL_API_KEY": testAPIKey}
				if tc.env != nil {
					env = tc.env(srv)
				}
				o := &runOpts{
					args:     append([]string{"--endpoint", srv.URL}, args...),
					env:      env,
					tty:      variant == "tty",
					linkSite: tc.linkSite,
				}
				if tc.stdin != "" {
					o.stdin = strings.NewReader(tc.stdin)
				}
				if variant == "json" {
					o.args = append(o.args, "-o", "json")
				}
				res := runCLI(t, o)

				if res.code != tc.wantCode {
					t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
						res.code, tc.wantCode, res.stdout.String(), res.stderr.String())
				}
				if tc.stdoutGolden {
					clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".golden"), res.stdout.Bytes())
				} else {
					mustEmptyStdout(t, res)
				}
				if tc.stderrGolden {
					clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".stderr.golden"), res.stderr.Bytes())
				} else if res.stderr.Len() != 0 {
					t.Fatalf("stderr must be empty for %s_%s, got %q", tc.name, variant, res.stderr.String())
				}
			})
		}
	}
}

// TestGoldenFilesAreLFOnly is the CRLF sentinel: no golden file may contain
// a carriage return, on any platform, ever. The repo .gitattributes protects
// checkout; this protects authoring.
func TestGoldenFilesAreLFOnly(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden files found; the sentinel would be vacuous")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, entry.Name())))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if i := bytes.IndexByte(data, '\r'); i >= 0 {
			t.Errorf("%s contains a carriage return at byte %d; goldens are LF-only", entry.Name(), i)
		}
	}
}
