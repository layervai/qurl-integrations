package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestLoginRejectsImplausibleKey(t *testing.T) {
	res := runCLI(t, &runOpts{
		args:  []string{"login"},
		stdin: strings.NewReader("not-a-key\n"),
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
}

func TestLoginEmptyPipeIsUsageErrorNotAHang(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"login"}, stdin: strings.NewReader("")})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
}

func TestVersionOutputShape(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"version"}})
	if res.code != 0 {
		t.Fatalf("exit = %d", res.code)
	}
	// The Homebrew formula asserts on this line's shape; keep it stable.
	if !strings.HasPrefix(res.stdout.String(), "qurl version test (") {
		t.Errorf("version output = %q, want the qurl version prefix", res.stdout.String())
	}
}

func TestReleaseNativeTrustVerifierIsHiddenAndFailsClosedInDarkBuild(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	help := runCLI(t, &runOpts{args: []string{"version", "--help"}})
	if help.code != 0 || strings.Contains(help.stdout.String(), "verify-release-native-trust") {
		t.Fatalf("version help exposed the release verifier: code=%d stdout=%q stderr=%q", help.code, help.stdout.String(), help.stderr.String())
	}
	result := runCLI(t, &runOpts{args: []string{"version", "--verify-release-native-trust"}})
	if result.code == 0 || result.stdout.Len() != 0 || !strings.Contains(result.stderr.String(), "missing required built-in connection settings") {
		t.Fatalf("dark release verifier = code=%d stdout=%q stderr=%q", result.code, result.stdout.String(), result.stderr.String())
	}
	for _, forbidden := range []string{"Hub", "QURL_CONNECTOR_HUB_", "server public key"} {
		if strings.Contains(result.stderr.String(), forbidden) {
			t.Fatalf("dark release verifier exposed %q: stderr=%q", forbidden, result.stderr.String())
		}
	}
}

// TestHelpLeadsWithTheOneCommandLocalJourney protects the first-run UX. The
// command reference can stay precise without making a new user read Connector
// internals before seeing the ordinary localhost path.
func TestHelpLeadsWithTheOneCommandLocalJourney(t *testing.T) {
	root := runCLI(t, &runOpts{args: []string{"--help"}})
	if root.code != 0 {
		t.Fatalf("root help exit = %d, stderr: %s", root.code, root.stderr.String())
	}
	rootHelp := root.stdout.String()
	local := strings.Index(rootHelp, "qurl publish http://127.0.0.1:3000")
	remote := strings.Index(rootHelp, "qurl publish https://api.example.com/reports")
	if local < 0 || remote < 0 || local >= remote {
		t.Errorf("root help must show local publish before remote publish:\n%s", rootHelp)
	}
	for _, want := range []string{"permanent resource ID", "qurl get"} {
		if !strings.Contains(rootHelp, want) {
			t.Errorf("root help missing %q:\n%s", want, rootHelp)
		}
	}

	publish := runCLI(t, &runOpts{args: []string{"publish", "--help"}})
	if publish.code != 0 {
		t.Fatalf("publish help exit = %d, stderr: %s", publish.code, publish.stderr.String())
	}
	publishHelp := publish.stdout.String()
	local = strings.Index(publishHelp, "qurl publish http://127.0.0.1:3000")
	remote = strings.Index(publishHelp, "qurl publish https://api.example.com/reports")
	if local < 0 || remote < 0 || local >= remote {
		t.Errorf("publish help must explain the local path first:\n%s", publishHelp)
	}
	for _, want := range []string{"On Linux, macOS, and Windows", "background daemon", "--foreground", "prints the CRID, and exits", "qurl get <CRID>"} {
		if !strings.Contains(publishHelp, want) {
			t.Errorf("publish help missing %q:\n%s", want, publishHelp)
		}
	}
	if strings.Contains(publishHelp, "outside macOS and Linux") {
		t.Errorf("publish help still says Windows is unsupported:\n%s", publishHelp)
	}
	for _, jargon := range []string{"FRP", "proxy registration", "one-shot enrollment", "native device identity"} {
		if strings.Contains(publishHelp, jargon) {
			t.Errorf("publish help exposes implementation jargon %q:\n%s", jargon, publishHelp)
		}
	}
}

