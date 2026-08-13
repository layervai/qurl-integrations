package teamsdata

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDDB is an in-memory DynamoDBClient that evaluates update and condition
// expressions instead of recording them.
//
// A stub that only captures the emitted UpdateExpression string cannot fail on
// an expression DynamoDB itself would reject, which is how three defects
// reached review: a SET of alias_bindings.#alias against a row with no
// alias_bindings map, a SET that seeded personal_conversation_refs and wrote
// personal_conversation_refs.#actor in one statement, and an empty
// ExpressionAttributeNames map on the no-alias revoke path. This fake models
// the rules those violated:
//
//   - a nested document path is only writable when its parent map exists
//     ("The document path provided in the update expression is invalid for
//     update"),
//   - one update expression may not touch two overlapping document paths
//     ("Two document paths overlap with each other"), and
//   - ExpressionAttributeNames, when sent at all, must be non-empty.
//
// It implements the subset of DynamoDB this package uses, and fails loudly on
// anything outside that subset so it cannot silently accept a new expression
// shape it does not actually model.
type fakeDDB struct {
	mu sync.Mutex
	// items maps table name -> encoded primary key -> item.
	items map[string]map[string]map[string]ddbtypes.AttributeValue
	// keyAttrs maps table name -> primary key attribute names, in key order.
	keyAttrs map[string][]string
}

func newFakeDDB(keyAttrs map[string][]string) *fakeDDB {
	return &fakeDDB{
		items:    map[string]map[string]map[string]ddbtypes.AttributeValue{},
		keyAttrs: keyAttrs,
	}
}

func (f *fakeDDB) encodeKey(table string, key map[string]ddbtypes.AttributeValue) (string, error) {
	attrs, ok := f.keyAttrs[table]
	if !ok {
		return "", fmt.Errorf("fakeDDB: unknown table %q", table)
	}
	parts := make([]string, 0, len(attrs))
	for _, a := range attrs {
		s, ok := key[a].(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("fakeDDB: table %q missing string key attribute %q", table, a)
		}
		parts = append(parts, s.Value)
	}
	return strings.Join(parts, "\x00"), nil
}

// seed installs an item directly, bypassing expression evaluation.
func (f *fakeDDB) seed(table string, item map[string]ddbtypes.AttributeValue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := f.encodeKey(table, item)
	if err != nil {
		panic(err)
	}
	if f.items[table] == nil {
		f.items[table] = map[string]map[string]ddbtypes.AttributeValue{}
	}
	f.items[table][k] = item
}

// snapshot returns a copy of the stored item, if present.
func (f *fakeDDB) snapshot(table string, key map[string]ddbtypes.AttributeValue) (map[string]ddbtypes.AttributeValue, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := f.encodeKey(table, key)
	if err != nil {
		panic(err)
	}
	item, ok := f.items[table][k]
	if !ok {
		return nil, false
	}
	return copyItem(item), true
}

func copyItem(in map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	out := make(map[string]ddbtypes.AttributeValue, len(in))
	for k, v := range in {
		switch tv := v.(type) {
		case *ddbtypes.AttributeValueMemberM:
			out[k] = &ddbtypes.AttributeValueMemberM{Value: copyItem(tv.Value)}
		case *ddbtypes.AttributeValueMemberSS:
			out[k] = &ddbtypes.AttributeValueMemberSS{Value: append([]string(nil), tv.Value...)}
		default:
			out[k] = v
		}
	}
	return out
}

// checkExpressionAttributeNames rejects a non-nil but empty map. The AWS SDK
// serializes ExpressionAttributeNames whenever it is non-nil, and DynamoDB
// rejects the resulting empty object, so callers must leave it nil rather than
// pass an empty map.
func checkExpressionAttributeNames(names map[string]string) error {
	if names != nil && len(names) == 0 {
		return fmt.Errorf("ValidationException: ExpressionAttributeNames must not be empty")
	}
	return nil
}

