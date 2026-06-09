package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeToken(t *testing.T) {
	cases := map[string]string{
		testSlugSigil: testSlug,
		" $x ":        "x",
		"plain":       "plain",
		"":            "",
		"$":           "",
		"  $on-call":  "on-call",
	}
	for in, want := range cases {
		if got := normalizeToken(in); got != want {
			t.Errorf("normalizeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustInput(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestParseProposal(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		input      map[string]any
		wantOK     bool
		wantErr    bool
		wantAction ActionKind
		wantGated  bool
		check      func(t *testing.T, p *Proposal)
	}{
		{
			name:       "get strips sigil and keeps reason",
			tool:       toolProposeGet,
			input:      map[string]any{fieldToken: testSlugSigil, fieldReason: "incident 5"},
			wantOK:     true,
			wantAction: ActionGet,
			check: func(t *testing.T, p *Proposal) {
				if p.Token != testSlug || p.Reason != "incident 5" {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:    "get requires token",
			tool:    toolProposeGet,
			input:   map[string]any{},
			wantOK:  true,
			wantErr: true,
		},
		{
			name:       "revoke is admin gated",
			tool:       toolProposeRevoke,
			input:      map[string]any{fieldToken: "analytics"},
			wantOK:     true,
			wantAction: ActionRevoke,
			wantGated:  true,
		},
		{
			name:       "set_alias needs alias and target",
			tool:       toolProposeSetAlias,
			input:      map[string]any{fieldAlias: testChannel, fieldTarget: testSlugSigil},
			wantOK:     true,
			wantAction: ActionSetAlias,
			wantGated:  true,
			check: func(t *testing.T, p *Proposal) {
				if p.Alias != testChannel || p.Target != testSlug {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:    "set_alias missing target errors",
			tool:    toolProposeSetAlias,
			input:   map[string]any{fieldAlias: testChannel},
			wantOK:  true,
			wantErr: true,
		},
		{
			name:       "unset_alias",
			tool:       toolProposeUnsetAlias,
			input:      map[string]any{fieldAlias: "$" + testChannel},
			wantOK:     true,
			wantAction: ActionUnsetAlias,
			wantGated:  true,
			check: func(t *testing.T, p *Proposal) {
				if p.Alias != testChannel {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:       "protect_connector tolerates missing fields",
			tool:       toolProposeProtectConnector,
			input:      map[string]any{fieldAlias: testChannel},
			wantOK:     true,
			wantAction: ActionProtectConnector,
			wantGated:  true,
			check: func(t *testing.T, p *Proposal) {
				if p.Alias != testChannel || !strings.Contains(p.Summary, testChannel) {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:       "protect_url requires url",
			tool:       toolProposeProtectURL,
			input:      map[string]any{fieldAlias: "dash"},
			wantOK:     true,
			wantErr:    true,
			wantAction: ActionProtectURL,
		},
		{
			// alias is required: exposing a URL must bind a channel alias, so an
			// alias-less proposal (whose confirm card could only fail on Approve) is
			// rejected at the propose layer rather than surfaced as a dead-end card.
			name:       "protect_url requires alias",
			tool:       toolProposeProtectURL,
			input:      map[string]any{fieldURL: "https://staging.example.com"},
			wantOK:     true,
			wantErr:    true,
			wantAction: ActionProtectURL,
		},
		{
			name:       "protect_url ok",
			tool:       toolProposeProtectURL,
			input:      map[string]any{fieldURL: "https://staging.example.com", fieldAlias: "$dash"},
			wantOK:     true,
			wantAction: ActionProtectURL,
			wantGated:  true,
			check: func(t *testing.T, p *Proposal) {
				if p.URL != "https://staging.example.com" || p.Alias != "dash" {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:   "read tool is not a proposal",
			tool:   toolListResources,
			input:  map[string]any{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := ToolCall{ID: "tu", Name: tt.tool, Input: mustInput(t, tt.input)}
			p, ok, err := parseProposal(call)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				return
			}
			if p.Action != tt.wantAction {
				t.Fatalf("action = %q, want %q", p.Action, tt.wantAction)
			}
			if p.AdminGated != tt.wantGated {
				t.Fatalf("adminGated = %v, want %v", p.AdminGated, tt.wantGated)
			}
			if p.Summary == "" {
				t.Errorf("proposal should always carry a confirm-card summary")
			}
			if tt.check != nil {
				tt.check(t, p)
			}
		})
	}
}

func TestDecodeFields_CoercesScalars(t *testing.T) {
	// A model may emit a number/bool for a string-schema field; the whole
	// object must not be rejected.
	raw := mustInput(t, map[string]any{
		"alias": "oncall",
		"port":  8080, // number, not string
		"flag":  true, // bool
		"ratio": 1.5,  // non-integer number
		"empty": nil,  // null
	})
	got, err := decodeFields(raw)
	if err != nil {
		t.Fatalf("decodeFields: %v", err)
	}
	want := map[string]string{"alias": "oncall", "port": "8080", "flag": "true", "ratio": "1.5", "empty": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseProposal_NumericPortSurvives(t *testing.T) {
	// Regression: propose_protect_connector with a numeric port must not lose
	// the other fields.
	call := ToolCall{ID: "tu", Name: toolProposeProtectConnector, Input: mustInput(t, map[string]any{
		fieldAlias: "oncall", fieldEnv: "docker", fieldPort: 8080,
	})}
	p, ok, err := parseProposal(call)
	if !ok || err != nil || p == nil {
		t.Fatalf("ok=%v err=%v p=%v", ok, err, p)
	}
	if p.Alias != "oncall" || p.Env != "docker" || p.Port != "8080" {
		t.Fatalf("numeric port lost sibling fields: %+v", p)
	}
}

func TestToolSpecs_CoverEveryToolWithSchemas(t *testing.T) {
	specs := toolSpecs()
	want := []string{
		toolListResources, toolListAliases, toolResolveToken, toolGetQuota,
		toolProposeGet, toolProposeRevoke, toolProposeSetAlias, toolProposeUnsetAlias,
		toolProposeProtectConnector, toolProposeProtectURL,
	}
	got := map[string]ToolSpec{}
	for _, s := range specs {
		got[s.Name] = s
	}
	for _, name := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("missing tool spec %q", name)
			continue
		}
		if s.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
		// Required keys must exist as schema properties.
		for _, req := range s.Required {
			if _, ok := s.Schema[req]; !ok {
				t.Errorf("tool %q requires %q but has no such property", name, req)
			}
		}
	}
}
