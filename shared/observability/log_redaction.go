package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
)

const (
	jsonTagName       = "json"
	logKeyHash        = "hash"
	logKeyPrivateKey  = "private_key"
	maxRedactionDepth = 5
	redactedLogValue  = "[REDACTED]"
)

// Mirrors Discord's substring policy exactly; intentionally do not broaden
// this to generic names like "key" without changing both implementations.
// This mirrors Discord's redact() path, not its wider audit() secret list;
// #221 should account for both lists when consolidating the definitions.
var redactSubstrings = [...]string{
	"token",
	"secret",
	"password",
	"authorization",
	"apikey",
	"api_key",
}

var contentHashLogKeys = [...]string{
	logKeyHash,
	"md5",
	"sha1",
	"sha256",
	"sha512",
	"digest",
	"checksum",
	"content_hash",
	"body_hash",
}

// Keep this mirror discoverable with the Discord logger until #221
// consolidates the cross-app redaction key definitions.
var redactExactKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(contentHashLogKeys)+1)
	for _, key := range contentHashLogKeys {
		keys[key] = struct{}{}
	}
	keys[logKeyPrivateKey] = struct{}{}
	return keys
}()

// NewRedactingJSONHandler returns a slog JSON handler that redacts
// secret-shaped attributes before emission.
func NewRedactingJSONHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	return NewRedactingHandler(slog.NewJSONHandler(w, opts))
}

// NewRedactingHandler wraps next with qURL's shared log redaction policy.
// For Discord parity, matched keys blank non-empty strings, byte sequences, and
// JSON-marshaled string scalars. Other scalars, including numbers, are
// preserved; matched-key containers and collections are walked by inner field
// names rather than fully suppressed. Map values are walked only when their Go
// map keys are strings.
// Struct values passed through slog.Any are only reflected when nested
// redaction changes them. In that case, rich struct output can differ from
// encoding/json details like omitempty, string coercion, or embedded-field
// flattening.
func NewRedactingHandler(next slog.Handler) slog.Handler {
	return redactingHandler{next: next}
}

type redactingHandler struct {
	next slog.Handler
}

// Enabled implements slog.Handler.
func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error { //nolint:gocritic // slog.Handler requires slog.Record by value.
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr, 0))
		return true
	})
	return h.next.Handle(ctx, clean)
}

// WithAttrs implements slog.Handler.
func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redactingHandler{next: h.next.WithAttrs(redactAttrs(attrs, 0))}
}

// WithGroup implements slog.Handler.
func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttrs(attrs []slog.Attr, depth int) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, redactAttr(attr, depth))
	}
	return out
}

func redactAttr(attr slog.Attr, depth int) slog.Attr {
	if shouldRedactKey(attr.Key) {
		attr.Value = redactMatchedValue(attr.Value, depth+1)
		return attr
	}

	attr.Value = redactValue(attr.Value, depth+1)
	return attr
}

func redactValue(value slog.Value, depth int) slog.Value {
	return redactResolvedValue(value.Resolve(), depth)
}

func redactResolvedValue(value slog.Value, depth int) slog.Value {
	if depth > maxRedactionDepth {
		return value
	}
	kind := value.Kind()
	if kind == slog.KindGroup {
		// slog.Group itself does not consume a depth level; child attrs do.
		// That leaves one extra wrapper versus equivalent slog.Any maps.
		return slog.GroupValue(redactAttrs(value.Group(), depth)...)
	}
	if kind == slog.KindAny {
		if redacted, changed := redactAny(value.Any(), depth); changed {
			return slog.AnyValue(redacted)
		}
	}
	return value
}

func redactMatchedValue(value slog.Value, depth int) slog.Value {
	value = value.Resolve()
	if value.Kind() == slog.KindString && value.String() != "" {
		return slog.StringValue(redactedLogValue)
	}
	if value.Kind() == slog.KindAny && matchedScalarNeedsRedaction(value.Any()) {
		return slog.StringValue(redactedLogValue)
	}
	// Match Discord's behavior: only non-empty strings and byte sequences are
	// blanked. Other scalars, including numbers, pass through; containers are
	// walked so nested sensitive fields can still be found. A matched object
	// such as slog.Any("token", SomeStruct{Public: "ok"}) can still emit safe
	// inner fields; this is a parity backstop, not an unconditional guarantee
	// for every token-keyed value.
	return redactResolvedValue(value, depth)
}