func (f *fakeDDB) GetItem(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if err := checkExpressionAttributeNames(params.ExpressionAttributeNames); err != nil {
		return nil, err
	}
	table := aws.ToString(params.TableName)
	item, ok := f.snapshot(table, params.Key)
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	proj := aws.ToString(params.ProjectionExpression)
	if proj == "" {
		return &dynamodb.GetItemOutput{Item: item}, nil
	}
	projected := map[string]ddbtypes.AttributeValue{}
	for _, raw := range strings.Split(proj, ",") {
		path, err := resolvePath(strings.TrimSpace(raw), params.ExpressionAttributeNames)
		if err != nil {
			return nil, err
		}
		if err := projectPath(item, projected, path); err != nil {
			return nil, err
		}
	}
	return &dynamodb.GetItemOutput{Item: projected}, nil
}

// projectPath copies path (top-level attribute, or one level of map nesting)
// from src into dst, preserving the nesting.
func projectPath(src, dst map[string]ddbtypes.AttributeValue, path []string) error {
	switch len(path) {
	case 1:
		if v, ok := src[path[0]]; ok {
			dst[path[0]] = v
		}
		return nil
	case 2:
		parent, ok := src[path[0]].(*ddbtypes.AttributeValueMemberM)
		if !ok {
			return nil
		}
		v, ok := parent.Value[path[1]]
		if !ok {
			return nil
		}
		existing, ok := dst[path[0]].(*ddbtypes.AttributeValueMemberM)
		if !ok {
			existing = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{}}
			dst[path[0]] = existing
		}
		existing.Value[path[1]] = v
		return nil
	default:
		return fmt.Errorf("fakeDDB: unsupported projection depth %v", path)
	}
}

func (f *fakeDDB) PutItem(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	table := aws.ToString(params.TableName)
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := f.encodeKey(table, params.Item)
	if err != nil {
		return nil, err
	}
	if f.items[table] == nil {
		f.items[table] = map[string]map[string]ddbtypes.AttributeValue{}
	}
	f.items[table][k] = copyItem(params.Item)
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) DeleteItem(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	table := aws.ToString(params.TableName)
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := f.encodeKey(table, params.Key)
	if err != nil {
		return nil, err
	}
	delete(f.items[table], k)
	return &dynamodb.DeleteItemOutput{}, nil
}

// Query supports only the "<hash key> = :v" form this package emits, returning
// every item sharing that hash key.
func (f *fakeDDB) Query(_ context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	table := aws.ToString(params.TableName)
	cond := aws.ToString(params.KeyConditionExpression)
	name, valueRef, ok := strings.Cut(cond, " = ")
	if !ok {
		return nil, fmt.Errorf("fakeDDB: unsupported KeyConditionExpression %q", cond)
	}
	want, ok := params.ExpressionAttributeValues[strings.TrimSpace(valueRef)].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("fakeDDB: unsupported key condition value %q", valueRef)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.items[table]))
	for k := range f.items[table] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := &dynamodb.QueryOutput{}
	for _, k := range keys {
		item := f.items[table][k]
		if s, ok := item[strings.TrimSpace(name)].(*ddbtypes.AttributeValueMemberS); ok && s.Value == want.Value {
			out.Items = append(out.Items, copyItem(item))
		}
	}
	return out, nil
}

func (f *fakeDDB) UpdateItem(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if err := checkExpressionAttributeNames(params.ExpressionAttributeNames); err != nil {
		return nil, err
	}
	table := aws.ToString(params.TableName)
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := f.encodeKey(table, params.Key)
	if err != nil {
		return nil, err
	}
	if f.items[table] == nil {
		f.items[table] = map[string]map[string]ddbtypes.AttributeValue{}
	}
	existing, found := f.items[table][k]
	// DynamoDB upserts on UpdateItem: an absent row starts as its key attributes.
	item := map[string]ddbtypes.AttributeValue{}
	if found {
		item = copyItem(existing)
	} else {
		for name, v := range params.Key {
			item[name] = v
		}
	}

	if cond := aws.ToString(params.ConditionExpression); cond != "" {
		ok, err := evalCondition(cond, item, found, params.ExpressionAttributeNames, params.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
	}

	if err := applyUpdateExpression(item, aws.ToString(params.UpdateExpression), params.ExpressionAttributeNames, params.ExpressionAttributeValues); err != nil {
		return nil, err
	}
	f.items[table][k] = item
	return &dynamodb.UpdateItemOutput{}, nil
}

// resolvePath splits a document path into its components and substitutes any
// #name placeholders, rejecting placeholders that were never declared.
func resolvePath(raw string, names map[string]string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "#") {
			resolved, ok := names[p]
			if !ok {
				return nil, fmt.Errorf("fakeDDB: undefined expression attribute name %q", p)
			}
			p = resolved
		}
		out = append(out, p)
	}
	return out, nil
}

