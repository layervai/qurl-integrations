package internal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestResolveChannelTokenPolicy(t *testing.T) {
	tunnel := func(id, slug, status string) client.Resource {
		return client.Resource{ResourceID: id, Slug: slug, Type: client.ResourceTypeTunnel, Status: status}
	}
	url := func(id, alias string) client.Resource {
		return client.Resource{ResourceID: id, Alias: alias, Type: client.ResourceTypeURL}
	}
	for _, tc := range []struct {
		name            string
		binding         string
		allowed         []string
		slugs, aliases  []client.Resource
		wantID, wantVia string
		wantErr         error
		wantCalls       []string
	}{
		{name: "bound alias wins without metadata or API", binding: "bound", wantID: "bound", wantVia: channelTokenAlias},
		{name: "legacy binding refuses before fallback", binding: "https://legacy.example.test", wantErr: errChannelTokenLegacy},
		{name: "empty channel spends no upstream call"},
		{name: "slug wins before resource alias", allowed: []string{"a", "b"}, slugs: []client.Resource{tunnel("a", "token", client.StatusActive)}, aliases: []client.Resource{url("b", "token")}, wantID: "a", wantVia: channelTokenSlug, wantCalls: []string{channelTokenSlug}},
		{name: "wrong type does not make a slug", allowed: []string{"a"}, slugs: []client.Resource{{ResourceID: "a", Slug: "token", Type: client.ResourceTypeURL}}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "wrong slug refuses", allowed: []string{"a"}, slugs: []client.Resource{tunnel("a", "different", client.StatusActive)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "revoked slug refuses", allowed: []string{"a"}, slugs: []client.Resource{tunnel("a", "token", client.StatusRevoked)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel slug is not a match", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel slug permits in channel alias", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive)}, aliases: []client.Resource{url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "only authorized slug counts", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive), tunnel("a", "token", client.StatusActive)}, wantID: "a", wantVia: channelTokenSlug, wantCalls: []string{channelTokenSlug}},
		{name: "ambiguous slugs refuse", allowed: []string{"a", "b"}, slugs: []client.Resource{tunnel("a", "token", client.StatusActive), tunnel("b", "token", client.StatusActive)}, wantErr: errChannelTokenAmbiguous, wantCalls: []string{channelTokenSlug}},
		{name: "URL resource alias", allowed: []string{"a"}, aliases: []client.Resource{url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "legacy URL metadata remains supported", allowed: []string{"a"}, aliases: []client.Resource{{ResourceID: "a", Alias: "token", TargetURL: "https://example.test"}}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "revoked alias refuses", allowed: []string{"a"}, aliases: []client.Resource{{ResourceID: "a", Alias: "token", Type: client.ResourceTypeURL, Status: client.StatusRevoked}}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel alias refuses", allowed: []string{"a"}, aliases: []client.Resource{url("secret", "token")}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "ambiguous URL aliases refuse", allowed: []string{"a", "b"}, aliases: []client.Resource{url("a", "token"), url("b", "token")}, wantErr: errChannelTokenAmbiguous, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "duplicate rows are not ambiguous", allowed: []string{"a"}, aliases: []client.Resource{url("a", "token"), url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			scopeCalls := 0
			got, err := resolveChannelToken("token", channelTokenSource{
				alias: func() (*channelTokenResource, error) {
					if tc.binding == "" {
						return nil, errChannelTokenNotFound
					}
					return &channelTokenResource{ResourceID: tc.binding}, nil
				},
				allowed: func() (map[string]struct{}, error) {
					scopeCalls++
					out := map[string]struct{}{}
					for _, id := range tc.allowed {
						out[id] = struct{}{}
					}
					return out, nil
				},
				candidates: func(via string) ([]client.Resource, error) {
					calls = append(calls, via)
					if via == channelTokenSlug {
						return tc.slugs, nil
					}
					return tc.aliases, nil
				},
			})
			wantErr := tc.wantErr
			if tc.wantID == "" && wantErr == nil {
				wantErr = errChannelTokenNotFound
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, tc.wantErr)
			}
			if tc.wantID == "" {
				if got != nil {
					t.Fatalf("unexpected resolution: %+v", got)
				}
			} else if got == nil || got.ResourceID != tc.wantID || got.Via != tc.wantVia {
				t.Fatalf("resolution=%+v, want id=%s via=%s", got, tc.wantID, tc.wantVia)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("upstream calls=%v want=%v", calls, tc.wantCalls)
			}
			if tc.binding != "" && scopeCalls != 0 {
				t.Fatalf("bound alias read an unnecessary allow-set")
			}
		})
	}
}

func TestResolveChannelTokenFailuresDoNotFallThrough(t *testing.T) {
	cause := errors.New("lookup unavailable")
	for _, stage := range []string{"binding", "scope", channelTokenSlug, channelTokenResourceAlias} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			got, err := resolveChannelToken("token", channelTokenSource{
				alias: func() (*channelTokenResource, error) {
					if stage == "binding" {
						return nil, cause
					}
					return nil, errChannelTokenNotFound
				},
				allowed: func() (map[string]struct{}, error) {
					if stage == "scope" {
						return nil, cause
					}
					return map[string]struct{}{"a": {}}, nil
				},
				candidates: func(via string) ([]client.Resource, error) {
					calls++
					if via == stage {
						return nil, cause
					}
					return []client.Resource{}, nil
				},
			})
			if got != nil || !errors.Is(err, cause) {
				t.Fatalf("lookup failure was hidden: result=%+v err=%v", got, err)
			}
			maxCalls := 0
			if stage == channelTokenSlug {
				maxCalls = 1
			}
			if stage == channelTokenResourceAlias {
				maxCalls = 2
			}
			if calls != maxCalls {
				t.Fatalf("lookup fell through after failure: calls=%d want=%d", calls, maxCalls)
			}
		})
	}
}

func TestAgentResolveUsesSharedLegacyAndResourceAliasPolicy(t *testing.T) {
	t.Run("legacy binding refuses without a client", func(t *testing.T) {
		b, store := newBackendUnderTest(t, false)
		if err := store.BindChannelAlias(context.Background(), "T1", "C1", "legacy", "https://legacy.example.test"); err != nil {
			t.Fatal(err)
		}
		got, err := b.ResolveToken(context.Background(), backendTC(), "$legacy")
		if err != nil || got != legacyAliasBindingMessage("legacy") {
			t.Fatalf("legacy guidance=%q err=%v", got, err)
		}
	})
	t.Run("resource alias works without channel alias binding", func(t *testing.T) {
		b, store := newBackendUnderTest(t, true)
		if err := store.UnbindChannelAlias(context.Background(), "T1", "C1", "oncall"); err != nil {
			t.Fatal(err)
		}
		got, err := b.ResolveToken(context.Background(), backendTC(), "$oncall")
		if err != nil || !strings.Contains(got, "protected resource") || strings.Contains(got, "r_1") {
			t.Fatalf("resource alias result=%q err=%v", got, err)
		}
	})
}