// TestSharingCopyStatesTheDeviceAccessRule pins the access copy wherever the
// CLI explains sharing.
//
// The rule is about the two commands: share and get use the device's
// identity, they work on the resource owner's devices and, for a private
// resource, on the devices the publisher allowed, and any other device gets
// "not found".
//
// Public and private stay distinct, and the default is stated: a resource is
// private unless published with --public, and a private one is limited to its
// owner and the allowed devices.
//
// Publish help and the README then say what a publisher needs to know about
// a public resource, each exactly once and in the words get's help uses:
// where the deployment offers it, anyone who has the CRID can open the
// resource; and the deployment this release ships does not offer it yet. Both
// also say that privacy is set at creation.
//
// --private was how a private resource was published while public was the
// default. It is still accepted and changes nothing, so no help surface
// teaches it: the flag is hidden, and only the README mentions it, once, for
// a reader who has a command from an earlier release.
//
// The sentence about the shipped deployment is true for as long as
// TestRequestCRIDLinkUnderTheShippedDeploymentIsNotMade passes. A release
// that ships a deployment which offers the request drops it in publish help,
// in get's help (TestGetCopyStatesWhatOpensWithOnlyACRID), in the README and
// in the share not-found hint.
//
// The test also keeps two retired statements off every help surface and out
// of the README: the promise that a public CRID can be shared without an
// account or login, and the limitation that a CLI release opens a public
// CRID only on the owner's devices. The CLI can ask for a link with the CRID
// alone now, so what is missing is a deployment that offers the request.
func TestSharingCopyStatesTheDeviceAccessRule(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	help := func(args ...string) string {
		t.Helper()
		res := runCLI(t, &runOpts{args: append(args, "--help")})
		if res.code != 0 {
			t.Fatalf("qurl %s exit = %d, stderr: %s", strings.Join(args, " "), res.code, res.stderr.String())
		}
		return collapse(res.stdout.String())
	}
	// Markdown code marks are dropped from the README, so one set of
	// sentences is compared with both.
	readme := collapse(strings.ReplaceAll(readCLIREADME(t), "`", ""))
	publishHelp := help("publish")

	const (
		scope = "resource owner's devices and, for a private resource, on the devices"
		rule  = `any other device gets "not found"`
	)
	for _, surface := range []struct{ name, text, identity string }{
		{"qurl --help", help(), "using this device's identity"},
		{"qurl share --help", help("share"), "share uses this device's identity"},
		{"qurl get --help", help("get"), "get uses this device's identity"},
		{"qurl publish --help", publishHelp, "use the identity of the device they run on"},
		{"README", readme, "using this device's identity"},
	} {
		for _, want := range []string{surface.identity, scope, rule} {
			if !strings.Contains(surface.text, want) {
				t.Errorf("%s does not state the access rule: missing %q", surface.name, want)
			}
		}
	}

	const (
		anyone = "Where the deployment offers it, a public resource can also be opened by anyone who has its CRID."
		notYet = "The deployment this release ships does not offer it yet."
	)
	for _, surface := range []struct {
		name, text string
		// setAtCreation is how the surface says that privacy cannot change.
		setAtCreation string
		// notYetCount is how often the surface says that the shipped
		// deployment does not offer the request. The README says it here and
		// in its section about get.
		notYetCount int
	}{
		{"qurl publish --help", publishHelp, "Privacy is set at creation and cannot be changed later.", 1},
		{"README", readme, "Privacy is set at creation; a retry cannot change it.", 2},
	} {
		for _, want := range []string{
			"A resource is private unless you publish it with --public",
			"limited to its owner and the devices",
		} {
			if !strings.Contains(surface.text, want) {
				t.Errorf("%s does not state the default and keep public and private distinct: missing %q", surface.name, want)
			}
		}
		for _, want := range []string{anyone + " " + notYet, surface.setAtCreation} {
			if got := strings.Count(surface.text, want); got != 1 {
				t.Errorf("%s states %q %d times, want exactly once", surface.name, want, got)
			}
		}
		if got := strings.Count(surface.text, notYet); got != surface.notYetCount {
			t.Errorf("%s says %d times that the shipped deployment does not offer the request, want %d", surface.name, got, surface.notYetCount)
		}
	}

	root, _ := newRoot("test", discardStreams())
	checked := map[string]string{"README": readme}
	if got := strings.Count(readme, "--private"); got != 1 || !strings.Contains(readme, "--private was given; that flag is still accepted and now changes nothing") {
		t.Errorf("README mentions --private %d times; want once, saying that the flag is still accepted and changes nothing", got)
	}
	for where, text := range visibleSurfaces(root) {
		if strings.Contains(text, "--private") {
			t.Errorf("%s teaches --private, which no longer changes what a publish does", where)
		}
		checked[where] = collapse(text)
		if where != "qurl publish long" && where != "qurl get long" && strings.Contains(checked[where], notYet) {
			t.Errorf("%s says that the shipped deployment does not offer the request; among the help surfaces only publish and get say it", where)
		}
	}
	if len(checked) < 2 {
		t.Fatal("no help surfaces collected; the retired-statement check would be vacuous")
	}
	for _, where := range []string{"qurl publish long", "qurl get long"} {
		if !strings.Contains(checked[where], notYet) {
			t.Fatalf("%s is not collected under the key the check above exempts", where)
		}
	}
	for where, text := range checked {
		lower := strings.ToLower(text)
		for _, retired := range []string{
			"anyone given a public crid",
			"need no account or login",
			"without an account or login",
			"without a layerv account",
		} {
			if strings.Contains(lower, retired) {
				t.Errorf("%s still promises sharing without the device's identity: found %q", where, retired)
			}
		}
		// Public was the default until private became it.
		for _, retired := range []string{
			"a resource is public unless",
			"publish it with --private",
			"requires --private",
		} {
			if strings.Contains(lower, retired) {
				t.Errorf("%s still describes public as the default: found %q", where, retired)
			}
		}
		for _, retired := range []string{
			"a public crid is meant to be opened by anyone who has it",
			"opens it only on the owner's devices",
			"from other devices is not available yet",
			"only on its owner's devices in this release",
		} {
			if strings.Contains(lower, retired) {
				t.Errorf("%s still says a CLI release cannot open a public CRID on other devices: found %q", where, retired)
			}
		}
	}
}

