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

func TestUnconfirmedConsumerPolicyExplainsRecovery(t *testing.T) {
	requested := &qurl.ConsumerAccessPolicy{Mode: "review", Version: 1}
	for _, existing := range []bool{false, true} {
		published := &Published{FoundExisting: &existing, ConsumerAccess: &qurl.ConsumerAccessPolicy{Mode: "invite_only", Version: 1}}
		err := confirmPublishConsumerPolicy(requested, published)
		wantError := qurl.ErrInvalidAPIResponse
		if existing {
			wantError = ErrPublishAccessConflict
		}
		if !errors.Is(err, wantError) {
			t.Fatalf("error=%v", err)
		}
		hint := "qurl list"
		if existing {
			hint = "qurl access configure"
		}
		if !strings.Contains(err.Error(), hint) {
			t.Fatalf("missing recovery hint: %v", err)
		}
	}
}

func TestExistingConsumerPolicyMismatchIsAConflict(t *testing.T) {
	existing := true
	requested := &qurl.ConsumerAccessPolicy{Mode: "contract", Version: 1, ContractID: "expected-contract"}
	actual := &Published{FoundExisting: &existing, ConsumerAccess: &qurl.ConsumerAccessPolicy{Mode: "contract", Version: 1, ContractID: "different-contract"}}
	err := confirmPublishConsumerPolicy(requested, actual)
	if !errors.Is(err, ErrPublishAccessConflict) || errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("existing policy mismatch = %v", err)
	}
}

func TestConsumerContractRejectsPadding(t *testing.T) {
	for _, contract := range []string{" acme", "acme ", "\tacme", "acme\n"} {
		if ValidateConsumerPolicyFields("contract", contract) == nil {
			t.Fatalf("accepted %q", contract)
		}
	}
	if err := ValidateConsumerPolicyFields("contract", "acme"); err != nil {
		t.Fatal(err)
	}
}

// TestConsumerManagementMapsAPIErrors keeps qurl access on the CLI's exit
// codes: without mapError a 409 or 503 from these routes exits 1, and the
// access help and agent instructions tell callers to act on 7 and 11.
func TestConsumerManagementMapsAPIErrors(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusConflict, "consumer_access_conflict", "Consumer access changed", "read its current version and retry")
	})
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/invitations", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "unavailable", "Unavailable", "try again")
	})
	owner := newTestClient(t, srv, nil).ConsumerManagement()

	err := owner.UpdateConsumerAccess(t.Context(), srv.Key.CRID, 1, qurl.ConsumerAccessPolicy{Mode: "review", Version: 2})
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.StatusCode != http.StatusConflict {
		t.Fatalf("configure error = %v, want *Error with status 409", err)
	}
	_, err = owner.InviteConsumer(t.Context(), srv.Key.CRID, "ana@example.com")
	var unavailable *Error
	if !errors.As(err, &unavailable) || unavailable.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("invite error = %v, want *Error with status 503", err)
	}
}

// TestEveryConsumerManagementMethodMapsAPIErrors answers each owner route
// with a 409 problem. A method that returns the SDK error without mapError
// fails here, which the compiler cannot catch.
func TestEveryConsumerManagementMethodMapsAPIErrors(t *testing.T) {
	srv := apitest.NewServer(t)
	owner := newTestClient(t, srv, nil).ConsumerManagement()
	crid := srv.Key.CRID
	id := strings.Repeat("a", 64)
	base := "/v1/resources/" + crid
	conflict := func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusConflict, "conflict", "Conflict", "read the current version")
	}
	for _, route := range [][2]string{
		{http.MethodPost, base + "/invitations"},
		{http.MethodGet, base + "/invitations"},
		{http.MethodPost, base + "/invitations/" + id + "/resend"},
		{http.MethodDelete, base + "/invitations/" + id},
		{http.MethodGet, base + "/consumer-grants"},
		{http.MethodPost, base + "/consumer-grants/" + id + "/approve"},
		{http.MethodPost, base + "/consumer-grants/" + id + "/deny"},
		{http.MethodDelete, base + "/consumer-grants/" + id},
		{http.MethodPatch, base},
	} {
		srv.Script(route[0], route[1], conflict)
	}
	calls := map[string]func() error{
		"InviteConsumer": func() error { _, err := owner.InviteConsumer(t.Context(), crid, "ana@example.com"); return err },
		"ListConsumerInvitations": func() error {
			_, err := owner.ListConsumerInvitations(t.Context(), crid, "")
			return err
		},
		"ResendConsumerInvitation": func() error {
			_, err := owner.ResendConsumerInvitation(t.Context(), crid, id, 1)
			return err
		},
		"RevokeConsumerInvitation": func() error {
			_, err := owner.RevokeConsumerInvitation(t.Context(), crid, id, 1)
			return err
		},
		"ListConsumerGrants": func() error { _, err := owner.ListConsumerGrants(t.Context(), crid, ""); return err },
		"ApproveConsumerGrant": func() error {
			_, err := owner.ApproveConsumerGrant(t.Context(), crid, id, qurl.ConsumerGrantApproval{ExpectedVersion: 1})
			return err
		},
		"DenyConsumerGrant":   func() error { _, err := owner.DenyConsumerGrant(t.Context(), crid, id, 1); return err },
		"RevokeConsumerGrant": func() error { _, err := owner.RevokeConsumerGrant(t.Context(), crid, id, 1); return err },
		"UpdateConsumerAccess": func() error {
			return owner.UpdateConsumerAccess(t.Context(), crid, 1, qurl.ConsumerAccessPolicy{Mode: "review", Version: 2})
		},
	}
	if len(calls) != 9 {
		t.Fatalf("calls %d methods, want all 9 of ConsumerManagementClient", len(calls))
	}
	for name, call := range calls {
		var apiErr *Error
		if err := call(); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
			t.Errorf("%s error = %v (%T), want *Error with status 409", name, err, err)
		}
	}
}
