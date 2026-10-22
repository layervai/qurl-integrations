//go:build clisandbox && cliproduction

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
)

func TestProductionProofCommandEnvironment(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "QURL_API_KEY_FILE", "QURL_DEPLOYMENT", "QURL_CONNECTOR_HUB_HOST", "GH_TOKEN", "AUTOMATION_API_KEY", "HTTPS_PROXY"} {
		t.Setenv(name, "must-not-reach-candidate")
	}
	raw, code := productionCommand(t, os.Args[0], map[string]string{"PROOF_FIXTURE": "success"}, "one-time-fixture-key\n", "-test.run=^TestProductionProofCommandFixture$")
	if code != 0 {
		t.Fatalf("fixture command exit = %d", code)
	}
	var result struct {
		Isolated bool
		InputOK  bool
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.Isolated || !result.InputOK {
		t.Fatal("candidate command inherited standing authority or lost its one-time input")
	}
}

func TestProductionProofPreservesCLIErrorExit(t *testing.T) {
	_, code := productionCommand(t, os.Args[0], map[string]string{"PROOF_FIXTURE": "reject"}, "", "-test.run=^TestProductionProofCommandFixture$")
	if code != 8 {
		t.Fatalf("production HTTP 400 became exit %d, want 8", code)
	}
}

func TestProductionProofCommandFixture(t *testing.T) {
	mode := os.Getenv("PROOF_FIXTURE")
	if mode == "" {
		return
	}
	if mode == "reject" {
		_, _ = fmt.Fprintln(os.Stderr, "Validation Error (HTTP 400)")
		os.Exit(8)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	isolated := true
	for _, entry := range os.Environ() {
		if strings.Contains(entry, "must-not-reach-candidate") {
			isolated = false
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]bool{"Isolated": isolated, "InputOK": string(input) == "one-time-fixture-key\n"}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestProductionProofFailureClassNeverReturnsSensitiveOutput(t *testing.T) {
	hostile := "lv_live_" + strings.Repeat("private", 10) + " https://qurl.link/private-bearer \n-----BEGIN PRIVATE KEY-----\nprivate-material"
	for _, test := range []struct{ message, want string }{
		{consume.MsgCRIDNotFound, "crid_not_found"},
		{consume.MsgCRIDLinkUnavailable, "crid_link_unavailable"},
		{consume.MsgCRIDLinkNoAnswer, "crid_link_no_answer"},
		{consume.MsgAccessBusy, "access_busy"},
		{consume.MsgLinkUnavailable, "download_host_unavailable"},
		{consume.MsgCRIDLinkRateLimited, "crid_link_rate_limited"},
		{consume.MsgCRIDPublisherOffline, "crid_publisher_offline"},
		{consume.MsgCRIDResourceClosed, "crid_resource_closed"},
		{consume.MsgCRIDLinkRequestRejected, "crid_link_request_rejected"},
		{consume.MsgCRIDLinkRefused, "crid_link_refused"},
		{"qURL could not start this Connector's device enrollment because the CLI's local enrollment settings are invalid.", "enrollment_config"},
		{"This Connector's device enrollment did not finish before its safe retry period ended.", "enrollment_unavailable"},
		{"The qURL platform could not verify this Connector during device enrollment.", "enrollment_identity"},
		{"The qURL platform refused this Connector's device enrollment request.", "enrollment_request_rejected"},
		{"The qURL platform and this CLI did not agree on this Connector's device enrollment, so qURL stopped safely.", "enrollment_mismatch"},
		{"qURL could not safely save this Connector's device enrollment state on this machine, so it stopped.", "enrollment_persistence"},
		{"The qURL platform isn't accepting new Connector enrollments here right now.", "enrollment_disabled"},
		{"qurl: platform denied access (errCode=", "platform_denied"},
		{"Access is denied.", "os_access_denied"},
		{"The system cannot find the file specified.", "os_file_missing"},
		{"context deadline exceeded", "native_deadline"},
		{"unrecognized private failure", "unclassified"},
	} {
		if got := productionFailureClass([]byte(hostile + test.message + hostile)); got != test.want {
			t.Fatal("failure classifier returned unexpected or sensitive output")
		}
	}
}

func TestProductionProofCredentialFailureClassIsPrivate(t *testing.T) {
	hostile := "C:\\private-path\\lv_live_" + strings.Repeat("private", 10) + "\n-----BEGIN PRIVATE KEY-----\nprivate-material"
	for _, test := range []struct{ message, want string }{
		{"must name one exact absolute private file", "file_path"},
		{"not one bounded owner-private regular file", "file_metadata"},
		{"not owned by the current user", "file_owner"},
		{"ACL must be protected from inheritance", "file_acl_inheritance"},
		{"DACL grants another principal access", "file_acl_principal"},
		{"unsafe ownership or link authority", "file_authority"},
		{"cannot open QURL_API_KEY_FILE without following links", "file_open"},
		{"changed while opening", "file_changed_on_open"},
		{"changed while reading", "file_changed_on_read"},
		{"exact key bytes plus one LF or CRLF", "file_line_format"},
		{"cannot read exact bounded", "file_read"},
		{"does not look like a qURL API key", "key_shape"},
		{"unknown private failure", "reader_unclassified"},
	} {
		if got := productionCredentialFailureClass(errors.New(hostile+test.message+hostile), ""); got != test.want {
			t.Fatal("credential classifier returned unexpected or sensitive output")
		}
	}
	if productionCredentialFailureClass(nil, auth.SourceEnvironmentFile) != "none" || productionCredentialFailureClass(nil, auth.SourceEnvironment) != "credential_not_file" || productionCredentialFailureClass(auth.ErrNoCredential, "") != "credential_missing" || productionCredentialFailureClass(auth.ErrCredentialConflict, "") != "credential_conflict" {
		t.Fatal("credential source classification changed")
	}
}