func TestLegacyConnectorCommandIsRemoved(t *testing.T) {
	root := runCLI(t, &runOpts{args: []string{"--help"}})
	if root.code != 0 {
		t.Fatalf("root help exit = %d, stderr: %s", root.code, root.stderr.String())
	}
	if strings.Contains(root.stdout.String(), "  connector ") {
		t.Fatalf("root help still exposes the removed connector command:\n%s", root.stdout.String())
	}
	legacy := runCLI(t, &runOpts{args: []string{"connector"}})
	if legacy.code != 2 || !strings.Contains(legacy.stderr.String(), `unknown command "connector"`) {
		t.Fatalf("removed connector command = exit %d, stderr %q; want usage error", legacy.code, legacy.stderr.String())
	}
}

// TestREADMECarriesACompleteLocalQuickstart keeps the README's opening path
// runnable: where to get a key, how to sign in, what to run, what success
// looks like, and how to stop or open the result.
func TestREADMECarriesACompleteLocalQuickstart(t *testing.T) {
	readme := readCLIREADME(t)
	const firstJourney = "```bash\nqurl publish http://127.0.0.1:3000\n```"
	if firstFence := strings.Index(readme, "```bash"); firstFence < 0 || firstFence != strings.Index(readme, firstJourney) {
		t.Fatal("CLI README must lead with the one-command localhost journey")
	}
	quickstartAt := strings.Index(readme, "## Publish localhost in 60 seconds")
	if quickstartAt < 0 {
		t.Fatal("CLI README has no 60-second localhost quickstart")
	}
	readme = readme[quickstartAt:]
	wantInOrder := []string{
		"## Publish localhost in 60 seconds",
		"brew install layervai/tap/qurl",
		"qurl version",
		"2.0.0 or newer",
		"No account, API key, or browser sign-in is required.",
		"python3 -m http.server 3000 --bind 127.0.0.1",
		"qurl publish http://127.0.0.1:3000",
		"Status:  serving",
		"qurl stop <CRID>",
		"qurl get <CRID>",
	}
	previous := -1
	for _, want := range wantInOrder {
		at := strings.Index(readme, want)
		if at < 0 {
			t.Fatalf("CLI README quickstart missing %q", want)
		}
		if at <= previous {
			t.Fatalf("CLI README quickstart has %q out of order", want)
		}
		previous = at
	}
	if !strings.Contains(readme, "only HTTPS URLs are allowed") || !strings.Contains(readme, "brew upgrade qurl") {
		t.Fatal("CLI README must explain how a legacy install recovers to the lifecycle release")
	}
}

