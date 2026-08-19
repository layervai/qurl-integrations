//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/crid"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// T6 live sandbox CRID journey: the design doc's §28.1/§28.3 manual
// checklist, automated. This file carries the clisandbox build tag, so the
// cli / sandbox e2e job runs it on every same-repo PR — serialized against
// the nightly extended pass through the workflow-level cli-sandbox-e2e
// concurrency group, because there is exactly one sandbox tenancy.
//
// Credential contract (all four required before this suite runs anything):
//
//	QURL_API_KEY  — a sandbox API key holding the qurl:read, qurl:write, and
//	    qurl:resolve scopes. Read through the CLI's hermetic mode: with the
//	    variable set, the credential store is bypassed entirely and nothing
//	    on disk is read or written.
//	QURL_ENDPOINT — the sandbox qURL API base URL (a repository secret:
//	    the sandbox hostname is deliberately not public).
//	QURL_SANDBOX_QV2_ISSUER_KEY — the sandbox's link-signing identity as
//	    "<kid>=<standard-base64 P-256 SPKI DER>" (a repository variable,
//	    mirrored from the same-named qurl-connector variable).
//	QURL_SANDBOX_QV2_RELAY_URL — the sandbox's platform access URL (a
//	    repository variable, same provenance).
//
// The last two become a QURL_DEPLOYMENT settings file for the download
// step: `get --file` opens fragment-credential links through the platform
// access flow, which needs the deployment's trust settings. Their values —
// like every minted link — never reach the log: repository variables are
// not masked, and CI logs are public.
//
// Quota safety: each run publishes exactly ONE throwaway resource and always
// reclaims it. The happy path ends in delete + idempotent re-delete, and a
// t.Cleanup registered the moment the CRID exists deletes by CRID even on
// mid-test failure, tolerating already-gone. Retries are the API transport's
// own bounded 429 handling (Retry-After honored via realSleep) — the suite
// adds no retry loops of its own.
//
// The commands run through runCLI with the PRODUCTION wiring — no injected
// seams — exactly like the sandbox Connector smoke in
// sandbox_connector_test.go, whose env-gate pattern this file follows.

// journeyTimeout bounds the whole journey. Every API call also carries the
// transport's own 30-second HTTP timeout; this outer bound exists so a
// pathological hang (a download that never ends, a stuck retry wait) fails
// the test instead of eating the job's timeout budget.
const journeyTimeout = 4 * time.Minute

// journeyDescription labels the throwaway resource so a human auditing the
// sandbox tenancy can tell a leaked fixture from a real one.
const journeyDescription = "qurl-integrations cli sandbox e2e journey (self-cleaning; safe to delete)"

// sandboxJourneyEnv reads the suite's env contract from the real process
// environment and skips loudly — naming every missing variable — when it
// is not fully provisioned. The returned map is the ONLY environment the
// CLI invocations see, which is what keeps hermetic mode airtight: the
// deployment settings the download step needs enter it as QURL_DEPLOYMENT,
// built by journeyDeploymentFile below, never read from the process.
func sandboxJourneyEnv(t *testing.T) map[string]string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("QURL_API_KEY"))
	endpoint := strings.TrimSpace(os.Getenv("QURL_ENDPOINT"))
	issuerKey := strings.TrimSpace(os.Getenv("QURL_SANDBOX_QV2_ISSUER_KEY"))
	relayURL := strings.TrimSpace(os.Getenv("QURL_SANDBOX_QV2_RELAY_URL"))
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_API_KEY":                key,
		"QURL_ENDPOINT":               endpoint,
		"QURL_SANDBOX_QV2_ISSUER_KEY": issuerKey,
		"QURL_SANDBOX_QV2_RELAY_URL":  relayURL,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Skipf("SKIPPED LOUDLY: live sandbox CRID journey is disarmed — missing %v. "+
			"Arm this by setting QURL_API_KEY (a sandbox key with the qurl:read, qurl:write, "+
			"and qurl:resolve scopes), QURL_ENDPOINT (the sandbox qURL API base URL — a "+
			"repository secret), and the QURL_SANDBOX_QV2_ISSUER_KEY / "+
			"QURL_SANDBOX_QV2_RELAY_URL repository variables the download step's deployment "+
			"settings are built from.", missing)
	}
	return map[string]string{
		"QURL_API_KEY":    key,
		"QURL_ENDPOINT":   endpoint,
		"QURL_DEPLOYMENT": journeyDeploymentFile(t, issuerKey, relayURL),
	}
}

