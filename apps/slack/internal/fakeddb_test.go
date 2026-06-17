package internal

// fakeDDB is the in-memory DynamoDB stub the post-pivot admin/policy
// tests run against. It satisfies [slackdata.DynamoDBClient] with
// enough behavior to exercise the handler-side contract surfaces:
// PK/SK composite keys, conditional UpdateItem/PutItem with the
// specific expression shapes the production code uses, SET-typed
// attributes for the channel_policies allowed_resource_ids,
// Query-with-Limit + ExclusiveStartKey/LastEvaluatedKey paging on
// (slack_team_id = :tid), and Select=COUNT for the workspace-status
// policy count.
//
// Scope is deliberately narrow — we only implement the expression
// fragments [apps/slack/internal/slackdata] actually emits today.
// A surface broader than the production callers would invite
// drift between fake and SDK behavior on unused paths.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// fakeDDB is an in-memory store. Each table is a map keyed by the
// composite of PK+SK string values; values are full item maps.
// Concurrency-safe so tests that run in t.Parallel can share a
// store if they choose to (none currently do, but the mutex keeps
// future-Kevin from re-debugging a data race in a t.Parallel suite).
type fakeDDB struct {
	mu sync.Mutex
	// tables maps tableName → composite-key string → item.
	tables map[string]map[string]map[string]ddbtypes.AttributeValue
	// keySchemas maps tableName → ordered PK/SK attr names, used for
	// composite-key construction. workspace_mappings is PK-only;
	// channel_policies is PK+SK.
	keySchemas map[string][]string

	// updateHook is invoked on every UpdateItem if non-nil. Tests use
	// it for "must NOT be called" assertions (e.g., the AddAdmin /
	// RemoveAdmin admin-gate that prevents non-admins from reaching
	// the mutation — see failOnAdminMutation).
	updateHook func(in *dynamodb.UpdateItemInput)
	// putHook mirrors updateHook for PutItem (BindWorkspace).
	putHook func(in *dynamodb.PutItemInput)
	// getHook mirrors updateHook for GetItem read assertions.
	getHook func(table string, key map[string]string)
	// getItemErrs maps tableName → injected GetItem error.
	getItemErrs map[string]error
	// updateItemErrs maps tableName → injected UpdateItem error.
	updateItemErrs map[string]error
	// putItemErrs maps tableName → injected PutItem error.
	putItemErrs map[string]error
	// queryErrs maps tableName → injected Query error. Used to drive the
	// best-effort degradation path in ChannelsForResource (e.g. a missing
	// dynamodb:Query grant surfacing as AccessDenied).
	queryErrs map[string]error
	// getItemCounts tracks call counts per table for the
	// SetGetItemErrAfter mechanism.
	getItemCounts map[string]int
	// getItemErrAfters maps tableName → (n, err): the (n+1)-th
	// GetItem against this table returns err.
	getItemErrAfters map[string]struct {
		n   int
		err error
	}
}

// SetGetItemErr injects an error returned on every GetItem against
// `table`. Used to simulate transport failures from the admin gate
// (workspace_mappings) or the channel-policy reads (channel_policies).
func (f *fakeDDB) SetGetItemErr(table string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getItemErrs == nil {
		f.getItemErrs = map[string]error{}
	}
	f.getItemErrs[table] = err
}

// SetGetItemErrAfter injects an error returned starting on the
// (n+1)-th GetItem against `table` — the first n calls succeed, the
// rest fail. Used by status tests that need the admin gate's
// GetItem to succeed but the subsequent status-config GetItem to
// fail (both calls hit the same workspace_mappings table).
func (f *fakeDDB) SetGetItemErrAfter(table string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getItemCounts == nil {
		f.getItemCounts = map[string]int{}
	}
	if f.getItemErrAfters == nil {
		f.getItemErrAfters = map[string]struct {
			n   int
			err error
		}{}
	}
	f.getItemErrAfters[table] = struct {
		n   int
		err error
	}{n: n, err: err}
}

// SetUpdateItemErr injects an error returned on every UpdateItem
// against `table`.
func (f *fakeDDB) SetUpdateItemErr(table string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateItemErrs == nil {
		f.updateItemErrs = map[string]error{}
	}
	f.updateItemErrs[table] = err
}

