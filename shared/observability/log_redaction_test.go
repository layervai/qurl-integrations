package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	discordLoggerPath  = "../../apps/discord/src/logger.js"
	testAccessTokenKey = "access_token"
	testAllowedValue   = "allowed"
	testPrivateKey     = "real-private-key"
	testRealToken      = "real-token"
	testSensitiveValue = "5d41402abc4b2a76b9719d911017c592"
)

type testJSONStringMarshaler struct {
	value string
}

func (m testJSONStringMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.value)
}

type testErrorMarshaler struct{}

func (testErrorMarshaler) MarshalJSON() ([]byte, error) {
	return nil, os.ErrInvalid
}

func TestContentHashLogKeys(t *testing.T) {
	t.Parallel()

	want := []string{
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
	got := append([]string(nil), contentHashLogKeys[:]...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contentHashLogKeys = %#v, want %#v", got, want)
	}
}

func TestDiscordRedactExactKeysStayInSync(t *testing.T) {
	t.Parallel()

	// Temporary shared-to-Discord source coupling: #221 tracks replacing this
	// parser with a consolidated key definition. These guards pin key lists,
	// not the independently implemented recursion and matching semantics.
	want := append([]string(nil), contentHashLogKeys[:]...)
	want = append(want, logKeyPrivateKey)
	sort.Strings(want)

	got := parseDiscordRedactExactKeys(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discord REDACT_EXACT_KEYS = %#v, want %#v", got, want)
	}
}

func TestDiscordRedactSubstringsStayInSync(t *testing.T) {
	t.Parallel()

	want := append([]string(nil), redactSubstrings[:]...)
	sort.Strings(want)

	got := parseDiscordRedactSubstrings(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discord REDACT_SUBSTRINGS = %#v, want %#v", got, want)
	}
}

func TestRedactingJSONHandlerRedactsContentHashKeys(t *testing.T) {
	t.Parallel()

	for _, key := range contentHashLogKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(NewRedactingJSONHandler(&buf, nil))
			logger.Info("uploaded", slog.String(key, testSensitiveValue), slog.String("resource_id", "r1"))

			line := buf.String()
			if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
				t.Fatalf("log line leaked sensitive value: %s", line)
			}

			fields := decodeLogLine(t, line)
			if got := fields[key]; got != redactedLogValue {
				t.Fatalf("field %q = %#v, want %q; line=%s", key, got, redactedLogValue, line)
			}
			if got := fields["resource_id"]; got != "r1" {
				t.Fatalf("resource_id = %#v, want r1; line=%s", got, line)
			}
		})
	}
}

