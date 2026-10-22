//go:build clisandbox

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// TestSandboxInspectionDocReadsEveryInspectKey holds sandboxInspectionDoc to
// the sharing document `qurl inspect -o json` writes for a published local
// service. It does not cover the status document inspect writes for a URL
// resource.
//
// The live journeys decode that document with unknown keys refused, and they
// run only from main. Without this test, a key added to the command passes
// every pull request check and first fails after the merge, in the nightly
// journeys and in the next release gate. This test needs no credentials, so it
// fails in the pull request that adds the key.
//
// It checks that the journey document names each key. It does not check that
// assertHealthySandboxInspection has a rule for it.
func TestSandboxInspectionDocReadsEveryInspectKey(t *testing.T) {
	at := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	// Every field is set, so every key is written, the optional ones too.
	inspection := &output.SharingInspection{
		TargetURL: "http://127.0.0.1:1",
		State: &qurlapi.Sharing{
			ResourceID:      "resource",
			CRID:            "crid",
			DesiredState:    qurlapi.DesiredState("on"),
			ServingEpoch:    1,
			ConnectionState: qurlapi.ConnectionState("serving"),
			CreatedAt:       &at,
			Publisher:       qurlapi.Publisher{Name: "name", Verified: true},
		},
		DaemonState:     "serving",
		LastTransition:  &at,
		FailureCategory: "category",
		FailureCode:     "code",
		RetryAttempt:    1,
		NextRetryAt:     &at,
		TargetHealth:    "healthy",
	}
	// A field added to the command's input has to be set above. Its key is
	// then written, and the decoder below refuses it until the journey
	// document names it.
	requireEveryFieldSet(t, "inspection", reflect.ValueOf(inspection))

	raw := renderSandboxInspection(t, inspection)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document sandboxInspectionDoc
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("the journey document does not read what qurl inspect writes: %v; output %s", err, raw)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("qurl inspect wrote more than one document: %s", raw)
	}
	// And the other way round: a key the journey document names and the
	// command no longer writes is found here, not in a live journey.
	requireEveryFieldSet(t, "document", reflect.ValueOf(&document))

	// The document a live journey reads is the other one: a healthy share
	// from a publisher with no name and no verification, and no optional key.
	// The journey's own check runs on it here, so a key that check requires
	// and the command stops writing for this share fails in the pull request.
	healthy := renderSandboxInspection(t, &output.SharingInspection{
		TargetURL: "http://127.0.0.1:1",
		State: &qurlapi.Sharing{
			ResourceID:      "resource",
			CRID:            "crid",
			DesiredState:    qurlapi.DesiredState("on"),
			ServingEpoch:    1,
			ConnectionState: qurlapi.ConnectionState("serving"),
		},
		DaemonState:    "serving",
		LastTransition: &at,
		TargetHealth:   "healthy",
	})
	assertHealthySandboxInspection(t, healthy, nil, "", "crid", "resource", "on", "serving", 1)
	var unverified sandboxInspectionDoc
	if err := json.Unmarshal(healthy, &unverified); err != nil {
		t.Fatalf("decode the healthy inspect document: %v", err)
	}
	if *unverified.Publisher.Verified || unverified.Publisher.Name != nil || unverified.CreatedAt != nil {
		t.Fatalf("the healthy inspect document is not the unnamed, unverified one: %s", healthy)
	}
}

// renderSandboxInspection returns what `qurl inspect -o json` writes to
// standard output for the inspection. The command writes nothing to standard
// error.
func renderSandboxInspection(t *testing.T, inspection *output.SharingInspection) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	printer := output.New(&output.Streams{Out: &stdout, Err: &stderr}, output.FormatJSON, false, false, false, nil)
	if err := printer.InspectSharing(inspection); err != nil {
		t.Fatalf("render the inspect document: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("qurl inspect -o json wrote to standard error: %s", stderr.String())
	}
	return stdout.Bytes()
}

// requireEveryFieldSet fails for a field that holds its zero value. It walks
// pointers and structs; a time is one value. Any other struct type with
// unexported fields needs the same exception as time.Time.
func requireEveryFieldSet(t *testing.T, name string, value reflect.Value) {
	t.Helper()
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			t.Fatalf("%s is not set", name)
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct || value.Type() == reflect.TypeOf(time.Time{}) {
		if value.IsZero() {
			t.Fatalf("%s is not set", name)
		}
		return
	}
	for index := 0; index < value.NumField(); index++ {
		requireEveryFieldSet(t, name+"."+value.Type().Field(index).Name, value.Field(index))
	}
}