// SetPutItemErr injects an error returned on every PutItem against
// `table`. Used to simulate the BindWorkspace non-409 path (a
// transport / throttling failure that isn't ConditionalCheckFailed),
// which surfaces the "code redeemed but binding failed — contact
// support" copy.
func (f *fakeDDB) SetPutItemErr(table string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putItemErrs == nil {
		f.putItemErrs = map[string]error{}
	}
	f.putItemErrs[table] = err
}

// SetQueryErr injects an error returned on every Query against `table`.
// Used to simulate a missing dynamodb:Query grant (AccessDenied) so tests
// can assert ChannelsForResource degrades without losing data.
func (f *fakeDDB) SetQueryErr(table string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queryErrs == nil {
		f.queryErrs = map[string]error{}
	}
	f.queryErrs[table] = err
}

// SetUpdateItemHook installs a callback invoked on every UpdateItem.
// Used by tests that need to fail-on-call assertions.
func (f *fakeDDB) SetUpdateItemHook(hook func(in interface{})) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if hook == nil {
		f.updateHook = nil
		return
	}
	f.updateHook = func(in *dynamodb.UpdateItemInput) { hook(in) }
}

// SetPutItemHook mirrors SetUpdateItemHook for PutItem.
func (f *fakeDDB) SetPutItemHook(hook func(in interface{})) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if hook == nil {
		f.putHook = nil
		return
	}
	f.putHook = func(in *dynamodb.PutItemInput) { hook(in) }
}

// SetGetItemHook installs a callback invoked on every GetItem.
func (f *fakeDDB) SetGetItemHook(hook func(table string, key map[string]string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getHook = hook
}

// tableNames groups the table names used across the post-pivot
// production code. Tests construct one of these per fakeDDB so the
// fake and the Store agree on which table maps to which schema.
type tableNames struct {
	workspace     string
	channelPolicy string
}

// defaultTestTableNames returns the canonical table-name set tests
// use. Lifted as a helper because every newAdminTestHandler call site
// needs the same pair.
func defaultTestTableNames() tableNames {
	return tableNames{
		workspace:     "test-workspace-mappings",
		channelPolicy: "test-channel-policies",
	}
}

// newFakeDDB builds an empty in-memory store with the post-pivot
// tables registered. `seed` may pre-populate items by table name; if
// nil the store starts empty. The t parameter is kept for t.Helper /
// future test-only Fatal calls; today it's referenced only to satisfy
// the signature.
func newFakeDDB(t *testing.T, names tableNames, seed map[string][]map[string]ddbtypes.AttributeValue) *fakeDDB {
	t.Helper()
	f := &fakeDDB{
		tables: map[string]map[string]map[string]ddbtypes.AttributeValue{
			names.workspace:     {},
			names.channelPolicy: {},
		},
		keySchemas: map[string][]string{
			names.workspace:     {fAttrSlackTeamID},
			names.channelPolicy: {fAttrSlackTeamID, fAttrSlackChannelID},
		},
	}
	for tbl, items := range seed {
		for _, it := range items {
			f.seedItem(t, tbl, it)
		}
	}
	return f
}

// seedItem inserts an item into the named table at its composite-
// key location. Fatal on unknown table or missing PK/SK attribute.
func (f *fakeDDB) seedItem(t *testing.T, table string, item map[string]ddbtypes.AttributeValue) {
	t.Helper()
	schema, ok := f.keySchemas[table]
	if !ok {
		t.Fatalf("fakeDDB.seedItem: unknown table %q", table)
	}
	key, err := compositeKey(schema, item)
	if err != nil {
		t.Fatalf("fakeDDB.seedItem(%s): %v", table, err)
	}
	f.tables[table][key] = item
}

// newStoreFromFake wires a [*slackdata.Store] against a fakeDDB.
// Mirrors what production cmd/main.go does, minus AWS-config load.
// Centralized here so every test site uses the same wiring shape.
func newStoreFromFake(t *testing.T, f *fakeDDB, names tableNames, now func() string) *slackdata.Store {
	t.Helper()
	s, err := slackdata.NewStore(context.Background(),
		slackdata.WithDynamoDBClient(f),
		slackdata.WithTableNames(names.workspace, names.channelPolicy),
	)
	if err != nil {
		t.Fatalf("newStoreFromFake: %v", err)
	}
	if now != nil {
		// Pin the clock for created_at/updated_at/expires_at-vs-now
		// assertions. now is a string-formatted-time helper so callers
		// can pin RFC3339 directly; we wrap it back into a time.Time
		// here via the same parser slackdata reads.
		_ = now // currently unused — callers seed time fields directly
	}
	return s
}

// compositeKey renders an item's PK[+SK] attributes as a stable
// string for the in-memory map. Only string attributes are allowed
// on key columns (matches the production schema in
// modules/qurl-slack-ddb/main.tf).
func compositeKey(schema []string, item map[string]ddbtypes.AttributeValue) (string, error) {
	parts := make([]string, 0, len(schema))
	for _, attr := range schema {
		v, ok := item[attr]
		if !ok {
			return "", fmt.Errorf("missing key attribute %q", attr)
		}
		s, ok := v.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("key attribute %q is not a string", attr)
		}
		parts = append(parts, s.Value)
	}
	return strings.Join(parts, "\x1f"), nil
}

