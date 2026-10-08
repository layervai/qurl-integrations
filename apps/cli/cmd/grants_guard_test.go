package main

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// productionGrants returns the run options for `qurl grants` pointed at the
// production endpoint, with the API client replaced by one for the mock, so
// the command sees a production endpoint and no request leaves the machine.
func productionGrants(t *testing.T, srv *apitest.Server, args ...string) *runOpts {
	t.Helper()
	return &runOpts{
		args: append([]string{"--endpoint", "https://api.layerv.ai", "grants"}, args...),
		openAPIClient: func(context.Context) (qurlapi.Client, error) {
			return qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "test"})
		},
	}
}

// TestGrantsReadNeedsNoConfirmationForATestCRID pins which grants commands
// the guard for a test CRID on the production endpoint applies to. Reading
// the list acts on nothing, so like status and inspect it needs no --yes and
// draws no warning. A change is refused before any request without --yes and
// goes through, with the warning, with it. An operand that is not a CRID is
// refused locally in both cases.
func TestGrantsReadNeedsNoConfirmationForATestCRID(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, grantKey(1))
		res := runCLI(t, productionGrants(t, srv, srv.Key.CRID))
		if res.code != 0 || res.stderr.Len() != 0 || !strings.Contains(res.stdout.String(), grantKey(1)) {
			t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
		}
		if log := sentLog(srv); !slices.Equal(log, []string{"GET /v1/resources/" + srv.Key.CRID}) {
			t.Fatalf("requests = %v, want the one read", log)
		}
	})
	for _, change := range [][]string{{"--add", grantKey(2)}, {"--remove", grantKey(1)}, {"--clear"}} {
		t.Run("change without --yes "+change[0], func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetResourceAccess(true, grantKey(1))
			res := runCLI(t, productionGrants(t, srv, append([]string{srv.Key.CRID}, change...)...))
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), "Re-run with --yes") || len(srv.Requests()) != 0 {
				t.Fatalf("exit %d, %d requests, stderr %q", res.code, len(srv.Requests()), res.stderr.String())
			}
			mustEmptyStdout(t, res)
		})
	}
	t.Run("change with --yes", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, grantKey(1))
		res := runCLI(t, productionGrants(t, srv, srv.Key.CRID, "--add", grantKey(2), "--yes"))
		if res.code != 0 || !strings.Contains(res.stderr.String(), "because --yes was given") {
			t.Fatalf("exit %d, stderr %q", res.code, res.stderr.String())
		}
		if log := sentLog(srv); !slices.Equal(log, []string{"PATCH /v1/resources/" + srv.Key.CRID}) {
			t.Fatalf("requests = %v, want the one change", log)
		}
	})
	for _, flags := range [][]string{nil, {"--add", grantKey(2)}} {
		srv := apitest.NewServer(t)
		res := runCLI(t, productionGrants(t, srv, append([]string{"not-a-crid"}, flags...)...))
		if res.code != exitcode.InvalidInput || len(srv.Requests()) != 0 {
			t.Fatalf("flags %v: exit %d, %d requests, stderr %q", flags, res.code, len(srv.Requests()), res.stderr.String())
		}
	}
}

// TestGrantsKeyLimitIsPerFlagAndTheListLimitIsTheServices pins what the
// command checks about the 256 limit and what it leaves to the service, as
// its help says. Each of --add and --remove takes at most 256 keys, checked
// before anything is sent. The two together may name more than 256 keys in
// one change, because what is limited is the list that results, and only the
// service knows that list.
func TestGrantsKeyLimitIsPerFlagAndTheListLimitIsTheServices(t *testing.T) {
	keys := func(marker byte, count int) []string {
		out := make([]string, 0, count)
		for index := range count {
			raw := make([]byte, devicePublicKeySize)
			raw[0], raw[1], raw[2] = marker, byte(index), byte(index>>8)
			out = append(out, base64.StdEncoding.EncodeToString(raw))
		}
		return out
	}
	flagged := func(flag string, values []string) []string {
		args := make([]string, 0, 2*len(values))
		for _, value := range values {
			args = append(args, flag, value)
		}
		return args
	}

	for _, flag := range []string{"--add", "--remove"} {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, flagged(flag, keys(1, 257))...)})
		if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), flag+" accepts at most 256 keys") || len(srv.Requests()) != 0 {
			t.Fatalf("%s with 257 keys: exit %d, %d requests, stderr %q", flag, res.code, len(srv.Requests()), res.stderr.String())
		}
	}

	// 200 keys off and 200 other keys on: 400 keys in one command, and a
	// list of 200 after it. It is one change, and it is sent.
	stored, added := keys(1, 200), keys(2, 200)
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, stored...)
	args := append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}, flagged("--remove", stored)...)
	res := runCLI(t, &runOpts{args: append(args, flagged("--add", added)...)})
	if res.code != 0 {
		t.Fatalf("a change of 400 keys: exit %d, stderr %q", res.code, res.stderr.String())
	}
	got := decodeGrants(t, res).AllowedDeviceKeys
	slices.Sort(got)
	slices.Sort(added)
	if !slices.Equal(got, added) || len(grantRequests(t, srv)) != 1 {
		t.Fatalf("list after the change has %d keys in %d changes, want the 200 added keys in one", len(got), len(grantRequests(t, srv)))
	}

	// A list that would end over the limit is the service's refusal, and
	// the list stays as it was.
	srv = apitest.NewServer(t)
	srv.SetResourceAccess(true, keys(1, 200)...)
	res = runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, flagged("--add", keys(2, 57))...)})
	if res.code != exitcode.InvalidInput || !strings.Contains(res.stderr.String(), "at most 256 allowed device keys") {
		t.Fatalf("a list that would hold 257 keys: exit %d, stderr %q", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if changes := grantRequests(t, srv); len(changes) != 1 {
		t.Fatalf("changes sent = %d, want the one the service refused", len(changes))
	}
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
	if kept := decodeGrants(t, res).AllowedDeviceKeys; len(kept) != 200 {
		t.Fatalf("list after the refused change has %d keys, want the 200 it had", len(kept))
	}

	help := runCLI(t, &runOpts{args: []string{"grants", "--help"}})
	collapsed := strings.Join(strings.Fields(help.stdout.String()), " ")
	for _, want := range []string{
		"A list holds at most 256 devices.",
		"--add and --remove each take at most 256 public keys in one command, which is checked before anything is sent.",
		"The limit on the list that results is the service's",
		"reading the list never needs it",
	} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("grants help lacks %q:\n%s", want, help.stdout.String())
		}
	}
}
