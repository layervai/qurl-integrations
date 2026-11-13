package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// originSecret is the value every test sends. It is distinctive, so a test
// that looks for it in output finds only a real leak.
const originSecret = "bypass-7f3c9a1e-secret"

// sentOriginHeaders returns the origin_header member of every create and
// PATCH request, as raw JSON; "<absent>" when a request had none.
func sentOriginHeaders(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	var sent []string
	for _, req := range srv.Requests() {
		create := req.Method == http.MethodPost && req.Path == "/v1/resources"
		if !create && req.Method != http.MethodPatch {
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		member, ok := body["origin_header"]
		if !ok {
			sent = append(sent, "<absent>")
			continue
		}
		sent = append(sent, string(member))
	}
	return sent
}

// assertNoSecret fails when the value reached stdout or stderr.
func assertNoSecret(t *testing.T, res *runResult) {
	t.Helper()
	for name, text := range map[string]string{"stdout": res.stdout.String(), "stderr": res.stderr.String()} {
		if strings.Contains(text, originSecret) {
			t.Fatalf("%s shows the origin header value:\n%s", name, text)
		}
	}
}

func originPublishArgs(srv *apitest.Server, extra ...string) []string {
	return append([]string{"--endpoint", srv.URL, "publish", "https://app.example.vercel.app", "--name", "App"}, extra...)
}

func TestPublishOriginHeaderSendsTheValueFromStdinAndShowsOnlyTheName(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{
				args:  append([]string{"-o", format, "-v"}, originPublishArgs(srv, "--origin-header", "x-vercel-protection-bypass")...),
				stdin: strings.NewReader(originSecret + "\n"),
			})
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.stderr.String())
			}
			want := fmt.Sprintf(`{"name":"x-vercel-protection-bypass","value":%q}`, originSecret)
			if got := sentOriginHeaders(t, srv); len(got) != 1 || got[0] != want {
				t.Fatalf("sent origin headers = %q, want [%s]", got, want)
			}
			if name, value := srv.OriginHeader(); name != "X-Vercel-Protection-Bypass" || value != originSecret {
				t.Fatalf("stored origin header = %q", name)
			}
			assertNoSecret(t, res)
			switch format {
			case "json":
				var doc struct {
					OriginHeaderName string `json:"origin_header_name"`
				}
				if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil || doc.OriginHeaderName != "X-Vercel-Protection-Bypass" {
					t.Fatalf("origin_header_name = %q, %v:\n%s", doc.OriginHeaderName, err, res.stdout.String())
				}
			default:
				if !strings.Contains(res.stdout.String(), "Origin header:") || !strings.Contains(res.stdout.String(), "X-Vercel-Protection-Bypass") {
					t.Fatalf("no origin header row:\n%s", res.stdout.String())
				}
			}
		})
	}
}

func TestPublishWithoutOriginHeaderSendsNone(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: originPublishArgs(srv)})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if got := sentOriginHeaders(t, srv); len(got) != 1 || got[0] != "<absent>" {
		t.Fatalf("sent origin headers = %q", got)
	}
	if strings.Contains(res.stdout.String(), "Origin header:") {
		t.Fatalf("a resource without a header shows one:\n%s", res.stdout.String())
	}
}

