package internal

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

func TestLogViewSubmissionRedactsRegisteredBlocks(t *testing.T) {
	t.Parallel()

	const secret = "boot_secret123"
	submission := testViewSubmission(map[string]map[string]interactionStateValue{
		claimCodeBlockID: {
			"claim_code_input": {Value: secret},
		},
		feedbackBlockDetails: {
			feedbackActionDetails: {Value: "free-form secret"},
		},
		tunnelInstallBlockSlug: {
			tunnelInstallActionSlug: {Value: "safe-diagnostic-slug"},
			"unexpected_action":     {Value: "unexpected value"},
		},
		"future_secret_block": {
			"future_secret_input": {Value: "future secret"},
		},
	})
	var buf bytes.Buffer
	LogViewSubmission(slog.New(slog.NewJSONHandler(&buf, nil)), submission)

	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Fatalf("view submission log leaked secret: %s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte("free-form secret")) {
		t.Fatalf("view submission log leaked redacted free-form field: %s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte("future secret")) {
		t.Fatalf("view submission log leaked unclassified future field: %s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte("unexpected value")) {
		t.Fatalf("view submission log leaked non-allowlisted sibling action: %s", buf.String())
	}
	fields := decodeInteractionLogLine(t, buf.Bytes())
	if _, ok := fields["trigger_id"]; ok {
		t.Fatalf("view submission log emitted trigger_id: %#v", fields)
	}
	state := fields["state_values"].(map[string]any)
	if got := state[claimCodeBlockID]; got != redactedSubmissionBlockValue(claimCodeBlockID) {
		t.Fatalf("redacted block = %#v, want sentinel", got)
	}
	if got := state[feedbackBlockDetails]; got != redactedSubmissionBlockValue(feedbackBlockDetails) {
		t.Fatalf("free-form block = %#v, want sentinel", got)
	}
	if got := state["future_secret_block"]; got != redactedSubmissionBlockValue("future_secret_block") {
		t.Fatalf("future block = %#v, want sentinel", got)
	}
	diagnostic := state[tunnelInstallBlockSlug].(map[string]any)
	if got := diagnostic[tunnelInstallActionSlug]; got != "safe-diagnostic-slug" {
		t.Fatalf("diagnostic value = %#v, want preserved", got)
	}
	if _, ok := diagnostic["unexpected_action"]; ok {
		t.Fatalf("non-allowlisted sibling action was logged: %#v", diagnostic)
	}
}

func TestViewSubmissionLogValueRedactsRegisteredBlocks(t *testing.T) {
	t.Parallel()

	const secret = "boot_secret123"
	submission := testViewSubmission(map[string]map[string]interactionStateValue{
		claimCodeBlockID: {
			"claim_code_input": {Value: secret},
		},
		tunnelInstallBlockEnvironment: {
			tunnelInstallActionEnvironment: {SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvDocker)}},
		},
	})
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "pointer", value: submission},
		{name: "value", value: *submission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.Info("direct submission log", "submission", tc.value)

			if bytes.Contains(buf.Bytes(), []byte(secret)) {
				t.Fatalf("direct view submission log leaked secret: %s", buf.String())
			}
			fields := decodeInteractionLogLine(t, buf.Bytes())
			submissionFields := fields["submission"].(map[string]any)
			if _, ok := submissionFields["trigger_id"]; ok {
				t.Fatalf("direct view submission log emitted trigger_id: %#v", submissionFields)
			}
			state := submissionFields["state_values"].(map[string]any)
			if got := state[claimCodeBlockID]; got != redactedSubmissionBlockValue(claimCodeBlockID) {
				t.Fatalf("redacted block = %#v, want sentinel", got)
			}
			visible := state[tunnelInstallBlockEnvironment].(map[string]any)
			if got := visible[tunnelInstallActionEnvironment]; got != string(tunnelEnvDocker) {
				t.Fatalf("visible value = %#v, want preserved", got)
			}
		})
	}
}