// journeyDeploymentFile converts the two sandbox repository variables into
// the SDK's deployment settings file and returns its path. Failures name
// the offending variable but NEVER its value: CI logs are public, and the
// values identify the sandbox.
func journeyDeploymentFile(t *testing.T, issuerKey, relayURL string) string {
	t.Helper()
	kid, keyStd, ok := strings.Cut(issuerKey, "=")
	if !ok || strings.TrimSpace(kid) == "" || strings.TrimSpace(keyStd) == "" {
		t.Fatal("QURL_SANDBOX_QV2_ISSUER_KEY must be of the form <kid>=<standard-base64 key>; refusing to print the malformed value (CI logs are public)")
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyStd))
	if err != nil {
		t.Fatal("QURL_SANDBOX_QV2_ISSUER_KEY's key part is not valid standard base64; refusing to print the malformed value (CI logs are public)")
	}
	relay, err := url.Parse(relayURL)
	if err != nil || relay.Scheme != "https" || relay.Host == "" {
		t.Fatal("QURL_SANDBOX_QV2_RELAY_URL must be an https URL; refusing to print the malformed value (CI logs are public)")
	}
	doc := map[string]any{
		"issuers": []map[string]string{{
			"kid": strings.TrimSpace(kid),
			// The SDK's settings files carry keys base64url-encoded.
			"spki_der_b64": base64.RawURLEncoding.EncodeToString(der),
		}},
		"cells":           []any{},
		"relay_allowlist": []string{relay.Host},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal deployment settings: %v", err)
	}
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write deployment settings: %v", err)
	}
	return path
}

// runSandboxCLI invokes the real command tree against the live sandbox:
// injected environment only (hermetic credential mode), non-TTY streams (the
// piped contracts are what scripts see), the production sleep path so the
// transport's bounded 429 retries wait like they would in the field, and the
// production access opener so `get --file` exercises the real platform
// access flow the injected QURL_DEPLOYMENT settings configure.
func runSandboxCLI(ctx context.Context, t *testing.T, cliEnv map[string]string, args ...string) *runResult {
	t.Helper()
	return runCLI(t, &runOpts{args: args, env: cliEnv, ctx: ctx, realSleep: true, realOpener: true})
}

// journeyPublishDoc mirrors the publish `-o json` document (output/shapes.go).
type journeyPublishDoc struct {
	CRID          string `json:"crid"`
	ResourceID    string `json:"resource_id"`
	TargetURL     string `json:"target_url"`
	FoundExisting bool   `json:"found_existing"`
}