func TestREADMEAPIKeyFileRecipeMatchesUnixSecurityContract(t *testing.T) {
	readme := readCLIREADME(t)
	for _, want := range []string{
		"have mode `0400` or `0600`",
		"exactly one hard link",
		`(umask 077; printf '%s\n' "$QURL_API_KEY" > "$path")`,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("CLI README API-key file recipe missing %q", want)
		}
	}
}

// TestREADMEQuickstartOutputMatchesPrinter keeps the walkthrough honest: its
// success block is the production plain-text formatter's exact byte stream,
// not a hand-maintained approximation that can drift from the CLI.
func TestREADMEQuickstartOutputMatchesPrinter(t *testing.T) {
	readme := readCLIREADME(t)

	var stdout, stderr bytes.Buffer
	printer := output.New(&output.Streams{
		In:  strings.NewReader(""),
		Out: &stdout,
		Err: &stderr,
	}, output.FormatText, false, false, false, nil)
	// The walkthrough publishes with no flag, so its resource is private.
	private := true
	if err := printer.Publish(&qurlapi.Published{
		Private:   &private,
		CRID:      "<CRID>",
		TargetURL: "http://127.0.0.1:3000",
		Status:    "serving",
	}); err != nil {
		t.Fatalf("render documented publish result: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("documented publish result wrote stderr: %q", stderr.String())
	}
	want := "```text\n" + stdout.String() + "```"
	if !strings.Contains(readme, want) {
		t.Fatalf("CLI README output is not the production formatter's exact output:\n%s", stdout.String())
	}
}

func TestREADMEHeadlessEnrollmentRecoveryContract(t *testing.T) {
	readme := strings.Join(strings.Fields(readCLIREADME(t)), " ")
	for _, want := range []string{
		"If bootstrap stops before registration finishes, retry with the same still-valid one-time credential.",
		"A complete warm start does not read or require that file.",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("CLI README lost interrupted-enrollment guidance %q", want)
		}
	}
}

// readCLIREADME makes repository checkout line endings irrelevant to semantic
// documentation assertions. GitHub's Windows runners can materialize Markdown
// with CRLF even though the formatter intentionally emits LF.
func readCLIREADME(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatalf("read CLI README: %v", err)
	}
	return normalizeDocumentationNewlines(string(raw))
}

func normalizeDocumentationNewlines(document string) string {
	return strings.ReplaceAll(document, "\r\n", "\n")
}