func redactAny(value any, depth int) (any, bool) {
	// This intentionally mirrors the Discord logger's depth-5 contract:
	// beyond the cap, values pass through unchanged rather than risking an
	// unbounded walk. Redaction is a bounded backstop, not a complete guarantee
	// for arbitrarily deep payloads.
	if depth > maxRedactionDepth || value == nil {
		return value, false
	}

	if marshaler, ok := value.(json.Marshaler); ok {
		return redactJSONMarshaled(marshaler, value, depth)
	}
	// json.RawMessage is a byte sequence, but it can contain nested fields
	// that need walking. json.Marshaler handling above catches it first.
	if isNonEmptyByteSequence(value) {
		return value, false
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return value, false
		}
		rv = rv.Elem()
	}

	kind := rv.Kind()
	if kind == reflect.Map {
		if !mapNeedsRedaction(rv, depth) {
			return value, false
		}
		return redactMap(value, rv, depth)
	}
	if kind == reflect.Struct {
		redacted, changed := redactStruct(value, rv, depth)
		if !changed {
			return value, false
		}
		return redacted, true
	}
	if kind == reflect.Slice || kind == reflect.Array {
		if !sliceOrArrayNeedsRedaction(rv, depth) {
			return value, false
		}
		return redactSliceOrArray(value, rv, depth)
	}
	return value, false
}

func redactMap(value any, rv reflect.Value, depth int) (any, bool) {
	if rv.Type().Key().Kind() != reflect.String {
		return value, false
	}
	out := make(map[string]any, rv.Len())
	changed := false
	iter := rv.MapRange()
	for iter.Next() {
		key := iter.Key().String()
		redacted, fieldChanged := redactAnyField(key, iter.Value().Interface(), depth)
		if fieldChanged {
			changed = true
		}
		out[key] = redacted
	}
	if !changed {
		return value, false
	}
	return out, true
}

func redactStruct(value any, rv reflect.Value, depth int) (any, bool) {
	if !structNeedsRedaction(rv, depth) {
		return value, false
	}
	// The reflected scan only decides whether this struct might change. The
	// JSON walk is authoritative for emitted shape; if JSON emits no sensitive
	// field (for example because of omitempty or a json tag), keep the original.
	// This can invoke custom MarshalJSON methods after the scan; log payload
	// marshalers should be idempotent and cheap enough for the redacted path.
	if redacted, jsonChanged, ok := redactJSONEncoded(value, depth); ok {
		return redacted, jsonChanged
	}
	// If encoding/json cannot represent the struct, fall back to the reflected
	// walk so sensitive exported fields are still not emitted in the clear.
	reflected, _ := redactStructFields(rv, depth)
	return reflected, true
}

func structNeedsRedaction(rv reflect.Value, depth int) bool {
	// This reflect gate is only an optimization; redactJSONEncoded remains
	// authoritative for the emitted field shape whenever the gate returns true.
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		field := rt.Field(i)
		name := logFieldName(&field)
		if name == "" || !rv.Field(i).CanInterface() {
			continue
		}
		if anonymousJSONFieldIsFlattened(&field) {
			if anyNeedsRedaction(rv.Field(i).Interface(), depth) {
				return true
			}
			continue
		}
		if fieldNeedsRedaction(name, rv.Field(i).Interface(), depth) {
			return true
		}
	}
	return false
}

func anonymousJSONFieldIsFlattened(field *reflect.StructField) bool {
	if !field.Anonymous {
		return false
	}
	tag := field.Tag.Get(jsonTagName)
	if tag == "-" {
		return false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" {
		return false
	}

	fieldType := field.Type
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	return fieldType.Kind() == reflect.Struct
}

func fieldNeedsRedaction(key string, value any, depth int) bool {
	if shouldRedactKey(key) && matchedScalarNeedsRedaction(value) {
		return true
	}
	return anyNeedsRedaction(value, depth+1)
}

func anyNeedsRedaction(value any, depth int) bool {
	if depth > maxRedactionDepth || value == nil {
		return false
	}
	if marshaler, ok := value.(json.Marshaler); ok {
		return jsonMarshaledNeedsRedaction(marshaler, depth)
	}
	if isNonEmptyByteSequence(value) {
		return false
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}

	kind := rv.Kind()
	if kind == reflect.Map {
		return mapNeedsRedaction(rv, depth)
	}
	if kind == reflect.Struct {
		return structNeedsRedaction(rv, depth)
	}
	if kind == reflect.Slice || kind == reflect.Array {
		return sliceOrArrayNeedsRedaction(rv, depth)
	}
	return false
}

