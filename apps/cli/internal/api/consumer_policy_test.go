package qurlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestPublishConsumerPolicyRequiresConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "confirmed"}[confirmed], func(t *testing.T) {
			srv := apitest.NewServer(t)
			policy := &qurl.ConsumerAccessPolicy{Mode: "invite_only", Version: 1}
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
				var body publishRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ConsumerAccess == nil || *body.ConsumerAccess != *policy || body.Private == nil || !*body.Private || body.AccessRequests {
					t.Fatalf("unsafe create: %+v", body)
				}
				row := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true}
				if confirmed {
					row["consumer_access"] = policy
				}
				apitest.WriteEnvelope(t, w, http.StatusCreated, row, nil)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{ConsumerAccess: policy, KeepExistingPublic: true})
			if confirmed {
				if err != nil || result.ConsumerAccess == nil || *result.ConsumerAccess != *policy {
					t.Fatalf("result=%+v error=%v", result, err)
				}
			} else if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("unconfirmed policy error=%v", err)
			}
			if len(sentRequests(srv)) != 1 {
				t.Fatalf("unexpected fallback: %v", sentRequests(srv))
			}
		})
	}
}

func TestPublishConsumerPolicyRejectsMixedAccessBeforeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, opts := range []PublishOptions{
		{Public: true}, {AllowRequests: true}, {AllowedDeviceKeys: []string{"key"}},
	} {
		opts.ConsumerAccess = &qurl.ConsumerAccessPolicy{Mode: "review", Version: 1}
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, opts)
		if !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Fatalf("mixed policy error=%v", err)
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("invalid policy reached network")
	}
}

func TestConsumerPolicyHasNoPublicConflictFallback(t *testing.T) {
	opts := &PublishOptions{KeepExistingPublic: true, ConsumerAccess: &qurl.ConsumerAccessPolicy{Mode: "review", Version: 1}}
	if mayKeepExistingPublic(opts, ErrPublishAccessConflict) {
		t.Fatal("private policy can fall back to public")
	}
	for _, policy := range []*qurl.ConsumerAccessPolicy{
		{Mode: "review", Version: 2}, {Mode: "unknown", Version: 1},
		{Mode: "contract", Version: 1}, {Mode: "contract", Version: 1, ContractID: strings.Repeat("x", 129)},
		{Mode: "review", Version: 1, ContractID: "contract"},
	} {
		if validateConsumerPolicy(policy) == nil {
			t.Fatalf("accepted invalid policy %+v", policy)
		}
	}
}

func TestPublishKeepsLaterPolicyOnlyForConfirmedExistingResource(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			srv := apitest.NewServer(t)
			requested := &qurl.ConsumerAccessPolicy{Mode: "invite_only", Version: 1}
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
				var body publishRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ConsumerAccess == nil || body.ConsumerAccess.Version != 1 {
					t.Fatal("creation request changed policy revision")
				}
				row := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": qurl.ConsumerAccessPolicy{Mode: "invite_only", Version: 3}}
				apitest.WriteEnvelope(t, w, http.StatusCreated, row, map[string]any{"found_existing": existing})
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{ConsumerAccess: requested})
			if existing {
				if err != nil || result.ConsumerAccess.Version != 3 {
					t.Fatalf("existing policy failed: %v", err)
				}
			} else if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("new resource accepted wrong revision: %v", err)
			}
			if len(sentRequests(srv)) != 1 {
				t.Fatal("publish attempted a second mutation")
			}
		})
	}
}