func TestRedactingJSONHandlerRedactsAuditSecretGapKeys(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("key loaded", slog.String(logKeyPrivateKey, testSensitiveValue))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	if got := fields[logKeyPrivateKey]; got != redactedLogValue {
		t.Fatalf("private_key = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerRecursesIntoGroups(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info(
		"uploaded",
		slog.Group("context",
			slog.String(logKeyHash, testSensitiveValue),
			slog.Group("body_hash",
				slog.String("myToken", "real-secret"),
				slog.String("nested_hash", testAllowedValue),
			),
		),
	)

	line := buf.String()
	for _, leaked := range []string{testSensitiveValue, "real-secret"} {
		if bytes.Contains(buf.Bytes(), []byte(leaked)) {
			t.Fatalf("log line leaked %q: %s", leaked, line)
		}
	}

	fields := decodeLogLine(t, line)
	contextFields := fields["context"].(map[string]interface{})
	if got := contextFields[logKeyHash]; got != redactedLogValue {
		t.Fatalf("context.hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	bodyHash := contextFields["body_hash"].(map[string]interface{})
	if got := bodyHash["myToken"]; got != redactedLogValue {
		t.Fatalf("context.body_hash.myToken = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := bodyHash["nested_hash"]; got != testAllowedValue {
		t.Fatalf("context.body_hash.nested_hash = %#v, want allowed; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerRecursesIntoAnyMapsAndSlices(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info(
		"uploaded",
		slog.Any("items", []map[string]any{
			{
				logKeyHash: testSensitiveValue,
				"meta": map[string]string{
					logKeyPrivateKey: testPrivateKey,
					"nested_hash":    testAllowedValue,
				},
			},
		}),
	)

	line := buf.String()
	for _, leaked := range []string{testSensitiveValue, testPrivateKey} {
		if bytes.Contains(buf.Bytes(), []byte(leaked)) {
			t.Fatalf("log line leaked %q: %s", leaked, line)
		}
	}

	fields := decodeLogLine(t, line)
	items := fields["items"].([]interface{})
	item := items[0].(map[string]interface{})
	if got := item[logKeyHash]; got != redactedLogValue {
		t.Fatalf("items[0].hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	meta := item["meta"].(map[string]interface{})
	if got := meta[logKeyPrivateKey]; got != redactedLogValue {
		t.Fatalf("items[0].meta.private_key = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := meta["nested_hash"]; got != testAllowedValue {
		t.Fatalf("items[0].meta.nested_hash = %#v, want allowed; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerDoesNotWalkNonStringKeyMaps(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", map[int]map[string]string{
		1: {testAccessTokenKey: testRealToken},
	}))

	line := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line unexpectedly walked non-string map keys: %s", line)
	}
}

func TestRedactingJSONHandlerRedactsSubstringKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"access_token",
		"api_secret",
		"password",
		"authorization",
		"apikey",
		"api_key",
	} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(NewRedactingJSONHandler(&buf, nil))
			logger.Info("uploaded", slog.String(key, testSensitiveValue))

			line := buf.String()
			if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
				t.Fatalf("log line leaked sensitive value: %s", line)
			}

			fields := decodeLogLine(t, line)
			if got := fields[key]; got != redactedLogValue {
				t.Fatalf("field %q = %#v, want %q; line=%s", key, got, redactedLogValue, line)
			}
		})
	}
}

func TestRedactingJSONHandlerReachesFiveLogicalLevels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"level4": map[string]any{
						testAccessTokenKey: testSensitiveValue,
					},
				},
			},
		},
	}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}
}

func TestRedactingJSONHandlerStopsAfterFiveLogicalLevels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"level4": map[string]any{
						"level5": map[string]any{
							testAccessTokenKey: testSensitiveValue,
						},
					},
				},
			},
		},
	}))

	line := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line unexpectedly redacted beyond depth cap: %s", line)
	}
}

func TestRedactingJSONHandlerGroupsReachFiveLogicalLevels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Group("level1",
		slog.Group("level2",
			slog.Group("level3",
				slog.Group("level4",
					slog.String(testAccessTokenKey, testSensitiveValue),
				),
			),
		),
	))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("group log line leaked sensitive value: %s", line)
	}
}

func TestRedactingJSONHandlerGroupsStopAfterFiveLogicalLevels(t *testing.T) {
	t.Parallel()

	// slog.Group enters child attrs at the current depth, so its cutoff has one
	// more wrapper than the equivalent slog.Any map path.
	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Group("level1",
		slog.Group("level2",
			slog.Group("level3",
				slog.Group("level4",
					slog.Group("level5",
						slog.Group("level6",
							slog.String(testAccessTokenKey, testSensitiveValue),
						),
					),
				),
			),
		),
	))

	line := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("group log line unexpectedly redacted beyond depth cap: %s", line)
	}
}