func TestPublishOriginHeaderRefusedBeforeAnyRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		stdin string
		want  string
	}{
		"local app":                {[]string{"publish", "http://127.0.0.1:3000", "--origin-header", "X-Key"}, originSecret + "\n", msgOriginHeaderLocal},
		"rule and secret on stdin": {[]string{"publish", "https://example.com", "--rule-file", "-", "--origin-header", "X-Key"}, originSecret + "\n", "both read standard input"},
		"empty stdin":              {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, "", msgNoOriginHeaderValue},
		"empty line":               {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, "\n", msgNoOriginHeaderValue},
		"empty name":               {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", ""}, originSecret + "\n", "1 to 256"},
		"reserved name":            {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Forwarded-For"}, originSecret + "\n", "qURL sets that header"},
		"name with a colon":        {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key:"}, originSecret + "\n", "letters, digits"},
		"leading space":            {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, " " + originSecret + "\n", "start or end"},
		"trailing tab":             {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, originSecret + "\t\n", "start or end"},
		"control character":        {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, originSecret + "\x00x\n", "control characters"},
		"value too long":           {[]string{"publish", "https://example.com", "--name", "A", "--origin-header", "X-Key"}, originSecret + strings.Repeat("v", 4096) + "\n", "4096 bytes"},
		"set: empty stdin":         {[]string{"origin-header", "set", "CRID", "X-Key"}, "", msgNoOriginHeaderValue},
		"set: reserved name":       {[]string{"origin-header", "set", "CRID", "cookie"}, originSecret + "\n", "qURL sets that header"},
		"set: bad value":           {[]string{"origin-header", "set", "CRID", "X-Key"}, originSecret + "\x7f\n", "control characters"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, tc.args...), stdin: strings.NewReader(tc.stdin)})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), tc.want) {
				t.Fatalf("exit %d, want %d with %q: %s", res.code, exitcode.Usage, tc.want, res.stderr.String())
			}
			if reqs := srv.Requests(); len(reqs) != 0 {
				t.Fatalf("sent %d requests before refusing", len(reqs))
			}
			assertNoSecret(t, res)
		})
	}
}

// TestPublishOriginHeaderPipedLeavesTheNameUnprompted pins how the two
// inputs share stdin when it is piped: the first line is the secret, and the
// name is never read from it, because the name prompt needs a terminal.
func TestPublishOriginHeaderPipedLeavesTheNameUnprompted(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "publish", "https://example.com/data", "--origin-header", "X-Key"},
		stdin: strings.NewReader(originSecret + "\nTyped name\n"),
	})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if got := sentPublishNames(t, srv); len(got) != 1 || got[0] != "<absent>" {
		t.Fatalf("sent names = %q; a piped publish must not read the name from stdin", got)
	}
	if _, value := srv.OriginHeader(); value != originSecret {
		t.Fatal("the secret is not the first line of stdin")
	}
}

// TestPublishOriginHeaderInATerminalAsksForTheSecretThenTheName pins the
// order in a terminal: the hidden prompt first, then the name prompt reads
// the next line.
func TestPublishOriginHeaderInATerminalAsksForTheSecretThenTheName(t *testing.T) {
	srv := apitest.NewServer(t)
	typed := originSecret
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "publish", "https://example.com/data", "--origin-header", "X-Key"},
		stdin: strings.NewReader("Typed name\n"), inTTY: true, tty: true, hiddenInput: &typed,
	})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	stderr := res.stderr.String()
	secretPrompt := strings.Index(stderr, fmt.Sprintf(msgOriginHeaderPrompt, "X-Key"))
	namePrompt := strings.Index(stderr, msgPublishNamePrompt)
	if secretPrompt < 0 || namePrompt < secretPrompt {
		t.Fatalf("want the hidden prompt, then the name prompt:\n%s", stderr)
	}
	if got := sentPublishNames(t, srv); len(got) != 1 || got[0] != "Typed name" {
		t.Fatalf("sent names = %q", got)
	}
	if _, value := srv.OriginHeader(); value != originSecret {
		t.Fatal("the hidden input is not the value")
	}
	assertNoSecret(t, res)
}

func TestPublishOriginHeaderMismatchSaysHowToChangeIt(t *testing.T) {
	for name, existing := range map[string]string{"another header": "X-Other", "no header": ""} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetPublishFoundExisting(true)
			srv.SetOriginHeader(existing, "old")
			res := runCLI(t, &runOpts{
				args:  originPublishArgs(srv, "--origin-header", "X-Key"),
				stdin: strings.NewReader(originSecret + "\n"),
			})
			if res.code != exitcode.Conflict {
				t.Fatalf("exit %d, want %d: %s", res.code, exitcode.Conflict, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), "qurl origin-header set <CRID> <name>") || res.stdout.Len() != 0 {
				t.Fatalf("stdout %q, stderr %q", res.stdout.String(), res.stderr.String())
			}
			if got, _ := srv.OriginHeader(); got != existing {
				t.Fatalf("the refused publish changed the header to %q", got)
			}
			assertNoSecret(t, res)
		})
	}
}