// journeyListDoc mirrors the list `-o json` document. HasMore — not cursor
// presence — is the continuation signal, per the ResourcePage contract.
type journeyListDoc struct {
	Resources []struct {
		CRID string `json:"crid"`
	} `json:"resources"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// TestSandboxCRIDJourney walks the whole customer journey against the real
// sandbox: publish → list (paginated) → resolve (verified, piped bare-URL) →
// get --file (real bytes through the minted link) → delete --yes →
// idempotent re-delete → resolve-after-delete (owner-truthful revoked exit).
func TestSandboxCRIDJourney(t *testing.T) {
	cliEnv := sandboxJourneyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), journeyTimeout)
	defer cancel()

	// Publish one throwaway resource. The target is unique per run (the
	// query string keeps example.com serving its stable 200 page) so
	// serialized runs never adopt each other's resources even if the service
	// dedupes an already-published target. The download step's content
	// assertion leans on the same stability: this page's body is known to
	// carry journeyTargetMarker.
	target := "https://example.com/?qurl-cli-sandbox-e2e=" + strconv.FormatInt(time.Now().UnixNano(), 10)
	res := runSandboxCLI(ctx, t, cliEnv, "-o", "json", "publish", target, "--description", journeyDescription)
	if res.code != 0 {
		t.Fatalf("publish exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	var pub journeyPublishDoc
	if err := json.Unmarshal(res.stdout.Bytes(), &pub); err != nil {
		t.Fatalf("publish -o json emitted %q: %v", res.stdout.String(), err)
	}
	if pub.CRID == "" {
		t.Fatalf("publish minted no CRID (resource %s); this endpoint cannot carry the CRID journey", pub.ResourceID)
	}
	// Reclaim the one resource no matter where the journey stops below. The
	// happy path deletes it first, so this is normally the idempotent no-op.
	t.Cleanup(func() { reclaimSandboxResource(t, cliEnv, pub.CRID) })
	if pub.FoundExisting {
		t.Logf("note: the service reports the target was already published (found_existing); adopting and reclaiming it")
	}
	t.Logf("published %s -> %s", pub.CRID, pub.TargetURL)

	// The minted identifier is a full-form test-environment CRID: 60
	// characters, and the leading 'q' that marks the test version bytes —
	// the premise of the environment-guard assertion at the resolve step.
	if len(pub.CRID) != 60 || !strings.HasPrefix(pub.CRID, "q") {
		t.Fatalf("CRID = %q (len %d), want the 60-character 'q'-prefixed test-environment form", pub.CRID, len(pub.CRID))
	}
	assessment, err := cridux.Assess(pub.CRID)
	if err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("published CRID %q does not pass the CLI's own local gate (kind %d): %v", pub.CRID, assessment.Kind, err)
	}
	if environment := assessment.CRID.Environment(); environment != crid.EnvironmentTest {
		t.Fatalf("published CRID environment = %v, want the test environment on the sandbox", environment)
	}

	assertListFindsCRID(ctx, t, cliEnv, pub.CRID)
	link := assertResolveJourney(ctx, t, cliEnv, pub.CRID)
	// The link value never reaches the log: CI logs are public, and a
	// minted link carries the sandbox hostname and a live qv2 credential.
	t.Logf("resolved %s -> a verified %d-byte https link", pub.CRID, len(link))
	assertGetDownloadsBytes(ctx, t, cliEnv, pub.CRID)
	assertDeleteJourney(ctx, t, cliEnv, pub.CRID)
}

// listPageLimit keeps pages small enough that pagination is real without
// hammering the tenancy; listMaxPages bounds the walk so a broken cursor can
// never spin the suite.
const (
	listPageLimit = 10
	listMaxPages  = 25
)

// assertListFindsCRID walks `qurl list -o json` page by page under the
// documented pagination contract — continue exactly while has_more, via
// next_cursor — and requires the published CRID to appear exactly once in
// what it walks (a page overlap or a dropped row is a pagination bug even
// when the row is eventually found).
//
// The shared tenancy accumulates rows from every sandbox surface (bots,
// suites), so the whole listing can legitimately outgrow any fixed page
// budget — it first crossed listMaxPages*listPageLimit rows on 2026-08-19,
// turning the old walk-everything Fatal into a permanent red. The walk
// therefore stops at the budget and asserts over the window it scanned.
// That window is sufficient because the row under test was published
// moments ago and the platform lists newest first.
//
// TODO(upstream-contract): created_at descending is qurl-service's pinned
// default sort for the resource listing (handlers/server.go, "default:
// created_at:desc"). If that default ever changes, this window argument
// breaks loudly — seen stays 0 — and the walk needs an explicit sort or a
// different presence strategy.
func assertListFindsCRID(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	seen := 0
	cursor := ""
	pages := 0
	for page := 1; ; page++ {
		args := []string{"-o", "json", "list", "--limit", strconv.Itoa(listPageLimit)}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		res := runSandboxCLI(ctx, t, cliEnv, args...)
		if res.code != 0 {
			t.Fatalf("list page %d exit = %d, want 0\nstderr: %s", page, res.code, res.stderr.String())
		}
		var doc journeyListDoc
		if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
			t.Fatalf("list page %d -o json emitted %q: %v", page, res.stdout.String(), err)
		}
		for _, row := range doc.Resources {
			if row.CRID == id {
				seen++
			}
		}
		pages = page
		if !doc.HasMore {
			break
		}
		if doc.NextCursor == "" {
			t.Fatalf("list page %d reports has_more with no next_cursor; pagination cannot continue", page)
		}
		if page == listMaxPages {
			t.Logf("list still reports more rows after %d pages of %d; asserting over the newest-first window walked so far", listMaxPages, listPageLimit)
			break
		}
		cursor = doc.NextCursor
	}
	if seen != 1 {
		t.Fatalf("published CRID appeared %d times across %d newest-first list pages, want exactly once", seen, pages)
	}
}

// assertResolveJourney resolves the CRID and holds the piped contract: with
// stdout not a terminal the command emits the bare link and nothing else, so
// `link="$(qurl resolve <CRID>)"` captures it cleanly (the link opens in a
// browser — downloading is get's job). Exit 0 here IS the verification
// evidence — the CLI discards any answer that fails CRID verification before
// printing (exit 12), so a printed link is a verified link. It also holds
// the environment-guard case: the sandbox is the test environment, so its
// 'q'-prefixed CRID at this non-production endpoint must produce no warning
// at all.
func assertResolveJourney(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) string {
	t.Helper()
	res := runSandboxCLI(ctx, t, cliEnv, "resolve", id)
	if res.code != 0 {
		t.Fatalf("resolve exit = %d, want 0 (a non-zero here means the sandbox refused or the answer failed verification)\nstderr: %s", res.code, res.stderr.String())
	}
	out := res.stdout.String()
	link := strings.TrimSuffix(out, "\n")
	if link == "" || link+"\n" != out || strings.ContainsAny(link, " \n\t") {
		t.Fatalf("piped resolve stdout = %q, want exactly one bare link and a newline", out)
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		t.Fatalf("resolved link %q is not a live https URL: %v", link, err)
	}
	// The environment guard must stay quiet: a test CRID against the
	// configured (non-production) sandbox endpoint is the matched case, so
	// ANY stderr here is a spurious warning.
	if res.stderr.Len() != 0 {
		t.Fatalf("resolve wrote %q to stderr; a test CRID at the sandbox endpoint must resolve without warnings", res.stderr.String())
	}
	return link
}

// journeyTargetMarker is the distinctive body text of the published target
// (example.com's stable page). The download assertion requires it, so the
// test can only pass when the actual target content was fetched — never
// when some other document (the in-browser verification page above all)
// was saved in its place.
const journeyTargetMarker = "Example Domain"

// assertGetDownloadsBytes pulls the published target's content through
// `get --file` and requires it to BE that content: the bytes must carry
// the target page's known marker and must not be the platform's in-browser
// verification page. It also holds the atomic-download contract: payload
// in the destination file, nothing on stdout, no .part left behind.
//
// The two content assertions are the regression guard for the defect where
// `get --file` saved the in-browser page (which a plain GET of a
// fragment-credential link always yields) and reported success. A size
// check alone passed on that page; content identity cannot. On failure,
// nothing about the payload is logged beyond sizes and booleans — the
// in-browser page embeds deployment hostnames, and CI logs are public.
func assertGetDownloadsBytes(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "journey-payload")
	res := runSandboxCLI(ctx, t, cliEnv, "get", id, "--file", dest)
	if res.code != 0 {
		t.Fatalf("get --file exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "Saved to") {
		t.Errorf("get --file stderr = %q, want the saved-to confirmation", res.stderr.String())
	}
	// #nosec G304 -- dest is this test's own t.TempDir path.
	payload, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	if !bytes.Contains(payload, []byte(journeyTargetMarker)) {
		t.Errorf("downloaded %d bytes do not contain the published target's %q marker; "+
			"get saved something other than the target content (payload deliberately not logged)",
			len(payload), journeyTargetMarker)
	}
	if bytes.Contains(payload, []byte(apitest.InterstitialTitle)) {
		t.Errorf("downloaded %d bytes carry the in-browser verification page's title; "+
			"get saved the page a browser needs instead of the target content", len(payload))
	}
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("atomic download left %s.part behind (stat err: %v)", dest, err)
	}
	t.Logf("downloaded %d bytes of verified target content", len(payload))
}

// assertDeleteJourney deletes the resource, proves the re-delete is the
// idempotent success the platform promises, and then holds the
// owner-truthful revoked path: resolving a deleted resource with the
// owner's own key exits with the not-found code and an honest "deleted"
// story on stderr — never a link, never a silent success.
func assertDeleteJourney(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	res := runSandboxCLI(ctx, t, cliEnv, "delete", id, "--yes")
	if res.code != 0 {
		t.Fatalf("delete exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "Deleted "+id) {
		t.Errorf("delete stderr = %q, want the deletion confirmation", res.stderr.String())
	}

	res = runSandboxCLI(ctx, t, cliEnv, "delete", id, "--yes")
	if res.code != 0 {
		t.Fatalf("re-delete exit = %d, want the idempotent 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	// Both shapes are platform-legal per the verified delete contract: a
	// soft-revoked row answers the second DELETE with 204 (the CLI truthfully
	// prints the deletion confirmation again — proven by this suite's first
	// live run), while a hard-reaped row answers 404 (the CLI's already-gone
	// note). Either way the exit is the idempotent 0 asserted above.
	stderrText := res.stderr.String()
	if !strings.Contains(stderrText, msgAlreadyGone) && !strings.Contains(stderrText, "Deleted "+id) {
		t.Errorf("re-delete stderr = %q, want the already-gone note or the repeated deletion confirmation", stderrText)
	}

	res = runSandboxCLI(ctx, t, cliEnv, "resolve", id)
	if res.code != exitcode.NotFound {
		t.Fatalf("resolve after delete exit = %d, want %d (the platform's gone family)\nstdout: %q\nstderr: %s",
			res.code, exitcode.NotFound, res.stdout.String(), res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(strings.ToLower(res.stderr.String()), "deleted") {
		t.Errorf("owner-truthful revoked path: stderr = %q never says the resource was deleted", res.stderr.String())
	}
}

// reclaimSandboxResource is the quota-safety net: delete the journey's one
// resource by CRID on its own fresh context (the journey's context may
// already be dead when a failure lands here). Already-gone is the expected
// idempotent success; any other outcome is a leaked fixture on the shared
// tenancy and fails the run loudly.
func reclaimSandboxResource(t *testing.T, cliEnv map[string]string, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res := runCLI(t, &runOpts{args: []string{"delete", id, "--yes"}, env: cliEnv, ctx: ctx, realSleep: true, realOpener: true})
	if res.code != 0 {
		t.Errorf("cleanup: delete %s exit = %d — the throwaway resource may be leaked on the sandbox tenancy\nstderr: %s",
			id, res.code, res.stderr.String())
	}
}
