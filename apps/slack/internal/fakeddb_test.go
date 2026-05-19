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
	"errors"
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
	// composite-key construction. workspace_mappings & bootstrap_codes
	// are PK-only; channel_policies is PK+SK.
	keySchemas map[string][]string

	// updateHook is invoked on every UpdateItem if non-nil. Tests use
	// it for "must NOT be called" assertions (e.g., the AllowResource
	// gate that prevents non-admins from reaching the mutation).
	updateHook func(in *dynamodb.UpdateItemInput)
	// putHook mirrors updateHook for PutItem (BindWorkspace).
	putHook func(in *dynamodb.PutItemInput)
	// getItemErrs maps tableName → injected GetItem error.
	getItemErrs map[string]error
	// updateItemErrs maps tableName → injected UpdateItem error.
	updateItemErrs map[string]error
	// putItemErrs maps tableName → injected PutItem error.
	putItemErrs map[string]error
	// queryErrs maps tableName → injected Query error.
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
// (workspace_mappings) or the ResolvePolicy path (channel_policies).
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

// tableNames groups the three table names used across the post-pivot
// production code. Tests construct one of these per fakeDDB so the
// fake and the Store agree on which table maps to which schema.
type tableNames struct {
	workspace      string
	channelPolicy  string
	bootstrapCodes string
}

// defaultTestTableNames returns the canonical table-name set tests
// use. Lifted as a helper because every newAdminTestHandler call site
// needs the same triple.
func defaultTestTableNames() tableNames {
	return tableNames{
		workspace:      "test-workspace-mappings",
		channelPolicy:  "test-channel-policies",
		bootstrapCodes: "test-bootstrap-codes",
	}
}

