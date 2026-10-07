package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// publishTargetState is what a publish finds for its target.
type publishTargetState struct {
	name string
	// exists says the create finds a resource; private and requests are what
	// that resource is like.
	exists, private, requests bool
}

// publishOutcome is what one publish does and says.
type publishOutcome struct {
	// code is the exit code.
	code int
	// creates is the number of create requests; change says that the one
	// change that turns access requests on was sent as well.
	creates int
	change  bool
	// private and requests are what the resource is like afterwards.
	private, requests bool
	// headline opens the text document; it is empty for a conflict.
	headline string
	// warning, turnedOn and guidance say which of the three extra texts the
	// document has: the kept-public warning, the line that access requests
	// were turned on, and what to send to people.
	warning, turnedOn, guidance bool
	// stderr is a part of the conflict message.
	stderr string
}

// TestPublishAccessFlagsAgainstEveryTarget pins how the privacy default, the
// kept public resource and access requests meet, as one table: each of no
// flag, --allow-requests and --public against a target that is new, one that
// exists as private with access requests off, one that exists as private
// with them on, and one that exists as public. Every cell pins the exit code,
// the requests sent, what the resource is like afterwards, and what the text
// document says, for a remote URL and for a local app.
//
// The rule the table shows: a publish gives what its flags named or fails. A
// flag that the existing resource cannot be given is a conflict and changes
// nothing. Only a publish with no flag takes the resource as it is, and only
// --allow-requests changes one, in the one way a private resource can be
// changed after it was made.
func TestPublishAccessFlagsAgainstEveryTarget(t *testing.T) {
	const (
		published = "Published\n"
		already   = "Already published\n"
	)
	targets := []publishTargetState{
		{name: "new", private: true},
		{name: "private, requests off", exists: true, private: true},
		{name: "private, requests on", exists: true, private: true, requests: true},
		{name: "public", exists: true},
	}
	want := map[string][]publishOutcome{
		"no flag": {
			{creates: 1, private: true, headline: published},
			{creates: 1, private: true, headline: already},
			// Reused as it is: the resource takes requests, so the document
			// says what to send, and nothing was sent to make it so.
			{creates: 1, private: true, requests: true, headline: already, guidance: true},
			// Kept: the create that states the default is refused, and the
			// same create without a privacy returns the public resource.
			{creates: 2, headline: already, warning: true},
		},
		"--allow-requests": {
			{creates: 1, private: true, requests: true, headline: published, guidance: true},
			{creates: 1, change: true, private: true, requests: true, headline: already, turnedOn: true, guidance: true},
			{creates: 1, private: true, requests: true, headline: already, guidance: true},
			{code: exitcode.Conflict, creates: 1, stderr: "this target is already published as public"},
		},
		"--public": {
			{creates: 1, headline: published},
			{code: exitcode.Conflict, creates: 1, private: true, stderr: "this target is already published as private"},
			{code: exitcode.Conflict, creates: 1, private: true, requests: true, stderr: "this target is already published as private"},
			{creates: 1, headline: already},
		},
	}

	for _, flag := range []string{"no flag", "--allow-requests", "--public"} {
		for index, target := range targets {
			outcome := want[flag][index]
			for _, local := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/local=%t", flag, target.name, local), func(t *testing.T) {
					srv := apitest.NewServer(t)
					srv.SetResourceAccess(target.private)
					srv.SetAccessRequests(target.requests)
					srv.SetPublishFoundExisting(target.exists)
					var flags []string
					if flag != "no flag" {
						flags = []string{flag}
					}
					opts := &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, flags...)}
					switch {
					case local && outcome.code != 0:
						opts = refusingLocalPublish(t, srv, flags...)
					case local:
						opts = servingLocalPublish(t, srv, target.exists, flags...)
					}
					opts.linkSite = testLinkSite
					res := runCLI(t, opts)
					if res.code != outcome.code {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, outcome.code, res.stderr.String())
					}

					// What was sent: the creates first, then the one change
					// when there is one, and never either of them again.
					log := requestLog(srv)
					change := "PATCH /v1/resources/" + srv.Key.CRID
					sent := slices.Repeat([]string{"POST /v1/resources"}, outcome.creates)
					if outcome.change {
						sent = append(sent, change)
					}
					if len(log) < len(sent) || !slices.Equal(log[:len(sent)], sent) {
						t.Fatalf("requests = %v, want them to start with %v", log, sent)
					}
					for _, later := range log[len(sent):] {
						if later == "POST /v1/resources" || later == change || strings.HasPrefix(later, "DELETE ") {
							t.Fatalf("requests = %v, want no create, change or delete after %v", log, sent)
						}
					}
					if !local && len(log) != len(sent) {
						t.Fatalf("requests = %v, want exactly %v", log, sent)
					}

					stdout, stderr := res.stdout.String(), res.stderr.String()
					if outcome.code != 0 {
						mustEmptyStdout(t, res)
						if !strings.Contains(stderr, outcome.stderr) || strings.Contains(stderr, srv.Key.CRID) {
							t.Fatalf("stderr lacks %q or names the CRID:\n%s", outcome.stderr, stderr)
						}
					} else {
						if stderr != "" {
							t.Errorf("text mode wrote to stderr: %q", stderr)
						}
						accessRow := publicAccessRow
						if outcome.private {
							accessRow = privateAccessRow
						}
						if !strings.HasPrefix(stdout, outcome.headline) || !strings.Contains(publishRows(stdout), "\n"+accessRow+"\n") || !strings.HasSuffix(stdout, "\nCRID: "+srv.Key.CRID+"\n") {
							t.Errorf("document lacks the headline %q, the row %q, or its last CRID line:\n%s", outcome.headline, accessRow, stdout)
						}
						for text, shown := range map[string]bool{
							"Warning: this target was published as public before, and it stays public": outcome.warning,
							turnedOnLine: outcome.turnedOn,
							"People can ask you for access to this resource.": outcome.guidance,
						} {
							if strings.Contains(stdout, text) != shown {
								t.Errorf("document shows %q = %t, want %t:\n%s", text, !shown, shown, stdout)
							}
						}
					}

					// What the resource is like afterwards. A conflict leaves
					// it as it was found.
					state := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
					var document struct {
						Private        *bool `json:"private"`
						AccessRequests *bool `json:"access_requests"`
					}
					if err := json.Unmarshal(state.stdout.Bytes(), &document); state.code != 0 || err != nil || document.Private == nil || document.AccessRequests == nil {
						t.Fatalf("grants -o json: exit %d, %v: %s%s", state.code, err, state.stdout.String(), state.stderr.String())
					}
					if *document.Private != outcome.private || *document.AccessRequests != outcome.requests {
						t.Fatalf("resource afterwards: private %t, access requests %t; want private %t, access requests %t", *document.Private, *document.AccessRequests, outcome.private, outcome.requests)
					}
				})
			}
		}
	}
}

