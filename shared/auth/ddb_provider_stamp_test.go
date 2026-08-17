package auth

import (
	"context"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// This file is the enforcement behind the invariant asserted in prose at
// attrUpdatedAtNano's declaration: "Every workspace_state write must refresh
// attrUpdatedAtNano."
//
// Why it needs enforcing. DeleteWorkspaceStateBeforeWithIdentity guards the
// lifecycle purge with
//
//	attribute_not_exists(#updated_at_nano) OR #updated_at_nano <= :purge_cutoff_nano
//
// whose first arm deliberately fails OPEN so rows written before the stamp
// existed (#820) can still be purged. That fail-open arm is only safe because
// every in-code writer stamps: a post-cutoff write — a reinstall's
// SetSlackBotToken, a fresh setup's SetAPIKeyWithMetadata — installs a stamp
// newer than the cutoff, so the delayed purge hits the second arm and no-ops.
// A writer that skips the stamp silently converts the guard back into an
// unconditional delete for that row, and the failure mode is the worst one
// available: a delayed uninstall purge deletes the credentials a reinstall just
// stored, and nothing errors.
//
// Before this file, that invariant was prose only. Deleting the stamp line from
// either SetAPIKeyWithMetadata or SetSlackBotToken — the two writers a reinstall
// actually runs — left `go test ./shared/auth/... ./apps/slack/...` fully green.
// Only DeleteAPIKey's stamp was pinned, by TestDDBProviderDeleteAPIKey's
// whole-expression equality assertion, and only incidentally.

// stampingWriter is one DDBProvider method that edits attributes on a surviving
// workspace_state row and must therefore refresh attrUpdatedAtNano.
type stampingWriter struct {
	method string
	invoke func(context.Context, *DDBProvider) error
}

// stampingWriters enumerates every attribute-editing workspace_state mutator.
// TestWorkspaceStateMutatorsAreStampCovered reflects over *DDBProvider and fails
// when a mutator is missing from this list, so it cannot silently fall behind the
// implementation. The reverse drift needs no guard: each entry calls its method
// directly, so a rename or removal is a compile error.
//
// Contract: every writer here persists through UpdateItem, which is why the
// assertion reads fakeDDBClient.updateInput. A future writer that legitimately
// upserts the whole row via PutItem would fail here for the wrong reason ("issued
// no UpdateItem") — give requireStampsUpdatedAtNano a second capture path rather
// than assuming the stamp is missing.
func stampingWriters() []stampingWriter {
	return []stampingWriter{
		{
			method: "SetAPIKeyWithMetadata",
			invoke: func(ctx context.Context, p *DDBProvider) error {
				return p.SetAPIKeyWithMetadata(ctx, testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN")
			},
		},
		{
			method: "SetSlackBotToken",
			invoke: func(ctx context.Context, p *DDBProvider) error {
				return p.SetSlackBotToken(ctx, testTeamID, &SlackBotTokenInstall{BotToken: testSlackBotToken})
			},
		},
		{
			method: "DeleteAPIKey",
			invoke: func(ctx context.Context, p *DDBProvider) error {
				return p.DeleteAPIKey(ctx, testTeamID)
			},
		},
	}
}

// exemptMutators maps each Set*/Delete* method that does NOT carry the stamp
// invariant to the reason it is exempt. Everything here today is a whole-row
// deleter — no row survives to carry a stamp, and these are the stamp's readers
// rather than its writers.
//
// It is deliberately one bucket keyed on a free-text reason rather than a
// taxonomy. The Set*/Delete* net in TestWorkspaceStateMutatorsAreStampCovered
// also catches methods that never touch workspace_state at all — a DI/config
// setter like SetClock or SetEncryptor would trip it — and those belong here
// with a reason saying so, not forced into a category that misdescribes them.
func exemptMutators() map[string]string {
	return map[string]string{
		"DeleteWorkspaceState":                   "unconditional whole-row DeleteItem; no row survives to carry a stamp",
		"DeleteWorkspaceStateWithIdentity":       "whole-row DeleteItem with ReturnValues=ALL_OLD; no row survives to carry a stamp",
		"DeleteWorkspaceStateBeforeWithIdentity": "whole-row DeleteItem; this is the guard that READS the stamp",
	}
}

// TestWorkspaceStateWritersStampUpdatedAtNano drives every attribute-editing
// workspace_state mutator over a capturing fake and asserts each one writes
// attrUpdatedAtNano from the provider's injected clock.
func TestWorkspaceStateWritersStampUpdatedAtNano(t *testing.T) {
	// A fixed, non-zero instant. The assertion compares the written stamp against
	// exactly this value, so a writer that reaches for time.Now() instead of the
	// provider clock fails here rather than passing on a near-enough number — the
	// stamp and the purge cutoff must come from one clock to be comparable.
	stampedAt := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	for _, w := range stampingWriters() {
		t.Run(w.method, func(t *testing.T) {
			ddb := &fakeDDBClient{}
			p := &DDBProvider{
				Client:    ddb,
				TableName: "ws",
				Encryptor: &passthroughEncryptor{},
				Now:       func() time.Time { return stampedAt },
			}
			if err := w.invoke(context.Background(), p); err != nil {
				t.Fatalf("%s: %v", w.method, err)
			}
			// Exactly one write, so the last-write-wins updateInput below is
			// unambiguously the write under test. A future read-modify-write writer
			// that stamps on a call other than its last would otherwise slip through.
			if ddb.updateCalls != 1 {
				t.Fatalf("%s issued %d UpdateItem calls, want exactly 1 — "+
					"assert the stamping call explicitly if this writer legitimately writes more than once",
					w.method, ddb.updateCalls)
			}
			requireStampsUpdatedAtNano(t, w.method, ddb.updateInput, stampedAt)
		})
	}
}

// TestWorkspaceStateMutatorsAreStampCovered is the completeness half: it fails
// when *DDBProvider grows a Set*/Delete* method that neither stampingWriters
// covers nor exemptMutators excuses. Without it, adding a fourth writer would
// reintroduce the exact hole this file exists to close — the new writer would
// simply have no test, the way SetSlackBotToken had none.
func TestWorkspaceStateMutatorsAreStampCovered(t *testing.T) {
	covered := make(map[string]bool)
	for _, w := range stampingWriters() {
		covered[w.method] = true
	}
	exempt := exemptMutators()
	// Membership is what exempts, so an entry can never be silently voided by an
	// empty reason. Require the reason separately, and say so plainly.
	for name, reason := range exempt {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemptMutators()[%q] has no reason — record why it is exempt from the %s invariant", name, attrUpdatedAtNano)
		}
	}

	typ := reflect.TypeOf(&DDBProvider{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		// Set*/Delete* is how every mutator on this type is spelled today. That
		// naming convention is not itself enforced, so this prefix filter is not
		// the last line of defense: TestDDBProviderExportedMethodsAreClassified
		// requires EVERY exported method to be classified, which is what actually
		// catches a future writer named Rotate*, Upsert*, Put* or Store*.
		if !strings.HasPrefix(name, "Set") && !strings.HasPrefix(name, "Delete") {
			continue
		}
		if _, ok := exempt[name]; ok || covered[name] {
			continue
		}
		t.Errorf("DDBProvider.%s mutates workspace_state but is not stamp-covered.\n"+
			"If it edits attributes on a surviving row it MUST set %s from p.nowOrDefault() — "+
			"the lifecycle purge guard's attribute_not_exists arm fails open, so an unstamped write "+
			"lets a delayed uninstall purge delete what it just stored. Add it to stampingWriters().\n"+
			"If it deletes the whole row, or does not touch workspace_state at all, add it to "+
			"exemptMutators() with the reason.",
			name, attrUpdatedAtNano)
	}
}

// nonMutatingMethods are the exported *DDBProvider methods that read, or report
// a capability, and never write the table — mapped to what they do.
func nonMutatingMethods() map[string]string {
	return map[string]string{
		"APIKey":               "read: decrypts and returns the workspace qURL API key",
		"APIKeyID":             "read: returns the stored qURL key id",
		"APIKeyIdentity":       "read: returns the key id plus qURL account provenance",
		"SlackBotToken":        "read: decrypts and returns the workspace Slack bot token",
		"SupportsDeleteAPIKey": "capability predicate; issues no DynamoDB call",
	}
}

// TestDDBProviderExportedMethodsAreClassified closes the residual hole in the
// Set*/Delete* naming net. TestWorkspaceStateMutatorsAreStampCovered can only
// see methods spelled that way, so a future attribute-editing writer named
// Rotate*, Upsert*, Put* or Store* would escape it silently and land untested —
// precisely the failure this file exists to prevent, just renamed.
//
// Requiring every exported method to appear in exactly one of the three
// classifications makes that impossible: any new method at all fails this test
// until someone decides which it is. The cost is a one-line edit when an
// unrelated reader is added, which is the intended forcing function, not a
// side effect.
func TestDDBProviderExportedMethodsAreClassified(t *testing.T) {
	classified := make(map[string]string)
	for _, w := range stampingWriters() {
		classified[w.method] = "stampingWriters()"
	}
	for name := range exemptMutators() {
		classified[name] = "exemptMutators()"
	}
	for name := range nonMutatingMethods() {
		classified[name] = "nonMutatingMethods()"
	}

	actual := make(map[string]bool)
	typ := reflect.TypeOf(&DDBProvider{})
	for i := range typ.NumMethod() {
		actual[typ.Method(i).Name] = true
	}

	for name := range actual {
		if _, ok := classified[name]; !ok {
			t.Errorf("DDBProvider.%s is exported but unclassified.\n"+
				"If it edits attributes on a surviving workspace_state row it MUST stamp %s — "+
				"add it to stampingWriters(). If it deletes the whole row, add it to exemptMutators(). "+
				"If it only reads or reports a capability, add it to nonMutatingMethods().",
				name, attrUpdatedAtNano)
		}
	}
	// The reverse direction matters for the two string-keyed maps: a renamed or
	// removed method leaves a stale entry that would silently excuse nothing.
	// stampingWriters() needs no such check — it calls each method directly, so a
	// rename there is a compile error.
	for name, bucket := range classified {
		if !actual[name] {
			t.Errorf("%s lists DDBProvider.%s, which no longer exists — drop the stale entry", bucket, name)
		}
	}
}

// requireStampsUpdatedAtNano asserts that in sets attrUpdatedAtNano to want's
// Unix-nanosecond value.
func requireStampsUpdatedAtNano(t *testing.T, method string, in *dynamodb.UpdateItemInput, want time.Time) {
	t.Helper()
	if in == nil {
		t.Fatalf("%s issued no UpdateItem on workspace_state", method)
	}
	expr := aws.ToString(in.UpdateExpression)
	valueRef, ok := stampValueRef(expr, in.ExpressionAttributeNames)
	if !ok {
		t.Fatalf("%s does not set %s: %q\n"+
			"The lifecycle purge compares this stamp against the teardown cutoff. Without it the row "+
			"matches the guard's attribute_not_exists arm, so a delayed uninstall purge deletes the "+
			"credentials this write just stored.", method, attrUpdatedAtNano, expr)
	}
	v, isNumber := in.ExpressionAttributeValues[valueRef].(*ddbtypes.AttributeValueMemberN)
	if !isNumber {
		t.Fatalf("%s binds %s to %s, which is not a DynamoDB number (%v) — the guard compares it numerically",
			method, attrUpdatedAtNano, valueRef, in.ExpressionAttributeValues[valueRef])
	}
	if wantNano := strconv.FormatInt(want.UnixNano(), 10); v.Value != wantNano {
		t.Fatalf("%s stamped %s = %s, want %s from the provider clock",
			method, attrUpdatedAtNano, v.Value, wantNano)
	}
}

// assignmentPattern captures every `<name> = :<value>` assignment in a DynamoDB
// update expression, where <name> is either a literal attribute or a `#`
// placeholder. The leading boundary and the required `=` together pin the WHOLE
// token, so neither a longer attribute ending in the one we want
// (`x_updated_at_unix_nano`) nor one starting with it (`updated_at_unix_nano_v2`)
// can masquerade as a match. Assignments to a function rather than a value
// placeholder — `configured_at = if_not_exists(configured_at, :now)` — simply do
// not match, which is correct: only the stamp's binding is of interest here.
var assignmentPattern = regexp.MustCompile(`(?:\A|[\s,])(#?[A-Za-z0-9_]+)\s*=\s*(:[A-Za-z0-9_]+)`)

// stampValueRef finds the `:value` placeholder that expr assigns to
// attrUpdatedAtNano, accepting either spelling the writers use: the literal
// attribute name (setAPIKey, SetSlackBotToken) or an ExpressionAttributeNames
// placeholder (DeleteAPIKey's #updated_at_nano).
func stampValueRef(expr string, names map[string]string) (valueRef string, found bool) {
	for _, m := range assignmentPattern.FindAllStringSubmatch(expr, -1) {
		token, value := m[1], m[2]
		attr := token
		if resolved, ok := names[token]; ok {
			attr = resolved
		}
		if attr == attrUpdatedAtNano {
			return value, true
		}
	}
	return "", false
}