// GetItem implements [slackdata.DynamoDBClient].
func (f *fakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.TableName)
	if f.getHook != nil {
		f.getHook(name, stringKey(in.Key))
	}
	if err, ok := f.getItemErrs[name]; ok {
		return nil, err
	}
	// SetGetItemErrAfter: succeed for n calls, then return the
	// injected error for every subsequent call.
	if cfg, ok := f.getItemErrAfters[name]; ok {
		if f.getItemCounts == nil {
			f.getItemCounts = map[string]int{}
		}
		count := f.getItemCounts[name]
		f.getItemCounts[name] = count + 1
		if count >= cfg.n {
			return nil, cfg.err
		}
	}
	table, schema, err := f.tableAndSchema(aws.ToString(in.TableName))
	if err != nil {
		return nil, err
	}
	key, err := compositeKey(schema, in.Key)
	if err != nil {
		return nil, err
	}
	item, ok := table[key]
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	return &dynamodb.GetItemOutput{Item: cloneItem(item)}, nil
}

func stringKey(in map[string]ddbtypes.AttributeValue) map[string]string {
	out := make(map[string]string, len(in))
	for name, value := range in {
		if s, ok := value.(*ddbtypes.AttributeValueMemberS); ok {
			out[name] = s.Value
		}
	}
	return out
}

// PutItem implements [slackdata.DynamoDBClient]. Honors the
// `attribute_not_exists(<pk>) OR <attr> = :val` shape used by
// [slackdata.Store.BindWorkspace].
func (f *fakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putHook != nil {
		f.putHook(in)
	}
	if injected, ok := f.putItemErrs[aws.ToString(in.TableName)]; ok {
		return nil, injected
	}
	table, schema, err := f.tableAndSchema(aws.ToString(in.TableName))
	if err != nil {
		return nil, err
	}
	key, err := compositeKey(schema, in.Item)
	if err != nil {
		return nil, err
	}
	if cond := aws.ToString(in.ConditionExpression); cond != "" {
		existing, present := table[key]
		ok, evalErr := evalCondition(cond, existing, present, in.ExpressionAttributeValues, in.ExpressionAttributeNames)
		if evalErr != nil {
			return nil, evalErr
		}
		if !ok {
			return nil, &ddbtypes.ConditionalCheckFailedException{
				Message: aws.String("ConditionalCheckFailedException"),
			}
		}
	}
	table[key] = cloneItem(in.Item)
	return &dynamodb.PutItemOutput{}, nil
}