func TestInteractionPayloadLogValueKeepsBlockActionsAllowlistShape(t *testing.T) {
	t.Parallel()

	payload := &interactionPayload{Type: interactionTypeBlockActions}
	payload.Team.ID = "T_test"
	payload.User.ID = "U_test"
	payload.View.State.Values = map[string]map[string]interactionStateValue{
		tunnelInstallBlockSlug: {
			tunnelInstallActionSlug: {Value: "safe-block-action-slug"},
			"unexpected_action":     {Value: "unexpected action value"},
		},
		"future_secret_block": {
			"future_secret_input": {Value: "future secret"},
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("block action log", "payload", payload)

	for _, leaked := range []string{"future secret", "unexpected action value"} {
		if bytes.Contains(buf.Bytes(), []byte(leaked)) {
			t.Fatalf("block action log leaked %q: %s", leaked, buf.String())
		}
	}
	fields := decodeInteractionLogLine(t, buf.Bytes())
	payloadFields := fields["payload"].(map[string]any)
	state := payloadFields["state_values"].(map[string]any)
	if _, ok := state["future_secret_block"]; ok {
		t.Fatalf("unclassified block_actions state was emitted: %#v", state)
	}
	diagnostic := state[tunnelInstallBlockSlug].(map[string]any)
	if got := diagnostic[tunnelInstallActionSlug]; got != "safe-block-action-slug" {
		t.Fatalf("block action diagnostic value = %#v, want preserved", got)
	}
	if _, ok := diagnostic["unexpected_action"]; ok {
		t.Fatalf("non-allowlisted block action sibling was logged: %#v", diagnostic)
	}
}

func TestInteractionPayloadLogValueNilPointer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("nil block action log", "payload", (*interactionPayload)(nil))

	fields := decodeInteractionLogLine(t, buf.Bytes())
	if got, ok := fields["payload"]; !ok || got != nil {
		t.Fatalf("nil payload log = %#v, want payload null", fields)
	}
}

func TestViewSubmissionStateLogClassificationCoversRegisteredBlocks(t *testing.T) {
	t.Parallel()

	for _, blockID := range viewSubmissionBlockIDs {
		_, allowlisted := viewSubmissionStateLogAllowlist[blockID]
		if !allowlisted && !IsRedactedSubmissionBlock(blockID) {
			t.Fatalf("view submission block %q is neither allowlisted nor redacted", blockID)
		}
	}
}

func TestViewSubmissionStateLogClassificationsAreDisjoint(t *testing.T) {
	t.Parallel()

	for blockID := range viewSubmissionStateLogAllowlist {
		if IsRedactedSubmissionBlock(blockID) {
			t.Fatalf("view submission block %q is both allowlisted and redacted", blockID)
		}
	}
}

func TestViewSubmissionStateLogAllowlistExtendsInteractionAllowlist(t *testing.T) {
	t.Parallel()

	for blockID, actions := range interactionStateLogAllowlist {
		viewActions, ok := viewSubmissionStateLogAllowlist[blockID]
		if !ok {
			t.Fatalf("view submission allowlist missing interaction block %q", blockID)
		}
		for actionID := range actions {
			if _, ok := viewActions[actionID]; !ok {
				t.Fatalf("view submission allowlist missing %q action %q", blockID, actionID)
			}
		}
	}
}

func TestViewSubmissionStateLogRegistryCoversClassifications(t *testing.T) {
	t.Parallel()

	registered := make(map[string]struct{}, len(viewSubmissionBlockIDs))
	for _, blockID := range viewSubmissionBlockIDs {
		registered[blockID] = struct{}{}
	}
	for blockID := range viewSubmissionStateLogAllowlist {
		if _, ok := registered[blockID]; !ok {
			t.Fatalf("view submission allowlist block %q is missing from registry", blockID)
		}
	}
	for blockID := range redactedSubmissionBlockIDs {
		if _, ok := registered[blockID]; !ok {
			t.Fatalf("view submission redacted block %q is missing from registry", blockID)
		}
	}
}

func TestLogViewSubmissionNilGuards(t *testing.T) {
	var buf bytes.Buffer
	LogViewSubmission(slog.New(slog.NewJSONHandler(&buf, nil)), nil)
	if buf.Len() != 0 {
		t.Fatalf("nil submission emitted log: %s", buf.String())
	}

	originalDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(originalDefault)

	LogViewSubmission(nil, nil)
}

func TestInteractionStateValueLogValuePreservesSelectedConversations(t *testing.T) {
	t.Parallel()

	value := interactionStateValue{SelectedConversations: []string{"C_one", "C_two"}}
	got, ok := value.logValue().([]string)
	if !ok {
		t.Fatalf("logValue() = %T, want []string", value.logValue())
	}
	if len(got) != 2 || got[0] != "C_one" || got[1] != "C_two" {
		t.Fatalf("selected conversations = %#v, want copied channel IDs", got)
	}
	got[0] = "C_changed"
	if value.SelectedConversations[0] != "C_one" {
		t.Fatalf("logValue returned backing slice; original = %#v", value.SelectedConversations)
	}
}

func testViewSubmission(values map[string]map[string]interactionStateValue) *ViewSubmission {
	submission := &ViewSubmission{Type: interactionTypeViewSubmission}
	submission.Team.ID = "T_test"
	submission.User.ID = "U_test"
	submission.View.ID = "V_test"
	submission.View.CallbackID = "callback_test"
	submission.View.State.Values = values
	return submission
}

func decodeInteractionLogLine(t *testing.T, line []byte) map[string]any {
	t.Helper()

	var fields map[string]any
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("decode log line: %v\n%s", err, string(line))
	}
	return fields
}