func TestRedactingJSONHandlerRedactsDeepPromotedJSONFields(t *testing.T) {
	t.Parallel()

	type Leaf struct {
		Token string `json:"token"`
	}
	type Level5 struct {
		Leaf
	}
	type Level4 struct {
		Level5
	}
	type Level3 struct {
		Level4
	}
	type Level2 struct {
		Level3
	}
	type Level1 struct {
		Level2
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", Level1{
		Level2: Level2{
			Level3: Level3{
				Level4: Level4{
					Level5: Level5{
						Leaf: Leaf{Token: testRealToken},
					},
				},
			},
		},
	}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked promoted token: %s", line)
	}

	fields := decodeLogLine(t, line)
	payload := fields["payload"].(map[string]interface{})
	if got := payload["token"]; got != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerPreservesAnyStructShapeWithoutRedaction(t *testing.T) {
	t.Parallel()

	type payload struct {
		Count int    `json:"count,string"`
		Empty string `json:"empty,omitempty"`
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("shape", slog.Any("payload", payload{Count: 7}))

	line := buf.String()
	fields := decodeLogLine(t, line)
	got := fields["payload"].(map[string]interface{})
	if got["count"] != "7" {
		t.Fatalf("payload.count = %#v, want string 7; line=%s", got["count"], line)
	}
	if _, ok := got["empty"]; ok {
		t.Fatalf("payload.empty was emitted despite omitempty; line=%s", line)
	}
}

func TestRedactingJSONHandlerUsesJSONStructShapeWhenRedacting(t *testing.T) {
	t.Parallel()

	type payload struct {
		Token string `json:"token"`
		Count int    `json:"count,string"`
		Empty string `json:"empty,omitempty"`
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("shape", slog.Any("payload", payload{Token: testRealToken, Count: 7}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked token: %s", line)
	}

	fields := decodeLogLine(t, line)
	got := fields["payload"].(map[string]interface{})
	if got["token"] != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got["token"], redactedLogValue, line)
	}
	if got["count"] != "7" {
		t.Fatalf("payload.count = %#v, want string 7; line=%s", got["count"], line)
	}
	if _, ok := got["empty"]; ok {
		t.Fatalf("payload.empty was emitted despite omitempty; line=%s", line)
	}
}

func TestRedactingJSONHandlerFallsBackToReflectedStructWhenJSONMarshalFails(t *testing.T) {
	t.Parallel()

	type payload struct {
		Token string             `json:"token"`
		Bad   testErrorMarshaler `json:"bad"`
	}

	redacted, changed := redactAny(payload{Token: testRealToken}, 0)
	if !changed {
		t.Fatal("redactAny changed = false, want true")
	}
	fields, ok := redacted.(map[string]any)
	if !ok {
		t.Fatalf("redacted type = %T, want map[string]any", redacted)
	}
	if got := fields["token"]; got != redactedLogValue {
		t.Fatalf("token = %#v, want %q", got, redactedLogValue)
	}
}

func TestRedactingJSONHandlerPreservesLargeNumbersWhenRedacting(t *testing.T) {
	t.Parallel()

	const largeID = "9007199254740993"
	type payload struct {
		Token string `json:"token"`
		ID    uint64 `json:"id"`
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("shape", slog.Any("payload", payload{Token: testRealToken, ID: 9007199254740993}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked token: %s", line)
	}

	fields := decodeLogLineWithNumbers(t, line)
	got := fields["payload"].(map[string]interface{})
	if got["token"] != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got["token"], redactedLogValue, line)
	}
	id, ok := got["id"].(json.Number)
	if !ok {
		t.Fatalf("payload.id type = %T, want json.Number; line=%s", got["id"], line)
	}
	if id.String() != largeID {
		t.Fatalf("payload.id = %s, want %s; line=%s", id.String(), largeID, line)
	}
}

func TestRedactingJSONHandlerRecursesIntoAnyStructPointers(t *testing.T) {
	t.Parallel()

	type nestedPayload struct {
		PrivateKey string `json:"private_key"`
		NestedHash string `json:"nested_hash"`
	}
	type uploadPayload struct {
		Token    string         `json:"token"`
		BodyHash string         `json:"body_hash"`
		Public   string         `json:"public"`
		Nested   *nestedPayload `json:"nested"`
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", &uploadPayload{
		Token:    "real-token",
		BodyHash: testSensitiveValue,
		Public:   testAllowedValue,
		Nested: &nestedPayload{
			PrivateKey: testPrivateKey,
			NestedHash: testAllowedValue,
		},
	}))

	line := buf.String()
	for _, leaked := range []string{"real-token", testSensitiveValue, testPrivateKey} {
		if bytes.Contains(buf.Bytes(), []byte(leaked)) {
			t.Fatalf("log line leaked %q: %s", leaked, line)
		}
	}

	fields := decodeLogLine(t, line)
	payload := fields["payload"].(map[string]interface{})
	if got := payload["token"]; got != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := payload["body_hash"]; got != redactedLogValue {
		t.Fatalf("payload.body_hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := payload["public"]; got != testAllowedValue {
		t.Fatalf("payload.public = %#v, want allowed; line=%s", got, line)
	}
	nested := payload["nested"].(map[string]interface{})
	if got := nested[logKeyPrivateKey]; got != redactedLogValue {
		t.Fatalf("payload.nested.private_key = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := nested["nested_hash"]; got != testAllowedValue {
		t.Fatalf("payload.nested.nested_hash = %#v, want allowed; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerRecursesIntoRawMessage(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"context":{"access_token":"` + testRealToken + `","public":"allowed"}}`)

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("payload", slog.Any("payload", raw))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked token: %s", line)
	}

	fields := decodeLogLine(t, line)
	payload := fields["payload"].(map[string]interface{})
	contextFields := payload["context"].(map[string]interface{})
	if got := contextFields[testAccessTokenKey]; got != redactedLogValue {
		t.Fatalf("payload.context.access_token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := contextFields["public"]; got != testAllowedValue {
		t.Fatalf("payload.context.public = %#v, want allowed; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerPreservesSensitiveNamedNumbersInRawMessage(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"context":{"access_token":"` + testRealToken + `","token_count":12345}}`)

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("payload", slog.Any("payload", raw))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked token: %s", line)
	}

	fields := decodeLogLineWithNumbers(t, line)
	payload := fields["payload"].(map[string]interface{})
	contextFields := payload["context"].(map[string]interface{})
	if got := contextFields[testAccessTokenKey]; got != redactedLogValue {
		t.Fatalf("payload.context.access_token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	tokenCount, ok := contextFields["token_count"].(json.Number)
	if !ok {
		t.Fatalf("payload.context.token_count type = %T, want json.Number; line=%s", contextFields["token_count"], line)
	}
	if tokenCount.String() != "12345" {
		t.Fatalf("payload.context.token_count = %s, want 12345; line=%s", tokenCount.String(), line)
	}
}

func TestRedactingJSONHandlerRedactsMatchedByteSlices(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any(logKeyHash, []byte(testSensitiveValue)))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	if got := fields[logKeyHash]; got != redactedLogValue {
		t.Fatalf("hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerRedactsMatchedByteArrays(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any(logKeyHash, [4]byte{1, 2, 3, 4}))

	line := buf.String()
	fields := decodeLogLine(t, line)
	if got := fields[logKeyHash]; got != redactedLogValue {
		t.Fatalf("hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerRedactsMatchedJSONMarshaledStrings(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("token", testJSONStringMarshaler{value: testSensitiveValue}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	if got := fields["token"]; got != redactedLogValue {
		t.Fatalf("token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerRedactsNestedMatchedJSONMarshaledStrings(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("payload", map[string]any{
		"token": testJSONStringMarshaler{value: testSensitiveValue},
	}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	payload := fields["payload"].(map[string]interface{})
	if got := payload["token"]; got != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingJSONHandlerPreservesNumericMatchedValues(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("metric", slog.Int("token", 12345), slog.Int(logKeyHash, 67890))

	line := buf.String()
	fields := decodeLogLine(t, line)
	if got := fields["token"]; got != float64(12345) {
		t.Fatalf("token = %#v, want 12345; line=%s", got, line)
	}
	if got := fields[logKeyHash]; got != float64(67890) {
		t.Fatalf("hash = %#v, want 67890; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerPreservesSensitiveNamedNumbersInRedactedStructs(t *testing.T) {
	t.Parallel()

	type payload struct {
		Token      string `json:"token"`
		TokenCount int    `json:"token_count"`
	}

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("metric", slog.Any("payload", payload{Token: testRealToken, TokenCount: 12345}))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked token: %s", line)
	}

	fields := decodeLogLineWithNumbers(t, line)
	payloadFields := fields["payload"].(map[string]interface{})
	if got := payloadFields["token"]; got != redactedLogValue {
		t.Fatalf("payload.token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	tokenCount, ok := payloadFields["token_count"].(json.Number)
	if !ok {
		t.Fatalf("payload.token_count type = %T, want json.Number; line=%s", payloadFields["token_count"], line)
	}
	if tokenCount.String() != "12345" {
		t.Fatalf("payload.token_count = %s, want 12345; line=%s", tokenCount.String(), line)
	}
}

func TestRedactingJSONHandlerWalksMatchedKeyContainersByInnerNames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info(
		"uploaded",
		slog.Any("token", map[string]any{
			testAccessTokenKey: testRealToken,
			"public":           testAllowedValue,
		}),
		slog.Group("secret",
			slog.String("public", testAllowedValue),
		),
	)

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line leaked nested token: %s", line)
	}

	fields := decodeLogLine(t, line)
	token := fields["token"].(map[string]interface{})
	if got := token[testAccessTokenKey]; got != redactedLogValue {
		t.Fatalf("token.access_token = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
	if got := token["public"]; got != testAllowedValue {
		t.Fatalf("token.public = %#v, want allowed; line=%s", got, line)
	}
	secret := fields["secret"].(map[string]interface{})
	if got := secret["public"]; got != testAllowedValue {
		t.Fatalf("secret.public = %#v, want allowed; line=%s", got, line)
	}
}

func TestRedactingJSONHandlerWalksMatchedStringSlicesByInnerNames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info("uploaded", slog.Any("token", []string{testRealToken}))

	line := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(testRealToken)) {
		t.Fatalf("log line unexpectedly redacted matched-key string slice: %s", line)
	}

	fields := decodeLogLine(t, line)
	token := fields["token"].([]interface{})
	if got := token[0]; got != testRealToken {
		t.Fatalf("token[0] = %#v, want %q; line=%s", got, testRealToken, line)
	}
}

func TestRedactingJSONHandlerPreservesAdjacentHashNames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil))
	logger.Info(
		"deploy",
		slog.String("md5_prefix", "5d41402a"),
		slog.String("sha256_prefix", "abc123"),
		slog.String("commitHash", "def456"),
		slog.String("commit_hash", "ghi789"),
	)

	line := buf.String()
	fields := decodeLogLine(t, line)
	for key, want := range map[string]string{
		"md5_prefix":    "5d41402a",
		"sha256_prefix": "abc123",
		"commitHash":    "def456",
		"commit_hash":   "ghi789",
	} {
		if got := fields[key]; got != want {
			t.Fatalf("%s = %#v, want %q; line=%s", key, got, want, line)
		}
	}
}

func TestRedactingHandlerRedactsAttrsBoundWithWith(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil)).
		With(slog.String(logKeyHash, testSensitiveValue))
	logger.Info("uploaded")

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	if got := fields[logKeyHash]; got != redactedLogValue {
		t.Fatalf("hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingHandlerRedactsAttrsInsideWithGroup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, nil)).
		WithGroup("ctx")
	logger.Info("uploaded", slog.String(logKeyHash, testSensitiveValue))

	line := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(testSensitiveValue)) {
		t.Fatalf("log line leaked sensitive value: %s", line)
	}

	fields := decodeLogLine(t, line)
	ctx := fields["ctx"].(map[string]interface{})
	if got := ctx[logKeyHash]; got != redactedLogValue {
		t.Fatalf("ctx.hash = %#v, want %q; line=%s", got, redactedLogValue, line)
	}
}

func TestRedactingHandlerHonorsEnabled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logger.Info("uploaded", slog.String(logKeyHash, testSensitiveValue))

	if got := buf.String(); got != "" {
		t.Fatalf("Info log with warn-level handler wrote %q", got)
	}
}

func TestRedactingHandlerHandleReturnsInnerError(t *testing.T) {
	t.Parallel()

	handler := NewRedactingHandler(errorHandler{})
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String(logKeyHash, testSensitiveValue))

	if err := handler.Handle(context.Background(), record); err == nil {
		t.Fatal("Handle error = nil, want non-nil")
	}
}

func TestAnonymousJSONFieldIsFlattenedHonorsJSONTags(t *testing.T) {
	t.Parallel()

	structType := reflect.TypeOf(struct{}{})
	stringType := reflect.TypeOf("")
	tests := []struct {
		name  string
		field reflect.StructField
		want  bool
	}{
		{
			name:  "untagged anonymous struct",
			field: reflect.StructField{Name: "Flattened", Type: structType, Anonymous: true},
			want:  true,
		},
		{
			name:  "ignored anonymous struct",
			field: reflect.StructField{Name: "Ignored", Type: structType, Anonymous: true, Tag: `json:"-"`},
			want:  false,
		},
		{
			name:  "named anonymous struct",
			field: reflect.StructField{Name: "Named", Type: structType, Anonymous: true, Tag: `json:"named"`},
			want:  false,
		},
		{
			name:  "plain field",
			field: reflect.StructField{Name: "Plain", Type: stringType},
			want:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := anonymousJSONFieldIsFlattened(&test.field); got != test.want {
				t.Fatalf("anonymousJSONFieldIsFlattened(%s) = %t, want %t", test.field.Name, got, test.want)
			}
		})
	}
}

func BenchmarkRedactingJSONHandlerNoSecretAnyMap(b *testing.B) {
	payload := map[string]any{
		"event":       "upload_success",
		"resource_id": "resource-1",
		"count":       3,
		"nested": map[string]any{
			"workspace": "T123",
			"channel":   "C123",
			"flags":     []string{"visible", "durable", "audited"},
		},
		"items": []map[string]any{
			{"name": "file-a.txt", "size": 1234},
			{"name": "file-b.txt", "size": 5678},
		},
	}
	logger := slog.New(NewRedactingJSONHandler(io.Discard, nil))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.LogAttrs(ctx, slog.LevelInfo, "upload", slog.Any("payload", payload))
	}
}

func BenchmarkRedactingJSONHandlerScalarAttrs(b *testing.B) {
	logger := slog.New(NewRedactingJSONHandler(io.Discard, nil))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.LogAttrs(ctx, slog.LevelInfo, "upload",
			slog.String("event", "upload_success"),
			slog.String("resource_id", "resource-1"),
			slog.Int("count", 3),
		)
	}
}

func BenchmarkRedactingJSONHandlerNoSecretRawMessage(b *testing.B) {
	payload := json.RawMessage(`{"event":"upload_success","resource_id":"resource-1","nested":{"workspace":"T123","channel":"C123","flags":["visible","durable","audited"]}}`)
	logger := slog.New(NewRedactingJSONHandler(io.Discard, nil))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.LogAttrs(ctx, slog.LevelInfo, "upload", slog.Any("payload", payload))
	}
}

func BenchmarkRedactingJSONHandlerNoSecretAnyStruct(b *testing.B) {
	const (
		benchmarkChannel   = "C123"
		benchmarkWorkspace = "T123"
	)

	type nestedPayload struct {
		Workspace string   `json:"workspace"`
		Channel   string   `json:"channel"`
		Flags     []string `json:"flags"`
	}
	type uploadPayload struct {
		Event      string          `json:"event"`
		ResourceID string          `json:"resource_id"`
		Count      int             `json:"count"`
		Nested     nestedPayload   `json:"nested"`
		Items      []nestedPayload `json:"items"`
	}

	payload := uploadPayload{
		Event:      "upload_success",
		ResourceID: "resource-1",
		Count:      3,
		Nested: nestedPayload{
			Workspace: benchmarkWorkspace,
			Channel:   benchmarkChannel,
			Flags:     []string{"visible", "durable", "audited"},
		},
		Items: []nestedPayload{
			{Workspace: benchmarkWorkspace, Channel: benchmarkChannel, Flags: []string{"file-a", "small"}},
			{Workspace: "T456", Channel: "C456", Flags: []string{"file-b", "large"}},
		},
	}
	logger := slog.New(NewRedactingJSONHandler(io.Discard, nil))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.LogAttrs(ctx, slog.LevelInfo, "upload", slog.Any("payload", payload))
	}
}