// TestPublishAgainWithANewSecretIsRefused pins that a publish cannot rotate
// the secret: the same name with another value is a mismatch, and the stored
// value is unchanged.
func TestPublishAgainWithANewSecretIsRefused(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	srv.SetOriginHeader("X-Key", "old-secret")
	res := runCLI(t, &runOpts{
		args:  originPublishArgs(srv, "--origin-header", "x-key"),
		stdin: strings.NewReader(originSecret + "\n"),
	})
	if res.code != exitcode.Conflict || !strings.Contains(res.stderr.String(), "qurl origin-header set") {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if _, value := srv.OriginHeader(); value != "old-secret" {
		t.Fatal("the refused publish changed the stored value")
	}
	assertNoSecret(t, res)
}

func TestPublishAgainWithoutOriginHeaderKeepsIt(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	srv.SetOriginHeader("X-Key", originSecret)
	res := runCLI(t, &runOpts{args: originPublishArgs(srv)})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), "X-Key") {
		t.Fatalf("the kept header is not shown:\n%s", res.stdout.String())
	}
	assertNoSecret(t, res)
}

// TestPublishOriginHeaderMustBeConfirmed holds a create to an answer that
// names the requested header in canonical form, so an older service that
// ignores the member cannot pass as having set it.
func TestPublishOriginHeaderMustBeConfirmed(t *testing.T) {
	for name, answered := range map[string]string{"absent": "", "another name": "X-Other", "not canonical": "x-key"} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "target_url": "https://app.example.vercel.app", "status": "active", "private": true}
				if answered != "" {
					data["origin_header_name"] = answered
				}
				apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": false})
			})
			res := runCLI(t, &runOpts{
				args:  originPublishArgs(srv, "--origin-header", "x-key"),
				stdin: strings.NewReader(originSecret + "\n"),
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), "did not confirm the origin header") {
				t.Fatalf("exit %d: %s", res.code, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), "qurl origin-header set "+srv.Key.CRID) {
				t.Fatalf("the error does not name the next step with the CRID: %s", res.stderr.String())
			}
			assertNoSecret(t, res)
		})
	}
}