// TestPublishNamesAccessCountsEveryAccessFlag pins which flags make a publish
// one that named who may open the resource. Only a publish that named
// nothing keeps a target that is already published as public. Each of these
// flags is a choice in any form it is given, an explicit false included, and
// --allow-requests is one of them: access requests are for a private
// resource. A flag about anything else is not.
func TestPublishNamesAccessCountsEveryAccessFlag(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{args: nil},
		{args: []string{"--description", "d"}},
		{args: []string{"--tag", "t", "--alias", "a"}},
		{args: []string{"--id", "local-app", "--foreground"}},
		{args: []string{"--public"}, want: true},
		{args: []string{"--public=false"}, want: true},
		{args: []string{"--private"}, want: true},
		{args: []string{"--allow-device-key", goldenDevicePublicKey}, want: true},
		{args: []string{"--allow-requests"}, want: true},
		{args: []string{"--allow-requests=false"}, want: true},
		{args: []string{"--description", "d", "--allow-requests"}, want: true},
	} {
		cmd := publishCmd(&globalOpts{})
		if err := cmd.ParseFlags(test.args); err != nil {
			t.Fatalf("flags %v: %v", test.args, err)
		}
		if got := publishNamesAccess(cmd); got != test.want {
			t.Errorf("flags %v: names access = %t, want %t", test.args, got, test.want)
		}
	}
}
