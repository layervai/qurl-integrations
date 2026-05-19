package internal

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHandleCreate_AliasForm_HappyPath fences the `$alias` create
// branch: target prefixed with `$` is resolved via the channel's
// alias_bindings map (same source as /qurl get) and the mint carries
// `resource_id`, not `target_url`. The reply surfaces the qURL link
// and labels the source as the alias.
func TestHandleCreate_AliasForm_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})

	var mintHits atomic.Int32
	var sawTargetURL bool
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
		mintHits.Add(1)
		// Body parse is incidental — what matters is `target_url`
		// MUST NOT be present (mutually-exclusive with resource_id on
		// the server). The presence-only check tolerates field-order
		// drift in the encoded JSON. ReadAll (not a single Body.Read
		// of len ContentLength) so a chunked or partial-read transport
		// can't split the body and silently pass the absence check.
		buf, _ := io.ReadAll(r.Body)
		if strings.Contains(string(buf), `"target_url"`) {
			sawTargetURL = true
		}
		writeCreateFixture(t, w, "https://qurl.link/from-alias", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("create $prod-db", testAdminTeamID, testAdminUserID)
	if mintHits.Load() != 1 {
		t.Fatalf("mint hits = %d, want 1", mintHits.Load())
	}
	if sawTargetURL {
		t.Error("mint body carried target_url on alias-form create — must be resource_id only")
	}
	if !strings.Contains(async, "qURL created!") {
		t.Errorf("async reply missing success header: %q", async)
	}
	if !strings.Contains(async, "https://qurl.link/from-alias") {
		t.Errorf("async reply missing qURL link: %q", async)
	}
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing alias label: %q", async)
	}
}

// TestHandleCreate_AliasForm_NotFound fences the no-binding path:
// `create $unknown` looks up the channel's alias_bindings, finds
// nothing, surfaces "No resource has alias `$X`" and never reaches
// the mint. Mirrors the get-side message so users see the same copy
// regardless of which verb they typed.
func TestHandleCreate_AliasForm_NotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("create $missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No resource has alias `$missing`") {
		t.Errorf("async reply missing not-found message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite missing alias (hits = %d)", mintHits.Load())
	}
}

// TestHandleCreate_AliasForm_EmptyAlias fences the bare-`$` path:
// `create $` (no alias name after the sigil) surfaces a usage hint
// without reaching DDB or the mint. Without this guard, the empty
// alias would land at `LookupChannelAlias` and surface as the
// generic "Could not reach qURL" copy — wrong disposition for
// a fix-your-input case.
func TestHandleCreate_AliasForm_EmptyAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("create $", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "missing alias name after `$`") {
		t.Errorf("async reply missing empty-alias usage hint: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite empty alias (hits = %d)", mintHits.Load())
	}
}

// TestHandleCreate_URLForm_Unchanged fences the URL-form path: the
// existing `create <url>` behavior is preserved — body carries
// target_url, no alias lookup happens. Regression fence so the
// `$alias` branch doesn't accidentally swallow raw URL input.
func TestHandleCreate_URLForm_Unchanged(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var sawTargetURL bool
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		if strings.Contains(string(buf), `"target_url":"https://example.com"`) {
			sawTargetURL = true
		}
		writeCreateFixture(t, w, "https://qurl.link/from-url", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("create https://example.com", testAdminTeamID, testAdminUserID)
	if !sawTargetURL {
		t.Error("mint body missing target_url on URL-form create")
	}
	if !strings.Contains(async, "*Target:* https://example.com") {
		t.Errorf("async reply missing target label: %q", async)
	}
}