func lookupPath(item map[string]ddbtypes.AttributeValue, path []string) (ddbtypes.AttributeValue, bool) {
	cur := item
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		m, ok := v.(*ddbtypes.AttributeValueMemberM)
		if !ok {
			return nil, false
		}
		cur = m.Value
	}
	return nil, false
}

// applyUpdateExpression evaluates the SET/REMOVE/ADD/DELETE subset this package
// emits, enforcing DynamoDB's overlapping-path and parent-must-exist rules.
func applyUpdateExpression(item map[string]ddbtypes.AttributeValue, expr string, names map[string]string, values map[string]ddbtypes.AttributeValue) error {
	clauses, err := splitClauses(expr)
	if err != nil {
		return err
	}

	// Collect every path the statement touches so overlaps can be rejected the
	// way DynamoDB rejects them, before any mutation is applied.
	var touched [][]string
	for verb, body := range clauses {
		for _, action := range splitActions(body) {
			var rawPath string
			switch verb {
			case "SET":
				lhs, _, ok := strings.Cut(action, "=")
				if !ok {
					return fmt.Errorf("fakeDDB: malformed SET action %q", action)
				}
				rawPath = lhs
			case "REMOVE":
				rawPath = action
			case "ADD", "DELETE":
				fields := strings.Fields(action)
				if len(fields) != 2 {
					return fmt.Errorf("fakeDDB: malformed %s action %q", verb, action)
				}
				rawPath = fields[0]
			}
			path, err := resolvePath(rawPath, names)
			if err != nil {
				return err
			}
			touched = append(touched, path)
		}
	}
	for i := 0; i < len(touched); i++ {
		for j := i + 1; j < len(touched); j++ {
			if pathsOverlap(touched[i], touched[j]) {
				return fmt.Errorf("ValidationException: Invalid UpdateExpression: Two document paths overlap with each other; path one: [%s], path two: [%s]",
					strings.Join(touched[i], ", "), strings.Join(touched[j], ", "))
			}
		}
	}

	for _, verb := range []string{"SET", "REMOVE", "ADD", "DELETE"} {
		body, ok := clauses[verb]
		if !ok {
			continue
		}
		for _, action := range splitActions(body) {
			if err := applyAction(item, verb, action, names, values); err != nil {
				return err
			}
		}
	}
	return nil
}

