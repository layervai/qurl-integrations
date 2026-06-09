package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestBuildAgentLLMDarkUnlessKeySet(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantNil bool
	}{
		{name: "unset", key: "", wantNil: true},
		{name: "whitespace only", key: "   ", wantNil: true},
		{name: "present", key: "sk-ant-test", wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", tc.key)
			llm := buildAgentLLM()
			if (llm == nil) != tc.wantNil {
				t.Fatalf("buildAgentLLM() nil=%v, want nil=%v", llm == nil, tc.wantNil)
			}
		})
	}
}

func TestBuildAgentStoreDarkWhenTableUnset(t *testing.T) {
	t.Setenv(slackdata.EnvAgentStateTable, "")
	if store := buildAgentStore(context.Background()); store != nil {
		t.Fatalf("buildAgentStore() = %v, want nil when table unset", store)
	}
}

func TestBuildAgentStoreWiredWhenTableSet(t *testing.T) {
	// LoadDefaultConfig resolves region/credentials lazily, so this stays offline.
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv(slackdata.EnvAgentStateTable, "qurl-agent-state-test")
	store := buildAgentStore(context.Background())
	if store == nil {
		t.Fatal("buildAgentStore() = nil, want a store when table is set")
	}
	if store.TableName != "qurl-agent-state-test" {
		t.Fatalf("store.TableName = %q, want qurl-agent-state-test", store.TableName)
	}
}

func TestReadAgentKillSwitch(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "unset is not disabled", val: "", want: false},
		{name: "true disables", val: "true", want: true},
		{name: "1 disables", val: "1", want: true},
		{name: "false does not disable", val: "false", want: false},
		{name: "0 does not disable", val: "0", want: false},
		// Fail-safe: an unparseable value must DISABLE, never silently enable.
		{name: "garbage fails safe to disabled", val: "disable", want: true},
		{name: "yes fails safe to disabled", val: "yes", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QURL_AGENT_DISABLED", tc.val)
			if got := readAgentKillSwitch(); got != tc.want {
				t.Fatalf("readAgentKillSwitch(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// captureSlog redirects the default slog logger to a buffer for the duration of
// the test and returns a function that yields the captured JSON log lines.
func captureSlog(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("unmarshal log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

func TestLogAgentSurfaceState(t *testing.T) {
	cases := []struct {
		name          string
		llm           bool
		store         bool
		post          bool
		killed        bool
		wantLevel     string
		wantSubstr    string
		wantMissing   string // when set, asserts the "missing" attribute contains it
		wantNoMissing bool
	}{
		{name: "live", llm: true, store: true, post: true, wantLevel: "INFO", wantSubstr: "LIVE", wantNoMissing: true},
		{name: "killed", llm: true, store: true, post: true, killed: true, wantLevel: "WARN", wantSubstr: "kill switch", wantNoMissing: true},
		{name: "fully dark", post: true, wantLevel: "INFO", wantSubstr: "no agent seams", wantNoMissing: true},
		{name: "partial: store missing", llm: true, post: true, wantLevel: "WARN", wantSubstr: "partially configured", wantMissing: slackdata.EnvAgentStateTable},
		{name: "partial: llm missing", store: true, post: true, wantLevel: "WARN", wantSubstr: "partially configured", wantMissing: "ANTHROPIC_API_KEY"},
		// PostMessage is wired unconditionally today, but the LIVE claim must still
		// require it: LLM+Store set with PostMessage nil is partial, not LIVE.
		{name: "partial: postmessage missing", llm: true, store: true, wantLevel: "WARN", wantSubstr: "partially configured", wantMissing: "PostMessage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			// confirmFlag stays false here, so only the read-only line is emitted.
			logAgentSurfaceState(agentSurfaceState{llmWired: tc.llm, storeWired: tc.store, postWired: tc.post, killed: tc.killed})
			records := logs()
			if len(records) != 1 {
				t.Fatalf("got %d log records, want 1: %v", len(records), records)
			}
			rec := records[0]
			if rec["level"] != tc.wantLevel {
				t.Fatalf("level = %v, want %v", rec["level"], tc.wantLevel)
			}
			if msg, _ := rec["msg"].(string); !strings.Contains(msg, tc.wantSubstr) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.wantSubstr)
			}
			missing, hasMissing := rec["missing"].(string)
			if tc.wantNoMissing && hasMissing {
				t.Fatalf("unexpected missing attribute %q", missing)
			}
			if tc.wantMissing != "" && !strings.Contains(missing, tc.wantMissing) {
				t.Fatalf("missing = %q, want substring %q", missing, tc.wantMissing)
			}
		})
	}
}

func TestReadAgentConfirmEnabled(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "unset is off", val: "", want: false},
		{name: "true enables", val: "true", want: true},
		{name: "1 enables", val: "1", want: true},
		{name: "false stays off", val: "false", want: false},
		{name: "0 stays off", val: "0", want: false},
		// Fail-safe: an unparseable value must stay OFF, never silently enable mutations.
		{name: "garbage fails safe to off", val: "enable", want: false},
		{name: "yes fails safe to off", val: "yes", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QURL_AGENT_CONFIRM_ENABLED", tc.val)
			if got := readAgentConfirmEnabled(); got != tc.want {
				t.Fatalf("readAgentConfirmEnabled(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestLogAgentSurfaceState_ConfirmMode(t *testing.T) {
	// The confirm/mutation line must key on the EFFECTIVE predicate
	// (Handler.agentConfirmEnabled), never the raw flag: a flag set while the surface
	// is dark must read "set but DARK", not "LIVE".
	wired := agentSurfaceState{llmWired: true, storeWired: true, postWired: true, blocksWired: true}
	cases := []struct {
		name        string
		state       agentSurfaceState
		wantConfirm string // substring expected in some emitted record ("" → no confirm line)
	}{
		{name: "flag off → no confirm line", state: wired, wantConfirm: ""},
		{name: "confirm live", state: with(wired, func(s *agentSurfaceState) { s.confirmFlag = true }), wantConfirm: "CONFIRM (mutation execution) is LIVE"},
		{name: "flag set but blocks unwired → dark", state: with(wired, func(s *agentSurfaceState) { s.confirmFlag = true; s.blocksWired = false }), wantConfirm: "confirm mode is DARK"},
		{name: "flag set but read-only dark → dark", state: agentSurfaceState{storeWired: true, postWired: true, blocksWired: true, confirmFlag: true}, wantConfirm: "confirm mode is DARK"},
		{name: "flag set but killed → dark", state: with(wired, func(s *agentSurfaceState) { s.confirmFlag = true; s.killed = true }), wantConfirm: "confirm mode is DARK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			logAgentSurfaceState(tc.state)
			var found bool
			for _, rec := range logs() {
				if msg, _ := rec["msg"].(string); tc.wantConfirm != "" && strings.Contains(msg, tc.wantConfirm) {
					found = true
				}
				// A flag-off state must never emit ANY confirm line (LIVE or DARK).
				if tc.wantConfirm == "" {
					if msg, _ := rec["msg"].(string); strings.Contains(msg, "CONFIRM") || strings.Contains(msg, "QURL_AGENT_CONFIRM_ENABLED") {
						t.Fatalf("flag off should emit no confirm line; got %q", msg)
					}
				}
			}
			if tc.wantConfirm != "" && !found {
				t.Fatalf("expected a confirm record containing %q; records=%v", tc.wantConfirm, logs())
			}
		})
	}
}

// with returns a copy of s mutated by fn — keeps the confirm-mode cases terse.
func with(s agentSurfaceState, fn func(*agentSurfaceState)) agentSurfaceState {
	fn(&s)
	return s
}