// UpdateItem implements [slackdata.DynamoDBClient]. Supports the
// SET/ADD/DELETE forms the production code emits and the
// conditional check shapes documented in the package preamble.
func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateHook != nil {
		f.updateHook(in)
	}
	if err, ok := f.updateItemErrs[aws.ToString(in.TableName)]; ok {
		return nil, err
	}
	table, schema, err := f.tableAndSchema(aws.ToString(in.TableName))
	if err != nil {
		return nil, err
	}
	key, err := compositeKey(schema, in.Key)
	if err != nil {
		return nil, err
	}
	existing, present := table[key]
	if cond := aws.ToString(in.ConditionExpression); cond != "" {
		ok, evalErr := evalCondition(cond, existing, present, in.ExpressionAttributeValues, in.ExpressionAttributeNames)
		if evalErr != nil {
			return nil, evalErr
		}
		if !ok {
			return nil, &ddbtypes.ConditionalCheckFailedException{
				Message: aws.String("ConditionalCheckFailedException"),
			}
		}
	}
	// UpdateItem on a missing row materializes the row from the key
	// (matches DDB behavior).
	if !present {
		existing = make(map[string]ddbtypes.AttributeValue, len(in.Key))
		for k, v := range in.Key {
			existing[k] = v
		}
	} else {
		existing = cloneItem(existing)
	}
	if err := applyUpdateExpression(aws.ToString(in.UpdateExpression), existing, in.ExpressionAttributeValues, in.ExpressionAttributeNames); err != nil {
		return nil, err
	}
	table[key] = existing
	out := &dynamodb.UpdateItemOutput{}
	if in.ReturnValues == ddbtypes.ReturnValueAllNew || in.ReturnValues == ddbtypes.ReturnValueUpdatedNew {
		out.Attributes = cloneItem(existing)
	}
	return out, nil
}

// DeleteItem implements [slackdata.DynamoDBClient]. Production
// code doesn't use it yet (DELETE on set-member goes through
// UpdateItem DELETE), but the interface requires it.
func (f *fakeDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	table, schema, err := f.tableAndSchema(aws.ToString(in.TableName))
	if err != nil {
		return nil, err
	}
	key, err := compositeKey(schema, in.Key)
	if err != nil {
		return nil, err
	}
	delete(table, key)
	return &dynamodb.DeleteItemOutput{}, nil
}

// Query implements [slackdata.DynamoDBClient]. Supports the single shape
// [slackdata.Store.ChannelsForResource] emits — a partition-key match
// `slack_team_id = :tid` over the channel_policies table — returning every
// item whose PK equals :tid. Pagination (Limit / ExclusiveStartKey /
// LastEvaluatedKey) is intentionally NOT modeled: the production caller sets
// no Limit and the in-memory tables are tiny, so one page returns everything
// and LastEvaluatedKey stays empty (terminating the caller's paging loop).
// Honors an injected query error so the best-effort degradation path is
// testable.
func (f *fakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.TableName)
	if err, ok := f.queryErrs[name]; ok {
		return nil, err
	}
	table, _, err := f.tableAndSchema(name)
	if err != nil {
		return nil, err
	}
	want, ok := in.ExpressionAttributeValues[":tid"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("fakeDDB.Query: expected a :tid string value (KeyConditionExpression %q)", aws.ToString(in.KeyConditionExpression))
	}
	var items []map[string]ddbtypes.AttributeValue
	for _, item := range table {
		if pk, ok := item[fAttrSlackTeamID].(*ddbtypes.AttributeValueMemberS); ok && pk.Value == want.Value {
			items = append(items, cloneItem(item))
		}
	}
	return &dynamodb.QueryOutput{Items: items}, nil
}

// tableAndSchema looks up the table map and key schema, returning a
// clear error on an unknown table name (catches typos in test setup).
func (f *fakeDDB) tableAndSchema(name string) (table map[string]map[string]ddbtypes.AttributeValue, schema []string, err error) {
	t, ok := f.tables[name]
	if !ok {
		return nil, nil, fmt.Errorf("fakeDDB: unknown table %q (did you wire it via newFakeDDB?)", name)
	}
	return t, f.keySchemas[name], nil
}

// cloneItem returns a shallow copy of the item map. AttributeValue
// pointers are shared — that's fine because the production code
// never mutates them after construction.
func cloneItem(item map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	out := make(map[string]ddbtypes.AttributeValue, len(item))
	for k, v := range item {
		out[k] = v
	}
	return out
}

