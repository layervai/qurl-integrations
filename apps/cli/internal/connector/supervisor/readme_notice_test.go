package supervisor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// readmeNoticeMsg pulls the msg="…" out of the README's reconnect_retrying
// sample. The sample is a single line, so a non-greedy match to the closing
// quote is exact.
var readmeNoticeMsg = regexp.MustCompile(`msg="([^"]*)"[^\n]*event=reconnect_retrying`)

// TestREADMEQuotesTheActualReconnectNotice pins the operator-facing sample in
// apps/cli/README.md to the string refresher.go actually logs.
//
// This guards a drift class that bit this change twice: the notice wording was
// revised, the README kept the old sentence, and the stale sample then told
// operators to grep for a string the binary can never emit — worse than no
// sample, because it reads as authoritative. The behavioral tests in
// reconnect_test.go pin what the sentence may not CLAIM; this pins that the
// documented copy of it is the same sentence.
//
// Deliberately an exact substring check against the source rather than a
// shared constant: the log call site should stay readable as a literal, and
// the failure mode this catches is precisely the two copies diverging.
func TestREADMEQuotesTheActualReconnectNotice(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	match := readmeNoticeMsg.FindSubmatch(readme)
	if match == nil {
		t.Fatal("README has no event=reconnect_retrying sample with a msg=\"…\" field; if the sample was removed, remove this guard with it")
	}
	documented := string(match[1])

	source, err := os.ReadFile("refresher.go")
	if err != nil {
		t.Fatalf("read refresher.go: %v", err)
	}
	if !strings.Contains(string(source), `"`+documented+`"`) {
		t.Errorf("README documents a reconnect_retrying message refresher.go never logs:\n  README: %q\nUpdate the README sample and the log call together.", documented)
	}
}