// pathsOverlap reports whether one path is a prefix of the other, which is what
// DynamoDB treats as an overlap.
func pathsOverlap(a, b []string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func applyAction(item map[string]ddbtypes.AttributeValue, verb, action string, names map[string]string, values map[string]ddbtypes.AttributeValue) error {
	switch verb {
	case "SET":
		lhs, rhs, _ := strings.Cut(action, "=")
		path, err := resolvePath(lhs, names)
		if err != nil {
			return err
		}
		val, err := evalOperand(item, strings.TrimSpace(rhs), names, values)
		if err != nil {
			return err
		}
		return writePath(item, path, val)
	case "REMOVE":
		path, err := resolvePath(action, names)
		if err != nil {
			return err
		}
		return deletePath(item, path)
	case "ADD", "DELETE":
		fields := strings.Fields(action)
		path, err := resolvePath(fields[0], names)
		if err != nil {
			return err
		}
		if len(path) != 1 {
			return fmt.Errorf("fakeDDB: %s only supported on top-level attributes, got %v", verb, path)
		}
		operand, ok := values[fields[1]].(*ddbtypes.AttributeValueMemberSS)
		if !ok {
			return fmt.Errorf("fakeDDB: %s operand %q must be a string set", verb, fields[1])
		}
		return applySetOp(item, verb, path[0], operand.Value)
	}
	return fmt.Errorf("fakeDDB: unsupported verb %q", verb)
}

func applySetOp(item map[string]ddbtypes.AttributeValue, verb, attr string, operand []string) error {
	current := map[string]struct{}{}
	if existing, ok := item[attr].(*ddbtypes.AttributeValueMemberSS); ok {
		for _, v := range existing.Value {
			current[v] = struct{}{}
		}
	} else if _, present := item[attr]; present {
		return fmt.Errorf("ValidationException: %s on non-set attribute %q", verb, attr)
	}
	for _, v := range operand {
		if verb == "ADD" {
			current[v] = struct{}{}
		} else {
			delete(current, v)
		}
	}
	if len(current) == 0 {
		// DynamoDB removes a string set attribute once it becomes empty.
		delete(item, attr)
		return nil
	}
	out := make([]string, 0, len(current))
	for v := range current {
		out = append(out, v)
	}
	sort.Strings(out)
	item[attr] = &ddbtypes.AttributeValueMemberSS{Value: out}
	return nil
}

// evalOperand resolves a SET right-hand side: a :value reference, or the
// if_not_exists(path, :value) function.
func evalOperand(item map[string]ddbtypes.AttributeValue, rhs string, names map[string]string, values map[string]ddbtypes.AttributeValue) (ddbtypes.AttributeValue, error) {
	if inner, ok := strings.CutPrefix(rhs, "if_not_exists("); ok {
		inner = strings.TrimSuffix(inner, ")")
		rawPath, rawDefault, ok := strings.Cut(inner, ",")
		if !ok {
			return nil, fmt.Errorf("fakeDDB: malformed if_not_exists %q", rhs)
		}
		path, err := resolvePath(rawPath, names)
		if err != nil {
			return nil, err
		}
		if existing, found := lookupPath(item, path); found {
			return existing, nil
		}
		rhs = strings.TrimSpace(rawDefault)
	}
	v, ok := values[rhs]
	if !ok {
		return nil, fmt.Errorf("fakeDDB: undefined expression attribute value %q", rhs)
	}
	return v, nil
}

// writePath sets a value, rejecting a nested write whose parent map is absent
// exactly as DynamoDB does.
func writePath(item map[string]ddbtypes.AttributeValue, path []string, val ddbtypes.AttributeValue) error {
	if len(path) == 1 {
		item[path[0]] = val
		return nil
	}
	cur := item
	for i := 0; i < len(path)-1; i++ {
		next, ok := cur[path[i]]
		if !ok {
			return fmt.Errorf("ValidationException: The document path provided in the update expression is invalid for update: [%s] does not exist", strings.Join(path[:i+1], "."))
		}
		m, ok := next.(*ddbtypes.AttributeValueMemberM)
		if !ok {
			return fmt.Errorf("ValidationException: The document path provided in the update expression is invalid for update: [%s] is not a map", strings.Join(path[:i+1], "."))
		}
		cur = m.Value
	}
	cur[path[len(path)-1]] = val
	return nil
}

// deletePath removes a value. REMOVE of an absent path is a no-op in DynamoDB.
func deletePath(item map[string]ddbtypes.AttributeValue, path []string) error {
	cur := item
	for i := 0; i < len(path)-1; i++ {
		m, ok := cur[path[i]].(*ddbtypes.AttributeValueMemberM)
		if !ok {
			return nil
		}
		cur = m.Value
	}
	delete(cur, path[len(path)-1])
	return nil
}

func splitClauses(expr string) (map[string]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("fakeDDB: empty UpdateExpression")
	}
	type marker struct {
		verb  string
		index int
	}
	var markers []marker
	for _, verb := range []string{"SET ", "REMOVE ", "ADD ", "DELETE "} {
		search := expr
		offset := 0
		for {
			// Verbs only start a clause at the very beginning of the expression
			// or after a space, never inside an attribute name.
			idx := strings.Index(search, verb)
			if idx == -1 {
				break
			}
			abs := offset + idx
			if abs == 0 || expr[abs-1] == ' ' {
				markers = append(markers, marker{verb: strings.TrimSpace(verb), index: abs})
			}
			offset = abs + len(verb)
			search = expr[offset:]
		}
	}
	if len(markers) == 0 {
		return nil, fmt.Errorf("fakeDDB: no recognised clause in UpdateExpression %q", expr)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].index < markers[j].index })
	if markers[0].index != 0 {
		return nil, fmt.Errorf("fakeDDB: UpdateExpression %q does not start with a clause verb", expr)
	}
	out := map[string]string{}
	for i, m := range markers {
		start := m.index + len(m.verb)
		end := len(expr)
		if i+1 < len(markers) {
			end = markers[i+1].index
		}
		if _, dup := out[m.verb]; dup {
			return nil, fmt.Errorf("fakeDDB: duplicate %s clause in %q", m.verb, expr)
		}
		out[m.verb] = strings.TrimSpace(expr[start:end])
	}
	return out, nil
}