// applyUpdateExpression walks the SET/ADD/DELETE clauses the
// production code emits. Supports the exact shapes in use:
//
//	SET redeemed = :true, redeemed_by = :user, redeemed_at = :now_iso
//	ADD allowed_resource_ids :rids SET updated_at = :now
//	DELETE allowed_resource_ids :rids SET updated_at = :now
//
// A general DDB expression parser is much larger than this; we'd
// rather grow this with each new production callsite than ship a
// half-finished parser.
func applyUpdateExpression(expr string, item, vals map[string]ddbtypes.AttributeValue, names map[string]string) error {
	if expr == "" {
		return nil
	}
	clauses := splitUpdateClauses(expr)
	for _, c := range clauses {
		switch c.verb {
		case "SET":
			if err := applySetClause(c.body, item, vals, names); err != nil {
				return err
			}
		case "ADD":
			if err := applyAddClause(c.body, item, vals, names); err != nil {
				return err
			}
		case "DELETE":
			if err := applyDeleteClause(c.body, item, vals); err != nil {
				return err
			}
		case "REMOVE":
			if err := applyRemoveClause(c.body, item, names); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fakeDDB: unsupported update verb %q", c.verb)
		}
	}
	return nil
}

type updateClause struct {
	verb string
	body string
}

// splitUpdateClauses walks the expression and returns one clause per
// SET/ADD/DELETE prefix, preserving order. The DDB grammar puts the
// verb keyword as the first whitespace-separated token of each
// clause; clauses run until the next verb keyword.
func splitUpdateClauses(expr string) []updateClause {
	verbs := []string{"SET", "ADD", "DELETE", "REMOVE"}
	tokens := strings.Fields(expr)
	clauses := []updateClause{}
	var cur *updateClause
	for _, tok := range tokens {
		isVerb := false
		for _, v := range verbs {
			if tok == v {
				isVerb = true
				if cur != nil {
					clauses = append(clauses, *cur)
				}
				cur = &updateClause{verb: v}
				break
			}
		}
		if isVerb {
			continue
		}
		if cur == nil {
			continue
		}
		if cur.body != "" {
			cur.body += " "
		}
		cur.body += tok
	}
	if cur != nil {
		clauses = append(clauses, *cur)
	}
	return clauses
}

// applySetClause handles `<attr> = <value>[, <attr> = <value>]*`.
// Values are `:vN` tokens (literal substitutions from
// ExpressionAttributeValues).
func applySetClause(body string, item, vals map[string]ddbtypes.AttributeValue, names map[string]string) error {
	pairs := splitTopLevelCommas(body)
	for _, p := range pairs {
		eq := strings.Index(p, "=")
		if eq < 0 {
			return fmt.Errorf("fakeDDB SET: expected '=' in %q", p)
		}
		attr := strings.TrimSpace(p[:eq])
		valTok := strings.TrimSpace(p[eq+1:])
		v, ok := vals[valTok]
		if !ok {
			return fmt.Errorf("fakeDDB SET: unknown value %q", valTok)
		}
		if err := setAttrPath(item, attr, names, v); err != nil {
			return err
		}
	}
	return nil
}

func applyRemoveClause(body string, item map[string]ddbtypes.AttributeValue, names map[string]string) error {
	paths := strings.Fields(body)
	for _, path := range paths {
		removeAttrPath(item, strings.TrimSuffix(path, ","), names)
	}
	return nil
}

// applyAddClause handles `<attr> :v`. Supports the SS merge form used by
// policy/admin mutations and the numeric increment form used by counters.
func applyAddClause(body string, item, vals map[string]ddbtypes.AttributeValue, names map[string]string) error {
	body = strings.TrimSpace(body)
	parts := strings.Fields(body)
	if len(parts) != 2 {
		return fmt.Errorf("fakeDDB ADD: expected `<attr> :value`, got %q", body)
	}
	attrParts := resolvePath(parts[0], names)
	if len(attrParts) != 1 {
		return fmt.Errorf("fakeDDB ADD: unsupported attr path %q", parts[0])
	}
	attr := attrParts[0]
	v, ok := vals[parts[1]]
	if !ok {
		return fmt.Errorf("fakeDDB ADD: unknown value %q", parts[1])
	}
	switch incoming := v.(type) {
	case *ddbtypes.AttributeValueMemberSS:
		existing, _ := item[attr].(*ddbtypes.AttributeValueMemberSS)
		merged := mergeStringSet(existing, incoming.Value)
		item[attr] = &ddbtypes.AttributeValueMemberSS{Value: merged}
	case *ddbtypes.AttributeValueMemberN:
		add, err := strconv.ParseInt(incoming.Value, 10, 64)
		if err != nil {
			return err
		}
		var cur int64
		if existing, ok := item[attr].(*ddbtypes.AttributeValueMemberN); ok {
			cur, err = strconv.ParseInt(existing.Value, 10, 64)
			if err != nil {
				return err
			}
		}
		item[attr] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(cur+add, 10)}
	default:
		return fmt.Errorf("fakeDDB ADD: only string-set or number ADD is supported, got %T", v)
	}
	return nil
}