func TestNormalizeDocumentationNewlines(t *testing.T) {
	const windows = "```bash\r\nqurl publish http://127.0.0.1:3000\r\n```\r\n"
	const portable = "```bash\nqurl publish http://127.0.0.1:3000\n```\n"
	if got := normalizeDocumentationNewlines(windows); got != portable {
		t.Fatalf("normalize Windows documentation newlines: got %q, want %q", got, portable)
	}
}

func TestLocalPublishRecoveryIsAutomatic(t *testing.T) {
	publish := runCLI(t, &runOpts{args: []string{"publish", "--help"}})
	if publish.code != 0 {
		t.Fatalf("publish help exit = %d, stderr: %s", publish.code, publish.stderr.String())
	}
	publishHelp := publish.stdout.String()
	for _, forbidden := range []string{"--refresh-mode", "explicit approval", "manual, auto, or disabled"} {
		if strings.Contains(publishHelp, forbidden) {
			t.Fatalf("publish help exposes removed manual recovery %q:\n%s", forbidden, publishHelp)
		}
	}
	for _, want := range []string{"background daemon", "sleep, wake, and network changes"} {
		if !strings.Contains(publishHelp, want) {
			t.Fatalf("publish help missing automatic recovery behavior %q", want)
		}
	}
}

func TestBackgroundSharePlatformContract(t *testing.T) {
	if err := requireBackgroundShareSupport("darwin"); err != nil {
		t.Fatalf("darwin background share support: %v", err)
	}
	if err := requireBackgroundShareSupport("windows"); err != nil {
		t.Fatalf("windows background share support: %v", err)
	}
	if err := requireBackgroundShareSupport("linux"); err != nil {
		t.Fatalf("linux background share support: %v", err)
	}
	err := requireBackgroundShareSupport("plan9")
	if err == nil || !strings.Contains(err.Error(), "local app sharing is supported on macOS, Linux, and Windows only") {
		t.Fatalf("unsupported background share error = %v", err)
	}
}

func TestLocalSharePlatformContractFailsBeforeStateOrNetwork(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		if err := requireLocalShareSupport(goos); err != nil {
			t.Fatalf("%s local share support: %v", goos, err)
		}
	}
	for _, args := range [][]string{
		{"publish", "http://127.0.0.1:3000", "--foreground"},
		{"start", exampleCRID},
		{"restart", exampleCRID},
		{"daemon", "run"},
	} {
		res := runCLI(t, &runOpts{args: args, platformGOOS: "plan9"})
		if res.code != 1 || !strings.Contains(res.stderr.String(), "local app sharing is supported on macOS, Linux, and Windows only") {
			t.Fatalf("unsupported platform %v = exit %d stderr %q", args, res.code, res.stderr.String())
		}
	}
}

func TestWindowsLocalShareCommandsReachRuntimeSeams(t *testing.T) {
	seamErr := errors.New("Windows runtime seam reached")
	for _, args := range [][]string{
		{"publish", "http://127.0.0.1:3000", "--foreground"},
		{"start", exampleCRID},
		{"restart", exampleCRID},
		{"daemon", "run"},
	} {
		res := runCLI(t, &runOpts{
			args: args, platformGOOS: "windows", shareStateDirErr: seamErr,
			preflightTarget: func(context.Context, string, int) error { return nil },
		})
		if res.code != 1 || !strings.Contains(res.stderr.String(), seamErr.Error()) {
			t.Fatalf("Windows %v did not reach the state/runtime seam: exit %d stderr %q", args, res.code, res.stderr.String())
		}
	}
}