func BenchmarkRedactingJSONHandlerRedactedAnyStruct(b *testing.B) {
	type nestedPayload struct {
		PrivateKey string `json:"private_key"`
		Public     string `json:"public"`
	}
	type uploadPayload struct {
		Token  string        `json:"token"`
		Count  int           `json:"count"`
		Nested nestedPayload `json:"nested"`
	}

	payload := uploadPayload{
		Token: testRealToken,
		Count: 3,
		Nested: nestedPayload{
			PrivateKey: testPrivateKey,
			Public:     testAllowedValue,
		},
	}
	logger := slog.New(NewRedactingJSONHandler(io.Discard, nil))
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.LogAttrs(ctx, slog.LevelInfo, "upload", slog.Any("payload", payload))
	}
}

func decodeLogLine(t *testing.T, line string) map[string]interface{} {
	t.Helper()

	fields := map[string]interface{}{}
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", line, err)
	}
	return fields
}

func decodeLogLineWithNumbers(t *testing.T, line string) map[string]interface{} {
	t.Helper()

	fields := map[string]interface{}{}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		t.Fatalf("json.Decode(%q): %v", line, err)
	}
	return fields
}

func parseDiscordRedactExactKeys(t *testing.T) []string {
	t.Helper()

	data := readDiscordLogger(t)

	setPattern := regexp.MustCompile(`(?s)const REDACT_EXACT_KEYS = new Set\(\[(.*?)\]\);`)
	setMatch := setPattern.FindSubmatch(data)
	if len(setMatch) != 2 {
		t.Fatalf("REDACT_EXACT_KEYS set not found in %s; update parseDiscordRedactExactKeys if the Discord logger moved or changed shape", discordLoggerPath)
	}

	setBody := stripLineComments(string(setMatch[1]))
	return quotedLiteralsFromJSBlock(t, "REDACT_EXACT_KEYS", setBody)
}