// applyDeleteClause handles `<attr> :v`. Currently only supports the
// SS (string-set) remove form used by RemoveAdmin. Removing the
// last element of the set drops the attribute, matching DDB's
// "empty set is not allowed" rule.
func applyDeleteClause(body string, item, vals map[string]ddbtypes.AttributeValue) error {
	body = strings.TrimSpace(body)
	parts := strings.Fields(body)
	if len(parts) != 2 {
		return fmt.Errorf("fakeDDB DELETE: expected `<attr> :value`, got %q", body)
	}
	attr := parts[0]
	v, ok := vals[parts[1]]
	if !ok {
		return fmt.Errorf("fakeDDB DELETE: unknown value %q", parts[1])
	}
	incoming, ok := v.(*ddbtypes.AttributeValueMemberSS)
	if !ok {
		return fmt.Errorf("fakeDDB DELETE: only string-set DELETE is supported, got %T", v)
	}
	existing, ok := item[attr].(*ddbtypes.AttributeValueMemberSS)
	if !ok {
		return nil
	}
	remaining := removeStringSet(existing.Value, incoming.Value)
	if len(remaining) == 0 {
		delete(item, attr)
		return nil
	}
	item[attr] = &ddbtypes.AttributeValueMemberSS{Value: remaining}
	return nil
}