func TestDaemonHubOverrideIsExactAndAllOrNone(t *testing.T) {
	const key = "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if got, present, err := exactDaemonHubOverride("", 0, ""); err != nil || present || got != (qurl.HubBootstrap{}) {
		t.Fatalf("empty daemon Hub override = (%#v, %t, %v), want absent", got, present, err)
	}
	for _, test := range []struct {
		host string
		port int
		key  string
	}{
		{host: "hub.sandbox.layerv.xyz"},
		{port: 443},
		{key: key},
		{host: "hub.sandbox.layerv.xyz", port: 443},
	} {
		if _, _, err := exactDaemonHubOverride(test.host, test.port, test.key); err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("partial daemon Hub override %#v error = %v", test, err)
		}
	}
	got, present, err := exactDaemonHubOverride("hub.sandbox.layerv.xyz", 443, key)
	if err != nil || !present || got.Host != "hub.sandbox.layerv.xyz" || got.Port != 443 || got.ServerPublicKeyB64 != key {
		t.Fatalf("exact daemon Hub override = (%#v, %t, %v)", got, present, err)
	}
	if _, _, err := exactDaemonHubOverride("127.0.0.1", 443, key); err == nil {
		t.Fatal("daemon Hub override accepted an untrusted IP endpoint")
	}
}

func TestCompletionGeneratesScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: []string{"completion", shell}})
			if res.code != 0 || res.stdout.Len() == 0 {
				t.Fatalf("completion %s: exit=%d, %d bytes", shell, res.code, res.stdout.Len())
			}
		})
	}
	res := runCLI(t, &runOpts{args: []string{"completion", "tcsh"}})
	if res.code != 2 {
		t.Errorf("unsupported shell should be a usage error, got exit %d", res.code)
	}
}

// TestDocsGeneratesManPagesAndMarkdown pins the release contract:
// .goreleaser.yml runs `qurl docs man -d manpages` and ships the output.
func TestDocsGeneratesManPagesAndMarkdown(t *testing.T) {
	manDir := t.TempDir()
	res := runCLI(t, &runOpts{args: []string{"docs", "man", "-d", manDir}})
	if res.code != 0 {
		t.Fatalf("docs man exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(manDir, "qurl.1")); err != nil {
		t.Errorf("expected qurl.1 man page: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manDir, "qurl-publish.1")); err != nil {
		t.Errorf("expected qurl-publish.1 man page: %v", err)
	}

	mdDir := t.TempDir()
	res = runCLI(t, &runOpts{args: []string{"docs", "markdown", "-d", mdDir}})
	if res.code != 0 {
		t.Fatalf("docs markdown exit = %d", res.code)
	}
	if _, err := os.Stat(filepath.Join(mdDir, "qurl_share.md")); err != nil {
		t.Errorf("expected qurl_share.md: %v", err)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := map[string][]string{
		"unknown command": {"frobnicate"},
		"unknown flag":    {"list", "--no-such-flag"},
		"bad output":      {"-o", "yaml", "list"},
		"bad color":       {"--color", "sometimes", "list"},
		"missing operand": {"share"},
		"extra operand":   {"whoami", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}
}

func TestUnusableOperandExitEight(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"share", "not a CRID at all!"}})
	if res.code != 8 {
		t.Fatalf("exit = %d, want 8; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

// TestResolveIsNoLongerACommand pins the hard cutover from `qurl resolve`
// to `qurl share`: the old name is not an alias, so it takes cobra's
// unknown-command path — usage exit, nothing on stdout, and no request
// ever leaves the process.
func TestResolveIsNoLongerACommand(t *testing.T) {
	srv := apitest.NewServer(t) // never contacted
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "resolve", srv.Key.CRID}})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2 (usage); stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), `unknown command "resolve"`) {
		t.Errorf("stderr = %q, want the unknown-command error", res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "share") {
		t.Errorf("stderr = %q, want the error to name the share command", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Errorf("an unknown command must not reach the service, saw %d request(s)", len(srv.Requests()))
	}
}

// TestEndpointPrecedence pins flag > env > profile for the endpoint setting
// end to end: each level is proven by the request actually landing on the
// mock that level names.
func TestEndpointPrecedence(t *testing.T) {
	srv := apitest.NewServer(t)

	t.Run("env", func(t *testing.T) {
		res := runCLI(t, &runOpts{
			args: []string{"list"},
			env:  map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": srv.URL},
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		res := runCLI(t, &runOpts{
			args: []string{"--endpoint", srv.URL, "list"},
			env:  map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": "https://unreachable.invalid"},
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})

	t.Run("profile", func(t *testing.T) {
		dir := t.TempDir()
		profileDir := filepath.Join(dir, "profiles")
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, "sandbox.yaml"), []byte("endpoint: "+srv.URL+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runCLI(t, &runOpts{
			args:      []string{"--profile", "sandbox", "list"},
			configDir: dir,
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})
}

func TestConfigFileWithSecretRefusedExitThree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("api_key: lv_test_shouldnotbehere123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{args: []string{"list"}, configDir: dir})
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "must not contain an API key") {
		t.Errorf("expected the no-secrets refusal, got %q", res.stderr.String())
	}
}

// The legacy injected account client still requires its explicit credential.
// TestPublishDefaultsToDeviceEnrollment covers the production default.
func TestInjectedAccountClientMissingCredentialExitFour(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"list"}, env: map[string]string{}})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "QURL_API_KEY") {
		t.Errorf("expected the remedy hint, got %q", res.stderr.String())
	}
}

// TestEnumSourceDecidesExitCode pins the round-4 disposition: the same
// invalid enum value is a configuration error (exit 3) when a config FILE
// says it, and a usage error (exit 2) when the flag or environment does —
// the config layer knows the source, the resolution site does not.
func TestEnumSourceDecidesExitCode(t *testing.T) {
	fileCases := map[string]string{
		"output": "output: yaml\n",
		"color":  "color: sometimes\n",
	}
	for name, body := range fileCases {
		t.Run("file "+name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			res := runCLI(t, &runOpts{args: []string{"list"}, configDir: dir})
			if res.code != 3 {
				t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), "config.yaml") {
				t.Errorf("the config-file error must name the file, got %q", res.stderr.String())
			}
		})
	}

	argCases := map[string][]string{
		"flag output": {"-o", "yaml", "list"},
		"flag color":  {"--color", "sometimes", "list"},
	}
	for name, args := range argCases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}

	envCases := map[string]map[string]string{
		"env output": {"QURL_API_KEY": testAPIKey, "QURL_OUTPUT": "yaml"},
		"env color":  {"QURL_API_KEY": testAPIKey, "QURL_COLOR": "sometimes"},
	}
	for name, env := range envCases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: []string{"list"}, env: env})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}
}