func parseDiscordRedactSubstrings(t *testing.T) []string {
	t.Helper()

	data := readDiscordLogger(t)

	arrayPattern := regexp.MustCompile(`(?s)const REDACT_SUBSTRINGS = \[(.*?)\];`)
	arrayMatch := arrayPattern.FindSubmatch(data)
	if len(arrayMatch) != 2 {
		t.Fatalf("REDACT_SUBSTRINGS array not found in %s; update parseDiscordRedactSubstrings if the Discord logger moved or changed shape", discordLoggerPath)
	}

	arrayBody := stripLineComments(string(arrayMatch[1]))
	return quotedLiteralsFromJSBlock(t, "REDACT_SUBSTRINGS", arrayBody)
}

func readDiscordLogger(t *testing.T) []byte {
	t.Helper()

	data, err := os.ReadFile(discordLoggerPath)
	if err != nil {
		t.Fatalf("read %s: %v", discordLoggerPath, err)
	}
	return data
}

func quotedLiteralsFromJSBlock(t *testing.T, name, block string) []string {
	t.Helper()

	stringPattern := regexp.MustCompile(`["']([^"']+)["']`)
	stringMatches := stringPattern.FindAllStringSubmatch(block, -1)
	if len(stringMatches) == 0 {
		t.Fatalf("%s in %s had no quoted keys; update the parser if the literal changed shape", name, discordLoggerPath)
	}
	keys := make([]string, 0, len(stringMatches))
	for _, match := range stringMatches {
		keys = append(keys, match[1])
	}
	sort.Strings(keys)
	return keys
}

func stripLineComments(source string) string {
	var out strings.Builder
	for _, line := range strings.Split(source, "\n") {
		beforeComment, _, _ := strings.Cut(line, "//")
		out.WriteString(beforeComment)
		out.WriteByte('\n')
	}
	return out.String()
}

type errorHandler struct{}

func (errorHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (errorHandler) Handle(context.Context, slog.Record) error {
	return context.Canceled
}

func (e errorHandler) WithAttrs([]slog.Attr) slog.Handler {
	return e
}

func (e errorHandler) WithGroup(string) slog.Handler {
	return e
}