func mergeStringSet(existing *ddbtypes.AttributeValueMemberSS, incoming []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	if existing != nil {
		for _, v := range existing.Value {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range incoming {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func removeStringSet(existing, remove []string) []string {
	drop := map[string]struct{}{}
	for _, v := range remove {
		drop[v] = struct{}{}
	}
	out := make([]string, 0, len(existing))
	for _, v := range existing {
		if _, ok := drop[v]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// splitTopLevelCommas splits on commas that are not inside parens.
// Used by SET's `<attr>=<val>, <attr>=<val>` form. Our production
// expressions don't nest parens, but defensive trims keep the impl
// future-proof.
func splitTopLevelCommas(s string) []string {
	depth := 0
	out := []string{}
	last := 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	tail := strings.TrimSpace(s[last:])
	if tail != "" {
		out = append(out, tail)
	}
	return out
}

func splitTopLevelKeyword(s, keyword string) []string {
	delimiter := " " + keyword + " "
	depth := 0
	out := []string{}
	last := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '(':
			depth++
			i++
		case ')':
			if depth > 0 {
				depth--
			}
			i++
		default:
			if depth == 0 && strings.HasPrefix(s[i:], delimiter) {
				out = append(out, strings.TrimSpace(s[last:i]))
				i += len(delimiter)
				last = i
				continue
			}
			i++
		}
	}
	tail := strings.TrimSpace(s[last:])
	if tail != "" {
		out = append(out, tail)
	}
	return out
}

// evalCondition supports the exact ConditionExpression shapes the
// production code emits: subexpressions joined by top-level " AND " plus the
// reset path's parenthesized top-level " OR ". Mixed or nested boolean trees
// beyond that are intentionally out of scope for this test fake. Each
// subexpression is one of:
//
//	attribute_exists(<attr>)
//	attribute_not_exists(<attr>)
//	contains(<attr>, :val)
//	NOT contains(<attr>, :val)
//	<attr> = :val
//	<attr> > :val
//
// Returns (true, nil) when every subexpression is satisfied.
func evalCondition(expr string, item map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue, names map[string]string) (bool, error) {
	expr = strings.TrimSpace(expr)
	parts := splitTopLevelKeyword(expr, "AND")
	for _, p := range parts {
		ok, err := evalConditionTerm(strings.TrimSpace(p), item, present, vals, names)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalConditionTerm(term string, item map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue, names map[string]string) (bool, error) {
	inner := term
	if strings.HasPrefix(inner, "(") && strings.HasSuffix(inner, ")") {
		inner = strings.TrimSuffix(strings.TrimPrefix(inner, "("), ")")
	}
	if parts := splitTopLevelKeyword(inner, "OR"); len(parts) > 1 {
		for _, part := range parts {
			ok, err := evalConditionTerm(strings.TrimSpace(part), item, present, vals, names)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	switch {
	case strings.HasPrefix(term, "attribute_exists("):
		attr := strings.TrimSuffix(strings.TrimPrefix(term, "attribute_exists("), ")")
		if !present {
			return false, nil
		}
		_, ok := getAttrPath(item, attr, names)
		return ok, nil
	case strings.HasPrefix(term, "attribute_not_exists("):
		attr := strings.TrimSuffix(strings.TrimPrefix(term, "attribute_not_exists("), ")")
		if !present {
			return true, nil
		}
		_, ok := getAttrPath(item, attr, names)
		return !ok, nil
	case strings.HasPrefix(term, "NOT contains("):
		// `NOT contains(<attr>, :val)` — true iff the SS attribute
		// at <attr> does NOT contain :val. Used by AddAdmin's
		// "is :uid already on the admin set?" guard to fold the
		// membership check into the conditional UpdateItem
		// (alongside `attribute_exists(slack_team_id)`).
		inner := strings.TrimSuffix(strings.TrimPrefix(term, "NOT contains("), ")")
		comma := strings.Index(inner, ",")
		if comma < 0 {
			return false, fmt.Errorf("fakeDDB condition: malformed NOT contains term %q", term)
		}
		attr := strings.TrimSpace(inner[:comma])
		valTok := strings.TrimSpace(inner[comma+1:])
		if !present {
			return true, nil
		}
		raw, ok := getAttrPath(item, attr, names)
		if !ok {
			return true, nil
		}
		ss, ok := raw.(*ddbtypes.AttributeValueMemberSS)
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: NOT contains target %q is not SS", attr)
		}
		rhs, ok := vals[valTok]
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: unknown value %q", valTok)
		}
		want, ok := rhs.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: NOT contains :val %q is not S", valTok)
		}
		for _, m := range ss.Value {
			if m == want.Value {
				return false, nil
			}
		}
		return true, nil
	case strings.HasPrefix(term, "contains("):
		// `contains(<attr>, :val)` — true iff the SS attribute at
		// <attr> contains :val. Used by RemoveAdmin's guard
		// (membership-required-for-removal) combined with
		// `attribute_exists(slack_team_id)` via the AND-splitter
		// above.
		inner := strings.TrimSuffix(strings.TrimPrefix(term, "contains("), ")")
		comma := strings.Index(inner, ",")
		if comma < 0 {
			return false, fmt.Errorf("fakeDDB condition: malformed contains term %q", term)
		}
		attr := strings.TrimSpace(inner[:comma])
		valTok := strings.TrimSpace(inner[comma+1:])
		if !present {
			return false, nil
		}
		raw, ok := getAttrPath(item, attr, names)
		if !ok {
			return false, nil
		}
		ss, ok := raw.(*ddbtypes.AttributeValueMemberSS)
		if !ok {
			return false, nil
		}
		rhs, ok := vals[valTok]
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: unknown value %q", valTok)
		}
		want, ok := rhs.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: contains :val %q is not S", valTok)
		}
		for _, m := range ss.Value {
			if m == want.Value {
				return true, nil
			}
		}
		return false, nil
	}
	// Binary comparison: `<attr> <op> :val`. The only operators in
	// production use are `=` and `>`.
	for _, op := range []string{">=", "<=", "<>", "=", ">", "<"} {
		idx := strings.Index(term, " "+op+" ")
		if idx < 0 {
			continue
		}
		attr := strings.TrimSpace(term[:idx])
		valTok := strings.TrimSpace(term[idx+len(op)+2:])
		lhs, ok := getAttrPath(item, attr, names)
		if !ok {
			return false, nil
		}
		rhs, ok := vals[valTok]
		if !ok {
			return false, fmt.Errorf("fakeDDB condition: unknown value %q", valTok)
		}
		return compareAttr(lhs, op, rhs)
	}
	return false, fmt.Errorf("fakeDDB condition: unsupported term %q", term)
}

func setAttrPath(item map[string]ddbtypes.AttributeValue, path string, names map[string]string, v ddbtypes.AttributeValue) error {
	parts := resolvePath(path, names)
	if len(parts) == 0 {
		return fmt.Errorf("fakeDDB SET: empty attr path %q", path)
	}
	if len(parts) == 1 {
		item[parts[0]] = v
		return nil
	}
	if len(parts) != 2 {
		return fmt.Errorf("fakeDDB SET: unsupported nested attr path %q", path)
	}
	m, ok := item[parts[0]].(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return fmt.Errorf("fakeDDB SET: %q is not a map", parts[0])
	}
	m.Value[parts[1]] = v
	return nil
}

func removeAttrPath(item map[string]ddbtypes.AttributeValue, path string, names map[string]string) {
	parts := resolvePath(path, names)
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		delete(item, parts[0])
		return
	}
	if len(parts) != 2 {
		return
	}
	m, ok := item[parts[0]].(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return
	}
	delete(m.Value, parts[1])
}

func getAttrPath(item map[string]ddbtypes.AttributeValue, path string, names map[string]string) (ddbtypes.AttributeValue, bool) {
	parts := resolvePath(path, names)
	if len(parts) == 0 {
		return nil, false
	}
	if len(parts) == 1 {
		v, ok := item[parts[0]]
		return v, ok
	}
	if len(parts) != 2 {
		return nil, false
	}
	m, ok := item[parts[0]].(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil, false
	}
	v, ok := m.Value[parts[1]]
	return v, ok
}

func resolvePath(path string, names map[string]string) []string {
	raw := strings.Split(path, ".")
	parts := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if names != nil {
			if resolved, ok := names[p]; ok {
				p = resolved
			}
		}
		parts = append(parts, p)
	}
	return parts
}

func compareAttr(lhs ddbtypes.AttributeValue, op string, rhs ddbtypes.AttributeValue) (bool, error) {
	switch a := lhs.(type) {
	case *ddbtypes.AttributeValueMemberS:
		b, ok := rhs.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return false, fmt.Errorf("fakeDDB compare: type mismatch (S vs %T)", rhs)
		}
		return cmpResult(strings.Compare(a.Value, b.Value), op), nil
	case *ddbtypes.AttributeValueMemberN:
		b, ok := rhs.(*ddbtypes.AttributeValueMemberN)
		if !ok {
			return false, fmt.Errorf("fakeDDB compare: type mismatch (N vs %T)", rhs)
		}
		av, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil {
			return false, err
		}
		bv, err := strconv.ParseInt(b.Value, 10, 64)
		if err != nil {
			return false, err
		}
		switch {
		case av < bv:
			return cmpResult(-1, op), nil
		case av > bv:
			return cmpResult(1, op), nil
		default:
			return cmpResult(0, op), nil
		}
	case *ddbtypes.AttributeValueMemberBOOL:
		b, ok := rhs.(*ddbtypes.AttributeValueMemberBOOL)
		if !ok {
			return false, fmt.Errorf("fakeDDB compare: type mismatch (BOOL vs %T)", rhs)
		}
		if op != "=" && op != "<>" {
			return false, fmt.Errorf("fakeDDB compare: BOOL only supports = and <>, got %q", op)
		}
		eq := a.Value == b.Value
		if op == "=" {
			return eq, nil
		}
		return !eq, nil
	}
	return false, fmt.Errorf("fakeDDB compare: unsupported attribute type %T", lhs)
}

// cmpResult turns a strings.Compare-style int + op string into a
// bool.
func cmpResult(c int, op string) bool {
	switch op {
	case "=":
		return c == 0
	case "<>":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// seedSerialize is a debug helper for test failures — renders an
// item map as JSON for inclusion in t.Errorf messages.
func seedSerialize(item map[string]ddbtypes.AttributeValue) string {
	flat := make(map[string]any, len(item))
	for k, v := range item {
		switch a := v.(type) {
		case *ddbtypes.AttributeValueMemberS:
			flat[k] = a.Value
		case *ddbtypes.AttributeValueMemberN:
			flat[k] = a.Value
		case *ddbtypes.AttributeValueMemberBOOL:
			flat[k] = a.Value
		case *ddbtypes.AttributeValueMemberSS:
			flat[k] = a.Value
		default:
			flat[k] = fmt.Sprintf("%T", v)
		}
	}
	b, _ := json.Marshal(flat)
	return string(b)
}

// keep seedSerialize referenced so it's available for tests that
// surface fixture-mismatch errors.
var _ = seedSerialize