// splitActions splits a clause body on commas that are not inside a function
// call such as if_not_exists(a, :b).
func splitActions(body string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(body[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// evalCondition evaluates the condition subset this package emits:
// attribute_exists / attribute_not_exists / contains, optionally negated with
// NOT and joined with AND.
func evalCondition(expr string, item map[string]ddbtypes.AttributeValue, itemExists bool, names map[string]string, values map[string]ddbtypes.AttributeValue) (bool, error) {
	for _, term := range strings.Split(expr, " AND ") {
		term = strings.TrimSpace(term)
		negate := false
		if rest, ok := strings.CutPrefix(term, "NOT "); ok {
			negate = true
			term = strings.TrimSpace(rest)
		}
		result, err := evalConditionTerm(term, item, itemExists, names, values)
		if err != nil {
			return false, err
		}
		if negate {
			result = !result
		}
		if !result {
			return false, nil
		}
	}
	return true, nil
}

func evalConditionTerm(term string, item map[string]ddbtypes.AttributeValue, itemExists bool, names map[string]string, values map[string]ddbtypes.AttributeValue) (bool, error) {
	switch {
	case strings.HasPrefix(term, "attribute_exists("), strings.HasPrefix(term, "attribute_not_exists("):
		want := strings.HasPrefix(term, "attribute_exists(")
		inner := term[strings.Index(term, "(")+1 : len(term)-1]
		path, err := resolvePath(inner, names)
		if err != nil {
			return false, err
		}
		// An UpdateItem on an absent row evaluates conditions against an empty
		// item, not against the synthesised key attributes.
		found := false
		if itemExists {
			_, found = lookupPath(item, path)
		}
		return found == want, nil
	case strings.HasPrefix(term, "contains("):
		inner := term[len("contains(") : len(term)-1]
		rawPath, rawValue, ok := strings.Cut(inner, ",")
		if !ok {
			return false, fmt.Errorf("fakeDDB: malformed contains %q", term)
		}
		path, err := resolvePath(rawPath, names)
		if err != nil {
			return false, err
		}
		operand, ok := values[strings.TrimSpace(rawValue)].(*ddbtypes.AttributeValueMemberS)
		if !ok {
			return false, fmt.Errorf("fakeDDB: contains operand %q must be a string", rawValue)
		}
		if !itemExists {
			return false, nil
		}
		v, found := lookupPath(item, path)
		if !found {
			return false, nil
		}
		set, ok := v.(*ddbtypes.AttributeValueMemberSS)
		if !ok {
			return false, fmt.Errorf("fakeDDB: contains on non-set attribute %v", path)
		}
		for _, s := range set.Value {
			if s == operand.Value {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("fakeDDB: unsupported condition term %q", term)
	}
}
