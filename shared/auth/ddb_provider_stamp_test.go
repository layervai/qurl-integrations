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

// wholeRowDeleters are the workspace_state methods that remove the ENTIRE row
// instead of editing attributes, mapped to why they are exempt from the stamp
// invariant. There is no surviving row to stamp, and these are the readers of the
// stamp rather than its writers.
func wholeRowDeleters() map[string]string {
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
			requireStampsUpdatedAtNano(t, w.method, ddb.updateInput, stampedAt)
		})
	}
}

// TestWorkspaceStateMutatorsAreStampCovered is the completeness half: it fails
// when *DDBProvider grows a Set*/Delete* method that neither stampingWriters
// covers nor wholeRowDeleters exempts. Without it, adding a fourth writer would
// reintroduce the exact hole this file exists to close — the new writer would
// simply have no test, the way SetSlackBotToken had none.
func TestWorkspaceStateMutatorsAreStampCovered(t *testing.T) {
	covered := make(map[string]bool)
	for _, w := range stampingWriters() {
		covered[w.method] = true
	}
	exempt := wholeRowDeleters()

	typ := reflect.TypeOf(&DDBProvider{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		// Set*/Delete* is how every mutator on this type is spelled. A future
		// mutator named otherwise (Rotate*, Upsert*) escapes this net, so extend
		// the prefix list alongside it.
		if !strings.HasPrefix(name, "Set") && !strings.HasPrefix(name, "Delete") {
			continue
		}
		if covered[name] || exempt[name] != "" {
			continue
		}
		t.Errorf("DDBProvider.%s mutates workspace_state but is not stamp-covered.\n"+
			"If it edits attributes on a surviving row it MUST set %s from p.nowOrDefault() — "+
			"the lifecycle purge guard's attribute_not_exists arm fails open, so an unstamped write "+
			"lets a delayed uninstall purge delete what it just stored. Add it to stampingWriters().\n"+
			"If it deletes the whole row instead, add it to wholeRowDeleters() with the reason.",
			name, attrUpdatedAtNano)
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

// stampValueRef finds the `:value` placeholder that expr assigns to
// attrUpdatedAtNano, accepting either spelling the writers use: the literal
// attribute name (setAPIKey, SetSlackBotToken) or an ExpressionAttributeNames
// placeholder (DeleteAPIKey's #updated_at_nano).
func stampValueRef(expr string, names map[string]string) (valueRef string, found bool) {
	tokens := []string{attrUpdatedAtNano}
	for placeholder, attr := range names {
		if attr == attrUpdatedAtNano {
			tokens = append(tokens, placeholder)
		}
	}
	for _, token := range tokens {
		// The leading boundary keeps the literal `updated_at_unix_nano` from also
		// matching a longer attribute that merely ends with it.
		re := regexp.MustCompile(`(?:\A|[\s,])` + regexp.QuoteMeta(token) + `\s*=\s*(:[A-Za-z0-9_]+)`)
		if m := re.FindStringSubmatch(expr); m != nil {
			return m[1], true
		}
	}
	return "", false
}
