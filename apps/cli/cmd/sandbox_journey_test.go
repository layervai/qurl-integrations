//go:build clisandbox

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/crid"

	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// T6 live sandbox CRID journey: the design doc's §28.1/§28.3 manual
// checklist, automated. This file carries the clisandbox build tag, so the
// cli / sandbox e2e job runs it on every same-repo PR — serialized against
// the nightly extended pass through the workflow-level cli-sandbox-e2e
// concurrency group, because there is exactly one sandbox tenancy.
//
// Credential contract (both required before this suite runs anything):
//
//	QURL_API_KEY  — a sandbox API key holding the qurl:read, qurl:write, and
//	    qurl:resolve scopes. Read through the CLI's hermetic mode: with the
//	    variable set, the credential store is bypassed entirely and nothing
//	    on disk is read or written.
//	QURL_ENDPOINT — the sandbox qURL API base URL (a repository secret:
//	the sandbox hostname is deliberately not public).
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
// environment and skips loudly — naming both variables — when it is not
// fully provisioned. The returned map is the ONLY environment the CLI
// invocations see, which is what keeps hermetic mode airtight.
func sandboxJourneyEnv(t *testing.T) map[string]string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("QURL_API_KEY"))
	endpoint := strings.TrimSpace(os.Getenv("QURL_ENDPOINT"))
	if key == "" || endpoint == "" {
		missing := []string{}
		if key == "" {
			missing = append(missing, "QURL_API_KEY")
		}
		if endpoint == "" {
			missing = append(missing, "QURL_ENDPOINT")
		}
		t.Skipf("SKIPPED LOUDLY: live sandbox CRID journey is disarmed — missing %v. "+
			"Sandbox credentials are not provisioned yet; arm this by setting QURL_API_KEY "+
			"(a sandbox key with the qurl:read, qurl:write, and qurl:resolve scopes) and "+
			"QURL_ENDPOINT (the sandbox qURL API base URL — a repository secret).", missing)
	}
	return map[string]string{"QURL_API_KEY": key, "QURL_ENDPOINT": endpoint}
}

// runSandboxCLI invokes the real command tree against the live sandbox:
// injected environment only (hermetic credential mode), non-TTY streams (the
// piped contracts are what scripts see), and the production sleep path so
// the transport's bounded 429 retries wait like they would in the field.
func runSandboxCLI(ctx context.Context, t *testing.T, cliEnv map[string]string, args ...string) *runResult {
	t.Helper()
	return runCLI(t, &runOpts{args: args, env: cliEnv, ctx: ctx, realSleep: true})
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
	// dedupes an already-published target.
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
// next_cursor — and requires the published CRID to appear exactly once
// across the whole listing (a page overlap or a dropped row is a pagination
// bug even when the row is eventually found).
func assertListFindsCRID(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	seen := 0
	cursor := ""
	for page := 1; ; page++ {
		if page > listMaxPages {
			t.Fatalf("list pagination did not terminate within %d pages of %d; refusing to walk further", listMaxPages, listPageLimit)
		}
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
		if !doc.HasMore {
			break
		}
		if doc.NextCursor == "" {
			t.Fatalf("list page %d reports has_more with no next_cursor; pagination cannot continue", page)
		}
		cursor = doc.NextCursor
	}
	if seen != 1 {
		t.Fatalf("published CRID appeared %d times across the paginated listing, want exactly once", seen)
	}
}

// assertResolveJourney resolves the CRID and holds the piped contract: with
// stdout not a terminal the command emits the bare link and nothing else, so
// `curl "$(qurl resolve <CRID>)"` composes. Exit 0 here IS the verification
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

// assertGetDownloadsBytes pulls real bytes through a freshly minted link
// with `get --file` and requires the atomic-download contract: payload in
// the destination file, nothing on stdout, no .part left behind.
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
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("get --file wrote zero bytes; the minted link served no payload")
	}
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("atomic download left %s.part behind (stat err: %v)", dest, err)
	}
	t.Logf("downloaded %d bytes through the minted link", info.Size())
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
	res := runCLI(t, &runOpts{args: []string{"delete", id, "--yes"}, env: cliEnv, ctx: ctx, realSleep: true})
	if res.code != 0 {
		t.Errorf("cleanup: delete %s exit = %d — the throwaway resource may be leaked on the sandbox tenancy\nstderr: %s",
			id, res.code, res.stderr.String())
	}
}