// newFakeDDB builds an empty in-memory store with the three
// post-pivot tables registered. `seed` may pre-populate items by
// table name; if nil the store starts empty. The t parameter is
// kept for t.Helper / future test-only Fatal calls; today it's
// referenced only to satisfy the signature.
func newFakeDDB(t *testing.T, names tableNames, seed map[string][]map[string]ddbtypes.AttributeValue) *fakeDDB {
	t.Helper()
	f := &fakeDDB{
		tables: map[string]map[string]map[string]ddbtypes.AttributeValue{
			names.workspace:      {},
			names.channelPolicy:  {},
			names.bootstrapCodes: {},
		},
		keySchemas: map[string][]string{
			names.workspace:      {fAttrSlackTeamID},
			names.channelPolicy:  {fAttrSlackTeamID, fAttrSlackChannelID},
			names.bootstrapCodes: {"code_hash"},
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
		slackdata.WithTableNames(names.workspace, names.channelPolicy, names.bootstrapCodes),
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
	if err := applyUpdateExpression(aws.ToString(in.UpdateExpression), existing, in.ExpressionAttributeValues); err != nil {
		return nil, err
	}
	table[key] = existing
	out := &dynamodb.UpdateItemOutput{}
	if in.ReturnValues == ddbtypes.ReturnValueAllNew {
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

// Query implements [slackdata.DynamoDBClient] over the
// `slack_team_id = :tid` shape used by [slackdata.Store.ListPolicies]
// and [slackdata.Store.countPoliciesForTeam]. Honors Limit,
// ExclusiveStartKey, and Select=COUNT.
//
// We don't parse the KeyConditionExpression — the only shape in use
// is `slack_team_id = :tid`. If a future caller adds a begins_with
// or SK predicate the fake will need to grow with it; until then
// keeping the parser narrow avoids over-engineering.
func (f *fakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.queryErrs[aws.ToString(in.TableName)]; ok {
		return nil, err
	}
	table, schema, err := f.tableAndSchema(aws.ToString(in.TableName))
	if err != nil {
		return nil, err
	}
	if len(schema) < 1 {
		return nil, errors.New("fakeDDB.Query: table has no PK")
	}
	pkAttr := schema[0]
	tidVal, err := requireStringExprValue(in.ExpressionAttributeValues, ":tid")
	if err != nil {
		return nil, err
	}
	// Filter rows whose PK matches :tid, ordered by composite-key
	// string so pagination is deterministic across runs.
	keys := make([]string, 0, len(table))
	for k, item := range table {
		s, ok := item[pkAttr].(*ddbtypes.AttributeValueMemberS)
		if !ok || s.Value != tidVal {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)

	startKey := ""
	if len(in.ExclusiveStartKey) > 0 {
		startKey, err = compositeKey(schema, in.ExclusiveStartKey)
		if err != nil {
			return nil, fmt.Errorf("Query: ExclusiveStartKey: %w", err)
		}
	}

	limit := -1
	if in.Limit != nil {
		limit = int(*in.Limit)
	}

	out := &dynamodb.QueryOutput{}
	collected := 0
	skipping := startKey != ""
	for _, k := range keys {
		if skipping {
			if k == startKey {
				skipping = false
			}
			continue
		}
		out.Items = append(out.Items, cloneItem(table[k]))
		collected++
		out.Count = int32(collected)
		if limit > 0 && collected >= limit {
			// Did we reach the end? If more keys remain after this
			// position, set LastEvaluatedKey to the current row's PK/SK.
			pos := indexOf(keys, k)
			if pos >= 0 && pos < len(keys)-1 {
				out.LastEvaluatedKey = lastEvaluatedKeyFrom(schema, table[k])
			}
			break
		}
	}

	// COUNT-only requests strip Items (matches DDB behavior).
	if in.Select == ddbtypes.SelectCount {
		out.Items = nil
	}
	return out, nil
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

// lastEvaluatedKeyFrom extracts just the PK/SK attrs from an item.
func lastEvaluatedKeyFrom(schema []string, item map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	out := make(map[string]ddbtypes.AttributeValue, len(schema))
	for _, attr := range schema {
		out[attr] = item[attr]
	}
	return out
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

// sortStrings is a tiny stable insertion sort so the package doesn't
// need an additional import for sort.Strings. The slices are small
// (typical workspace has tens of channel_policies rows) so an O(n^2)
// sort is fine.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// requireStringExprValue reads a string expression-attribute value
// or errors. Used by Query to extract :tid.
func requireStringExprValue(vals map[string]ddbtypes.AttributeValue, name string) (string, error) {
	v, ok := vals[name]
	if !ok {
		return "", fmt.Errorf("fakeDDB: missing expression attribute %q", name)
	}
	s, ok := v.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("fakeDDB: expression attribute %q is not a string", name)
	}
	return s.Value, nil
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
func applyUpdateExpression(expr string, item, vals map[string]ddbtypes.AttributeValue) error {
	if expr == "" {
		return nil
	}
	clauses := splitUpdateClauses(expr)
	for _, c := range clauses {
		switch c.verb {
		case "SET":
			if err := applySetClause(c.body, item, vals); err != nil {
				return err
			}
		case "ADD":
			if err := applyAddClause(c.body, item, vals); err != nil {
				return err
			}
		case "DELETE":
			if err := applyDeleteClause(c.body, item, vals); err != nil {
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
// Values are either `:vN` tokens (literal substitutions from
// ExpressionAttributeValues) or `if_not_exists(<attr>, :vN)` —
// the latter preserves the existing item attribute when present.
func applySetClause(body string, item, vals map[string]ddbtypes.AttributeValue) error {
	pairs := splitTopLevelCommas(body)
	for _, p := range pairs {
		eq := strings.Index(p, "=")
		if eq < 0 {
			return fmt.Errorf("fakeDDB SET: expected '=' in %q", p)
		}
		attr := strings.TrimSpace(p[:eq])
		valTok := strings.TrimSpace(p[eq+1:])
		// `if_not_exists(<attr>, :val)` — short-circuits the SET when
		// the named attribute already exists on the item. Production
		// AllowResource uses this on `created_at` so the row-creation
		// path stamps the timestamp once and subsequent allow-on-same-
		// channel calls preserve it.
		if strings.HasPrefix(valTok, "if_not_exists(") && strings.HasSuffix(valTok, ")") {
			inner := strings.TrimSuffix(strings.TrimPrefix(valTok, "if_not_exists("), ")")
			comma := strings.Index(inner, ",")
			if comma < 0 {
				return fmt.Errorf("fakeDDB SET: malformed if_not_exists in %q", valTok)
			}
			refAttr := strings.TrimSpace(inner[:comma])
			refVal := strings.TrimSpace(inner[comma+1:])
			if _, present := item[refAttr]; present {
				// Existing attribute wins — leave it alone.
				continue
			}
			v, ok := vals[refVal]
			if !ok {
				return fmt.Errorf("fakeDDB SET if_not_exists: unknown value %q", refVal)
			}
			item[attr] = v
			continue
		}
		v, ok := vals[valTok]
		if !ok {
			return fmt.Errorf("fakeDDB SET: unknown value %q", valTok)
		}
		item[attr] = v
	}
	return nil
}

// applyAddClause handles `<attr> :v`. Currently only supports the
// SS (string-set) merge form used by AllowResource.
func applyAddClause(body string, item, vals map[string]ddbtypes.AttributeValue) error {
	body = strings.TrimSpace(body)
	parts := strings.Fields(body)
	if len(parts) != 2 {
		return fmt.Errorf("fakeDDB ADD: expected `<attr> :value`, got %q", body)
	}
	attr := parts[0]
	v, ok := vals[parts[1]]
	if !ok {
		return fmt.Errorf("fakeDDB ADD: unknown value %q", parts[1])
	}
	incoming, ok := v.(*ddbtypes.AttributeValueMemberSS)
	if !ok {
		return fmt.Errorf("fakeDDB ADD: only string-set ADD is supported, got %T", v)
	}
	existing, _ := item[attr].(*ddbtypes.AttributeValueMemberSS)
	merged := mergeStringSet(existing, incoming.Value)
	item[attr] = &ddbtypes.AttributeValueMemberSS{Value: merged}
	return nil
}

// applyDeleteClause handles `<attr> :v`. Currently only supports the
// SS (string-set) remove form used by DisallowResource. Removing the
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

// evalCondition supports the exact ConditionExpression shapes the
// production code emits, joined by " AND ". Each subexpression is
// one of:
//
//	attribute_exists(<attr>)
//	attribute_not_exists(<attr>)
//	<attr> = :val
//	<attr> > :val
//
// Returns (true, nil) when every subexpression is satisfied. We do
// NOT parse OR — except a single top-level
// `attribute_not_exists(<pk>) OR <attr> = :val` shape used by
// [slackdata.Store.BindWorkspace], which we special-case below.
func evalCondition(expr string, item map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue, _ map[string]string) (bool, error) {
	expr = strings.TrimSpace(expr)
	// Special-case BindWorkspace's
	//   `attribute_not_exists(<pk>) OR <attr> = :val`
	// shape: a top-level OR with exactly two subexpressions. We bail
	// to false only if both halves fail.
	if strings.Contains(expr, " OR ") {
		halves := strings.SplitN(expr, " OR ", 2)
		for _, h := range halves {
			ok, err := evalCondition(strings.TrimSpace(h), item, present, vals, nil)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	parts := strings.Split(expr, " AND ")
	for _, p := range parts {
		ok, err := evalConditionTerm(strings.TrimSpace(p), item, present, vals)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalConditionTerm(term string, item map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue) (bool, error) {
	switch {
	case strings.HasPrefix(term, "attribute_exists("):
		attr := strings.TrimSuffix(strings.TrimPrefix(term, "attribute_exists("), ")")
		if !present {
			return false, nil
		}
		_, ok := item[attr]
		return ok, nil
	case strings.HasPrefix(term, "attribute_not_exists("):
		attr := strings.TrimSuffix(strings.TrimPrefix(term, "attribute_not_exists("), ")")
		if !present {
			return true, nil
		}
		_, ok := item[attr]
		return !ok, nil
	case strings.HasPrefix(term, "NOT contains("):
		// `NOT contains(<attr>, :val)` — true iff the SS attribute
		// at <attr> does NOT contain :val. Used by AllowResource's
		// "is :rid already in the set?" guard to fold the membership
		// check into the conditional UpdateItem.
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
		raw, ok := item[attr]
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
		// <attr> contains :val. Used by DisallowResource's guard
		// (membership-required-for-removal) and any future verb
		// that needs a positive membership check inside the
		// conditional UpdateItem.
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
		raw, ok := item[attr]
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
		lhs, ok := item[attr]
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