func TestOriginHeaderSetSendsThePatchAndShowsOnlyTheName(t *testing.T) {
	for name, tc := range map[string]struct {
		flags []string
		check func(t *testing.T, stdout string)
	}{
		"text": {nil, func(t *testing.T, stdout string) {
			if !strings.Contains(stdout, "Origin header:") || !strings.Contains(stdout, "Authorization") {
				t.Fatalf("stdout:\n%s", stdout)
			}
		}},
		"json": {[]string{"-o", "json"}, func(t *testing.T, stdout string) {
			var doc struct {
				CRID             string `json:"crid"`
				OriginHeaderName string `json:"origin_header_name"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil || doc.OriginHeaderName != "Authorization" || doc.CRID == "" {
				t.Fatalf("document %+v, %v:\n%s", doc, err, stdout)
			}
		}},
		"quiet": {[]string{"-q"}, func(t *testing.T, stdout string) {
			if stdout != "Authorization\n" {
				t.Fatalf("stdout = %q", stdout)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			args := append(append([]string{"--endpoint", srv.URL, "-v"}, tc.flags...), "origin-header", "set", srv.Key.CRID, "authorization")
			res := runCLI(t, &runOpts{args: args, stdin: strings.NewReader("Basic " + originSecret + "\r\n")})
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.stderr.String())
			}
			want := fmt.Sprintf(`{"name":"authorization","value":"Basic %s"}`, originSecret)
			if got := sentOriginHeaders(t, srv); len(got) != 1 || got[0] != want {
				t.Fatalf("sent = %q, want [%s]", got, want)
			}
			tc.check(t, res.stdout.String())
			assertNoSecret(t, res)
		})
	}
}

// TestOriginHeaderSetReadsAHiddenPrompt covers the terminal form of set: the
// prompt names the header, and a carriage return the terminal leaves at the
// end of the line is not part of the value.
func TestOriginHeaderSetReadsAHiddenPrompt(t *testing.T) {
	srv := apitest.NewServer(t)
	typed := originSecret + "\r"
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "origin-header", "set", srv.Key.CRID, "X-Key"},
		inTTY: true, tty: true, hiddenInput: &typed,
	})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), fmt.Sprintf(msgOriginHeaderPrompt, "X-Key")) {
		t.Fatalf("no prompt: %q", res.stderr.String())
	}
	if _, value := srv.OriginHeader(); value != originSecret {
		t.Fatalf("stored value has %d bytes, want %d", len(value), len(originSecret))
	}
	assertNoSecret(t, res)
}

func TestOriginHeaderClearSendsNull(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetOriginHeader("X-Key", originSecret)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "origin-header", "clear", srv.Key.CRID}})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPatch || string(reqs[0].Body) != `{"origin_header":null}` {
		t.Fatalf("requests = %+v", reqs)
	}
	if name, _ := srv.OriginHeader(); name != "" {
		t.Fatalf("header %q still set", name)
	}
	if !strings.Contains(res.stdout.String(), "none") {
		t.Fatalf("stdout:\n%s", res.stdout.String())
	}
	quiet := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "origin-header", "clear", srv.Key.CRID}})
	if quiet.code != 0 || quiet.stdout.Len() != 0 {
		t.Fatalf("-q clear: exit %d, stdout %q", quiet.code, quiet.stdout.String())
	}
}

// TestOriginHeaderChangeMustBeConfirmed holds set and clear to an answer
// that shows the change.
func TestOriginHeaderChangeMustBeConfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		answered string
	}{
		"set, answer names none":    {[]string{"set", "", "X-Key"}, ""},
		"set, answer names another": {[]string{"set", "", "X-Key"}, "X-Other"},
		"clear, answer still names": {[]string{"clear", ""}, "X-Key"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "private": true}
				if tc.answered != "" {
					data["origin_header_name"] = tc.answered
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
			})
			args := append([]string(nil), tc.args...)
			args[1] = srv.Key.CRID
			res := runCLI(t, &runOpts{
				args:  append([]string{"--endpoint", srv.URL, "origin-header"}, args...),
				stdin: strings.NewReader(originSecret + "\n"),
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), "did not confirm the change to the origin header") || res.stdout.Len() != 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
			}
			assertNoSecret(t, res)
		})
	}
}

func TestStatusShowsTheOriginHeaderName(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetOriginHeader("x-vercel-protection-bypass", originSecret)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "status", srv.Key.CRID}})
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stdout.String(), "X-Vercel-Protection-Bypass") {
		t.Fatalf("stdout:\n%s", res.stdout.String())
	}
	assertNoSecret(t, res)
}

// TestOriginHeaderServiceRefusalShowsNoValue checks that a refusal by the
// service, rendered with its problem text and verbose request lines, shows
// no value: the value is in the request body only, which no diagnostic
// prints.
func TestOriginHeaderServiceRefusalShowsNoValue(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_origin_header", "Invalid Origin Header",
			"origin_header.value must not contain control characters other than tab")
	})
	res := runCLI(t, &runOpts{
		args:  []string{"--endpoint", srv.URL, "-v", "origin-header", "set", srv.Key.CRID, "X-Key"},
		stdin: strings.NewReader(originSecret + "\n"),
	})
	if res.code != exitcode.InvalidInput {
		t.Fatalf("exit %d: %s", res.code, res.stderr.String())
	}
	assertNoSecret(t, res)
}