func mapNeedsRedaction(rv reflect.Value, depth int) bool {
	if rv.Type().Key().Kind() != reflect.String {
		// encoding/json can stringify some non-string map keys, but log
		// redaction intentionally only interprets string-keyed fields.
		return false
	}
	iter := rv.MapRange()
	for iter.Next() {
		if fieldNeedsRedaction(iter.Key().String(), iter.Value().Interface(), depth) {
			return true
		}
	}
	return false
}

func sliceOrArrayNeedsRedaction(rv reflect.Value, depth int) bool {
	for i := 0; i < rv.Len(); i++ {
		if anyNeedsRedaction(rv.Index(i).Interface(), depth+1) {
			return true
		}
	}
	return false
}

func jsonMarshaledNeedsRedaction(marshaler json.Marshaler, depth int) bool {
	data, err := marshaler.MarshalJSON()
	if err != nil {
		return false
	}
	decoded, err := decodeJSONAny(data)
	if err != nil {
		return false
	}
	return anyNeedsRedaction(decoded, depth)
}

func redactStructFields(rv reflect.Value, depth int) (any, bool) {
	out := make(map[string]any, rv.NumField())
	changed := false
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		field := rt.Field(i)
		name := logFieldName(&field)
		if name == "" || !rv.Field(i).CanInterface() {
			continue
		}
		redacted, fieldChanged := redactAnyField(name, rv.Field(i).Interface(), depth)
		if fieldChanged {
			changed = true
		}
		out[name] = redacted
	}
	return out, changed
}

func redactSliceOrArray(value any, rv reflect.Value, depth int) (any, bool) {
	out := make([]any, rv.Len())
	changed := false
	for i := 0; i < rv.Len(); i++ {
		redacted, elemChanged := redactAny(rv.Index(i).Interface(), depth+1)
		if elemChanged {
			changed = true
		}
		out[i] = redacted
	}
	if !changed {
		return value, false
	}
	return out, true
}

func redactJSONMarshaled(marshaler json.Marshaler, fallback any, depth int) (any, bool) {
	data, err := marshaler.MarshalJSON()
	if err != nil {
		return fallback, false
	}
	return redactJSONBytes(data, fallback, depth)
}

func redactJSONEncoded(value any, depth int) (redacted any, changed, ok bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return value, false, false
	}
	redacted, changed = redactJSONBytes(data, value, depth)
	return redacted, changed, true
}

func redactJSONBytes(data []byte, fallback any, depth int) (any, bool) {
	decoded, err := decodeJSONAny(data)
	if err != nil {
		return fallback, false
	}
	redacted, changed := redactAny(decoded, depth)
	if !changed {
		return fallback, false
	}
	return redacted, true
}

func decodeJSONAny(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func logFieldName(field *reflect.StructField) string {
	tag := field.Tag.Get(jsonTagName)
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" {
		return name
	}
	if field.PkgPath != "" {
		return ""
	}
	return field.Name
}

func redactAnyField(key string, value any, depth int) (any, bool) {
	if shouldRedactKey(key) {
		if matchedScalarNeedsRedaction(value) {
			return redactedLogValue, true
		}
	}
	return redactAny(value, depth+1)
}

func matchedScalarNeedsRedaction(value any) bool {
	return isNonEmptyStringValue(value) ||
		isNonEmptyByteSequence(value) ||
		isNonEmptyJSONMarshaledString(value)
}

func isNonEmptyStringValue(value any) bool {
	if _, ok := value.(json.Number); ok {
		return false
	}
	rv := reflect.ValueOf(value)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.CanInterface() {
		if _, ok := rv.Interface().(json.Number); ok {
			return false
		}
	}
	return rv.IsValid() && rv.Kind() == reflect.String && rv.Len() > 0
}

func isNonEmptyJSONMarshaledString(value any) bool {
	marshaler, ok := value.(json.Marshaler)
	if !ok {
		return false
	}
	data, err := marshaler.MarshalJSON()
	if err != nil {
		return false
	}
	decoded, err := decodeJSONAny(data)
	if err != nil {
		return false
	}
	s, ok := decoded.(string)
	return ok && s != ""
}

func isNonEmptyByteSequence(value any) bool {
	rv := reflect.ValueOf(value)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return false
	}
	return rv.Type().Elem().Kind() == reflect.Uint8 && rv.Len() > 0
}

func shouldRedactKey(key string) bool {
	k := strings.ToLower(key)
	if _, ok := redactExactKeys[k]; ok {
		return true
	}
	for _, substring := range redactSubstrings {
		if strings.Contains(k, substring) {
			return true
		}
	}
	return false
}