// TestWhoamiListedInHelp guards the command roster: every v2 command is
// registered and visible.
func TestWhoamiListedInHelp(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"--help"}})
	if res.code != 0 {
		t.Fatalf("help exit = %d", res.code)
	}
	for _, name := range []string{"publish", "share", "get", "list", "start", "stop", "restart", "status", "inspect", "daemon", "delete", "login", "whoami", "publisher", "version", "completion"} {
		if !strings.Contains(res.stdout.String(), name) {
			t.Errorf("help does not list %q", name)
		}
	}
	daemon := runCLI(t, &runOpts{args: []string{"daemon", "--help"}})
	if daemon.code != 0 || !strings.Contains(daemon.stdout.String(), "run") || !strings.Contains(daemon.stdout.String(), "headless") {
		t.Errorf("daemon help does not expose its run mode:\n%s\n%s", daemon.stdout.String(), daemon.stderr.String())
	}
}

func TestReleaseTrustVerifierRejectsDeploymentOverride(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, filepath.Join(t.TempDir(), "deployment.json"))
	result := runCLI(t, &runOpts{args: []string{"version", "--verify-release-native-trust"}})
	if result.code == 0 || result.stdout.Len() != 0 || !strings.Contains(result.stderr.String(), "requires QURL_DEPLOYMENT to be unset") {
		t.Fatalf("override verification = code %d, stderr %q", result.code, result.stderr.String())
	}
}
