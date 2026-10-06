package qurlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Fixture values of the access-request tests. No person or device has them.
const (
	testRequestCode  = "482913"
	testOtherCode    = "175306"
	testDeviceID     = "abcd-efgh-2345-mnop"
	testOtherDevice  = "qrst-uvwx-yz67-abcd"
	testRequester    = "Ana Lopez"
	testOtherPerson  = "Sam Okafor"
	unknownTestCRID  = "qaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	unsupportedStart = "this service does not offer access requests yet"
)

// requestBodies returns the bodies of the requests the mock received for one
// method and path.
func requestBodies(srv *apitest.Server, method, path string) []string {
	var bodies []string
	for _, request := range srv.Requests() {
		if request.Method == method && request.Path == path {
			bodies = append(bodies, string(request.Body))
		}
	}
	return bodies
}

// requestLines returns "METHOD path" for every request the mock received.
func requestLines(srv *apitest.Server) []string {
	requests := srv.Requests()
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return lines
}

// wantUnsupported fails the test unless err is the unsupported error with the
// message every such error starts with.
func wantUnsupported(t *testing.T, err error) {
	t.Helper()
	var shown interface{ UserMessage() string }
	if !errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &shown) || !strings.HasPrefix(shown.UserMessage(), unsupportedStart) {
		t.Fatalf("error = %v, want %q", err, unsupportedStart)
	}
	var problem *Error
	if errors.As(err, &problem) {
		t.Fatalf("the unsupported error carries a service problem, which would give it that problem's exit code: %v", err)
	}
}

func TestValidRequestCodeAndDeviceID(t *testing.T) {
	for code, want := range map[string]bool{
		"482913": true, "000000": true,
		"": false, "48291": false, "4829133": false, "482 913": false, "48291a": false, "４８２９１３": false,
	} {
		if got := ValidRequestCode(code); got != want {
			t.Errorf("ValidRequestCode(%q) = %t, want %t", code, got, want)
		}
	}
	for id, want := range map[string]bool{
		testDeviceID: true, "aaaa-aaaa-aaaa-aaaa": true, "2345-6722-3344-5566": true,
		"": false, "abcdefgh2345mnop": false, "ABCD-EFGH-2345-MNOP": false, "abcd-efgh-2345-mno": false,
		"abcd-efgh-2345-mnop-": false, "abcd-efgh-1890-mnop": false, "abcd_efgh_2345_mnop": false, "abcd-efgh-2345-mno\n": false,
	} {
		if got := ValidDeviceID(id); got != want {
			t.Errorf("ValidDeviceID(%q) = %t, want %t", id, got, want)
		}
	}
}

// TestPublishSendsAccessRequestsOnlyWhenAsked pins the create request: the
// access_requests member is true when the publisher asked for it and absent
// otherwise, so a service from before access requests is never sent a member
// it does not know.
func TestPublishSendsAccessRequestsOnlyWhenAsked(t *testing.T) {
	for _, connectorID := range []string{"", "requests-connector"} {
		for _, asked := range []bool{false, true} {
			t.Run(fmt.Sprintf("connector=%q/asked=%t", connectorID, asked), func(t *testing.T) {
				srv := apitest.NewServer(t)
				target := "https://example.com/data"
				if connectorID != "" {
					target = ""
				}
				result, err := newTestClient(t, srv, nil).Publish(t.Context(), target, PublishOptions{AllowRequests: asked, ConnectorID: connectorID})
				if err != nil {
					t.Fatal(err)
				}
				var sent map[string]json.RawMessage
				if err := json.Unmarshal(srv.Requests()[0].Body, &sent); err != nil {
					t.Fatal(err)
				}
				stated, present := sent["access_requests"]
				if present != asked || (asked && string(stated) != "true") {
					t.Fatalf("create request access_requests = %q (present %t), asked %t", stated, present, asked)
				}
				if result.AccessRequests == nil || *result.AccessRequests != asked {
					t.Fatalf("result access requests = %v, want %t", result.AccessRequests, asked)
				}
			})
		}
	}
}

// TestPublishRequiresTheAnswerToConfirmAccessRequests pins the check on the
// create answer when access requests were asked for. A row without the member
// is a service that does not offer them. A row that says they are off is a
// service that did not turn them on. Neither returns a result, so no caller
// can print a CRID as if the publisher got what they asked for.
func TestPublishRequiresTheAnswerToConfirmAccessRequests(t *testing.T) {
	for _, test := range []struct {
		name     string
		answered any
		check    func(*testing.T, error)
	}{
		{name: "confirmed", answered: true},
		{name: "no member", check: func(t *testing.T, err error) {
			wantUnsupported(t, err)
			if !strings.Contains(err.Error(), "published as a private resource without them") || !strings.Contains(err.Error(), "without --allow-requests") {
				t.Fatalf("the message does not say what was made and what to do: %v", err)
			}
		}},
		{name: "off", answered: false, check: func(t *testing.T, err error) {
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgAccessRequestsCreateUnconfirmed {
				t.Fatalf("error = %v, want the not-turned-on message", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true}
				if test.answered != nil {
					data["access_requests"] = test.answered
				}
				apitest.WriteEnvelope(t, w, http.StatusCreated, data, nil)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
			if test.check == nil {
				if err != nil || result.AccessRequests == nil || !*result.AccessRequests {
					t.Fatalf("confirmed publish = %+v, %v", result, err)
				}
				return
			}
			if result != nil {
				t.Fatalf("an unconfirmed publish returned %+v", result)
			}
			test.check(t, err)
		})
	}

	// The same through the mock of a service from before access requests.
	srv := apitest.NewServer(t)
	srv.PlayNoAccessRequests()
	_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
	wantUnsupported(t, err)
	// Without the flag that service answers as it always did.
	if result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{}); err != nil || result.AccessRequests != nil {
		t.Fatalf("a plain publish against that service = %+v, %v", result, err)
	}
}

// TestPublishConflictWithAccessRequestsNamesNoCause pins the reading of the
// older refusal when the request turned access requests on: the setting may
// be what differs, so the error names neither privacy nor anything else. The
// privacy-mismatch code still means privacy.
func TestPublishConflictWithAccessRequestsNamesNoCause(t *testing.T) {
	for _, test := range []struct {
		code, detail string
		want         ExistingAccess
	}{
		{code: "invalid_input", detail: apitest.LegacyAccessSettingsDetail, want: ExistingAccessUnknown},
		{code: apitest.CodePrivacyMismatch, detail: "d", want: ExistingAccessPublic},
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, test.code, "Refused", test.detail)
		})
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		var conflict *PublishAccessConflictError
		if !errors.As(err, &conflict) || conflict.Existing != test.want {
			t.Fatalf("code %q: error = %v, want existing access %d", test.code, err, test.want)
		}
	}
	// The mock of the service itself: a create that finds the resource with
	// access requests off is refused, not answered with them still off.
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
	if !errors.Is(err, ErrPublishAccessConflict) {
		t.Fatalf("error = %v, want an access conflict", err)
	}
}

// TestSetAccessRequests pins the change of the setting: one PATCH that
// carries only access_requests, and an answer that must confirm it.
func TestSetAccessRequests(t *testing.T) {
	for _, on := range []bool{true, false} {
		srv := apitest.NewServer(t)
		srv.SetAccessRequests(!on)
		srv.AddApprovedPerson(testDeviceID, testRequester)
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, on)
		if err != nil || resource.AccessRequests == nil || *resource.AccessRequests != on || len(resource.AllowedPasskeys) != 1 {
			t.Fatalf("SetAccessRequests(%t) = %+v, %v", on, resource, err)
		}
		want := fmt.Sprintf(`{"access_requests":%t}`, on)
		if bodies := requestBodies(srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID); len(bodies) != 1 || bodies[0] != want || len(srv.Requests()) != 1 {
			t.Fatalf("requests = %v, want one PATCH with body %s", requestLines(srv), want)
		}
	}

	t.Run("older service", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayNoAccessRequests()
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		wantUnsupported(t, err)
		if resource != nil || !strings.Contains(err.Error(), "does not show the setting") {
			t.Fatalf("an ignored change returned %+v, %v", resource, err)
		}
	})
	t.Run("the answer has the other setting", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "type": "url", "status": "active", "access_requests": false,
			}, nil)
		})
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgAccessRequestsSettingUnconfirmed {
			t.Fatalf("an unconfirmed change returned %+v, %v", resource, err)
		}
	})
	t.Run("a public resource", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(false)
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		var problem *Error
		if resource != nil || !errors.As(err, &problem) || problem.StatusCode != http.StatusBadRequest || !strings.Contains(problem.Detail, "private resource") {
			t.Fatalf("turning requests on for a public resource returned %+v, %v", resource, err)
		}
	})
}

// TestAccessRequestsListings pins the two listings: every field of a row, and
// the resource each row is for.
func TestAccessRequestsListings(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	srv.AddAccessRequest(testOtherCode, "", testOtherDevice)
	client := newTestClient(t, srv, nil)

	for _, id := range []string{"", srv.Key.CRID} {
		requests, err := client.AccessRequests(t.Context(), id)
		if err != nil || len(requests) != 2 {
			t.Fatalf("AccessRequests(%q) = %+v, %v", id, requests, err)
		}
		first, second := requests[0], requests[1]
		if first.Code != testRequestCode || first.Name != testRequester || first.DeviceID != testDeviceID ||
			second.Code != testOtherCode || second.Name != "" || second.DeviceID != testOtherDevice {
			t.Fatalf("AccessRequests(%q) rows = %+v", id, requests)
		}
		if first.RequestedAt == nil || first.ExpiresAt == nil || !first.ExpiresAt.After(*first.RequestedAt) {
			t.Fatalf("AccessRequests(%q) lost the request times: %+v", id, first)
		}
		if first.CRID != srv.Key.CRID || second.CRID != srv.Key.CRID {
			t.Fatalf("AccessRequests(%q) rows do not name the resource: %+v", id, requests)
		}
	}
	want := []string{"GET /v1/access-requests", "GET /v1/resources/" + srv.Key.CRID + "/access-requests"}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// TestAccessRequestsListingRefusesRowsOutsideTheContract pins that a row
// whose code or device id is not one, and a row of the all-resources listing
// that does not name a real resource, fail the listing: a code is what a
// publisher approves, and it is never shown in a form that could not be one.
func TestAccessRequestsListingRefusesRowsOutsideTheContract(t *testing.T) {
	for name, row := range map[string]map[string]any{
		"code with a letter":  {"request_code": "48291a", "device_id": testDeviceID},
		"code too long":       {"request_code": "4829133", "device_id": testDeviceID},
		"no code":             {"device_id": testDeviceID},
		"device id malformed": {"request_code": testRequestCode, "device_id": "ABCD-EFGH-2345-MNOP"},
		"device id escape":    {"request_code": testRequestCode, "device_id": "abcd-efgh-2345-mn\x1b["},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/access-requests", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{row}, nil)
			})
			requests, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), srv.Key.CRID)
			if requests != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("AccessRequests = %+v, %v; want an invalid-response error", requests, err)
			}
		})
	}
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/access-requests", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{"request_code": testRequestCode, "device_id": testDeviceID, "crid": "not-a-crid", "resource_id": srv.Key.ResourceID}}, nil)
	})
	if requests, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), ""); requests != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("a row for no real resource was accepted: %+v, %v", requests, err)
	}
}

// TestAccessRequestsOnAServiceWithoutThem pins what a 404 from the listings
// means. For all resources it can only be a service without the route. For
// one resource the client asks whether the resource exists: when it does, the
// route is what is missing; when it does not, the answer is the plain
// not-found of that resource and never "this service does not offer".
func TestAccessRequestsOnAServiceWithoutThem(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.PlayNoAccessRequests()
	client := newTestClient(t, srv, nil)
	_, err := client.AccessRequests(t.Context(), "")
	wantUnsupported(t, err)
	_, err = client.AccessRequests(t.Context(), srv.Key.CRID)
	wantUnsupported(t, err)

	for _, older := range []bool{false, true} {
		srv := apitest.NewServer(t)
		if older {
			srv.PlayNoAccessRequests()
		}
		_, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), unknownTestCRID)
		var problem *Error
		if errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound {
			t.Fatalf("older=%t: an unknown resource read as %v, want its plain not-found", older, err)
		}
	}
}

// TestApproveAccessRequest pins the approval: one POST on the route of the
// code that was given, never repeated, and the person the service says now
// has access.
func TestApproveAccessRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	client := newTestClient(t, srv, nil)
	person, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
	if err != nil || person.DeviceID != testDeviceID || person.Name != testRequester || person.ApprovedAt == nil {
		t.Fatalf("ApproveAccessRequest = %+v, %v", person, err)
	}
	want := []string{"POST /v1/resources/" + srv.Key.CRID + "/access-requests/" + testRequestCode + "/approve"}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if body := string(srv.Requests()[0].Body); body != "{}" {
		t.Fatalf("approval body = %q, want an empty JSON object", body)
	}
	// The other request is still pending and the approved one is gone.
	pending, err := client.AccessRequests(t.Context(), srv.Key.CRID)
	if err != nil || len(pending) != 1 || pending[0].Code != testOtherCode {
		t.Fatalf("pending after the approval = %+v, %v", pending, err)
	}
	resource, err := client.Resource(t.Context(), srv.Key.CRID)
	if err != nil || len(resource.AllowedPasskeys) != 1 || resource.AllowedPasskeys[0].DeviceID != testDeviceID {
		t.Fatalf("approved people after the approval = %+v, %v", resource, err)
	}
}

// TestApprovalAndDenialOfACodeThatIsNotPending pins the three meanings of a
// 404 from the routes that name a code, for an approval and for a denial.
func TestApprovalAndDenialOfACodeThatIsNotPending(t *testing.T) {
	calls := map[string]func(Client, string) error{
		"approve": func(client Client, id string) error {
			_, err := client.ApproveAccessRequest(t.Context(), id, testRequestCode)
			return err
		},
		"deny": func(client Client, id string) error {
			return client.DenyAccessRequest(t.Context(), id, testRequestCode)
		},
	}
	for name, call := range calls {
		t.Run(name+"/the code is not pending", func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
			err := call(newTestClient(t, srv, nil), srv.Key.CRID)
			var problem *Error
			var shown interface{ UserMessage() string }
			if !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound || problem.RequestID == "" || !errors.As(err, &shown) {
				t.Fatalf("error = %v, want a not-found with its own message", err)
			}
			for _, want := range []string{"no pending request has the code 482 913 for this resource", "`qurl requests <CRID>`"} {
				if !strings.Contains(shown.UserMessage(), want) {
					t.Fatalf("message %q lacks %q", shown.UserMessage(), want)
				}
			}
			if errors.Is(err, ErrAccessRequestsUnsupported) {
				t.Fatal("a code that is not pending read as a service without access requests")
			}
			// The other request was not touched.
			pending, listErr := newTestClient(t, srv, nil).AccessRequests(t.Context(), srv.Key.CRID)
			if listErr != nil || len(pending) != 1 || pending[0].Code != testOtherCode {
				t.Fatalf("pending = %+v, %v", pending, listErr)
			}
		})
		t.Run(name+"/the service has no access requests", func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.PlayNoAccessRequests()
			wantUnsupported(t, call(newTestClient(t, srv, nil), srv.Key.CRID))
		})
		t.Run(name+"/the resource is unknown", func(t *testing.T) {
			srv := apitest.NewServer(t)
			err := call(newTestClient(t, srv, nil), unknownTestCRID)
			var problem *Error
			var shown interface{ UserMessage() string }
			if errors.Is(err, ErrAccessRequestsUnsupported) || errors.As(err, &shown) || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound {
				t.Fatalf("an unknown resource read as %v, want its plain not-found", err)
			}
		})
		t.Run(name+"/a code that is not six digits is never sent", func(t *testing.T) {
			srv := apitest.NewServer(t)
			for _, code := range []string{"", "48291", "482 913", "48291a", "../482913"} {
				var err error
				if name == "approve" {
					_, err = newTestClient(t, srv, nil).ApproveAccessRequest(t.Context(), srv.Key.CRID, code)
				} else {
					err = newTestClient(t, srv, nil).DenyAccessRequest(t.Context(), srv.Key.CRID, code)
				}
				if !errors.Is(err, qurl.ErrInvalidResourceRequest) {
					t.Errorf("code %q: error = %v", code, err)
				}
			}
			if got := len(srv.Requests()); got != 0 {
				t.Fatalf("a refused code sent %d requests", got)
			}
		})
	}
}

// TestApprovalAnswerMustNameTheDevice pins that an approval answer without a
// device id is not reported as an approval of anyone.
func TestApprovalAnswerMustNameTheDevice(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"no device id":        {"name": testRequester},
		"malformed device id": {"name": testRequester, "device_id": "abcd"},
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/access-requests/"+testRequestCode+"/approve", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
		})
		person, err := newTestClient(t, srv, nil).ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
		if person != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgApprovalUnconfirmed {
			t.Fatalf("%s: ApproveAccessRequest = %+v, %v", name, person, err)
		}
	}
}

// TestDenyAccessRequest pins the denial: one DELETE on the route of the code,
// and no access for anyone.
func TestDenyAccessRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	client := newTestClient(t, srv, nil)
	if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, testRequestCode); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + testRequestCode}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	resource, err := client.Resource(t.Context(), srv.Key.CRID)
	if err != nil || len(resource.AllowedPasskeys) != 0 {
		t.Fatalf("a denial gave access: %+v, %v", resource, err)
	}
	pending, err := client.AccessRequests(t.Context(), srv.Key.CRID)
	if err != nil || len(pending) != 1 || pending[0].Code != testOtherCode {
		t.Fatalf("pending after the denial = %+v, %v", pending, err)
	}
}

// TestRemoveAllowedPasskeys pins the removal of approved people: one DELETE
// for each device id, in order, then one read of the resource, which must not
// list a removed person.
func TestRemoveAllowedPasskeys(t *testing.T) {
	const kept = "keep-keep-keep-keep"
	t.Run("removed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		for _, id := range []string{testDeviceID, kept, testOtherDevice} {
			srv.AddApprovedPerson(id, "person "+id)
		}
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID, testOtherDevice})
		if err != nil || len(resource.AllowedPasskeys) != 1 || resource.AllowedPasskeys[0].DeviceID != kept {
			t.Fatalf("RemoveAllowedPasskeys = %+v, %v", resource, err)
		}
		base := "/v1/resources/" + srv.Key.CRID
		want := []string{"DELETE " + base + "/allowed-passkeys/" + testDeviceID, "DELETE " + base + "/allowed-passkeys/" + testOtherDevice, "GET " + base}
		if got := requestLines(srv); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
	t.Run("a device id that is not on the list stops the rest", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddApprovedPerson(testOtherDevice, testOtherPerson)
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID, testOtherDevice})
		var problem *Error
		if resource != nil || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound ||
			!strings.Contains(err.Error(), "no approved person has the device id "+testDeviceID) || !strings.Contains(err.Error(), "nothing was removed") {
			t.Fatalf("RemoveAllowedPasskeys = %+v, %v", resource, err)
		}
		for _, line := range requestLines(srv) {
			if strings.Contains(line, testOtherDevice) {
				t.Fatalf("a removal after the failed one was sent: %v", requestLines(srv))
			}
		}
	})
	t.Run("the service has no access requests", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayNoAccessRequests()
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID})
		wantUnsupported(t, err)
	})
	t.Run("the person is still listed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddApprovedPerson(testDeviceID, testRequester)
		srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+testDeviceID, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID})
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgRemovalUnconfirmed {
			t.Fatalf("an answer that still lists the person returned %+v, %v", resource, err)
		}
	})
	t.Run("nothing that is not a device id is sent", func(t *testing.T) {
		srv := apitest.NewServer(t)
		for _, ids := range [][]string{nil, {"abcd"}, {testDeviceID, "../" + testOtherDevice}} {
			if _, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, ids); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
				t.Errorf("ids %v: error = %v", ids, err)
			}
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("a refused removal sent %d requests", got)
		}
	})
}

// TestResourceRowsCarryAccessRequestsAndApprovedPeople pins the two members a
// resource read gains, and that a row from a service without them reads as
// "not said", never as off.
func TestResourceRowsCarryAccessRequestsAndApprovedPeople(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetAccessRequests(true)
	srv.AddApprovedPerson(testDeviceID, testRequester)
	resource, err := newTestClient(t, srv, nil).Resource(t.Context(), srv.Key.CRID)
	if err != nil || resource.AccessRequests == nil || !*resource.AccessRequests || len(resource.AllowedPasskeys) != 1 {
		t.Fatalf("Resource = %+v, %v", resource, err)
	}
	if person := resource.AllowedPasskeys[0]; person.DeviceID != testDeviceID || person.Name != testRequester || person.ApprovedAt == nil {
		t.Fatalf("approved person = %+v", person)
	}

	older := apitest.NewServer(t)
	older.PlayNoAccessRequests()
	resource, err = newTestClient(t, older, nil).Resource(t.Context(), older.Key.CRID)
	if err != nil || resource.AccessRequests != nil || resource.AllowedPasskeys != nil {
		t.Fatalf("Resource from a service without access requests = %+v, %v", resource, err)
	}

	bad := apitest.NewServer(t)
	bad.AddApprovedPerson("not-a-device-id", testRequester)
	if resource, err := newTestClient(t, bad, nil).Resource(t.Context(), bad.Key.CRID); resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("a row with a malformed device id was accepted: %+v, %v", resource, err)
	}
}

// TestDeviceCredentialCannotReachAccessRequestRoutesYet records a limit of
// this release, and fails when the limit is gone.
//
// A device credential is used through the SDK, on the routes the SDK lists.
// The pinned SDK does not list the five routes that exist only for access
// requests, so on a real install, where every command uses the device
// credential, those requests are refused before anything is sent. The client
// turns that into a message that says so. Creating a resource with access
// requests, changing the setting and reading a resource use routes the SDK
// does list, and work.
//
// When the SDK lists these routes, this test fails. The fix is to delete its
// second half and to assert the opposite: that the five calls succeed with a
// device credential.
func TestDeviceCredentialCannotReachAccessRequestRoutesYet(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	client := newRegisteredTestClient(t, srv)

	if result, err := client.Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true}); err != nil || result.AccessRequests == nil || !*result.AccessRequests {
		t.Fatalf("publish with access requests through a device credential = %+v, %v", result, err)
	}
	if resource, err := client.SetAccessRequests(t.Context(), srv.Key.CRID, false); err != nil || *resource.AccessRequests {
		t.Fatalf("turning access requests off through a device credential = %+v, %v", resource, err)
	}
	sent := len(srv.Requests())

	for name, call := range map[string]func() error{
		"list all": func() error { _, err := client.AccessRequests(t.Context(), ""); return err },
		"list one": func() error { _, err := client.AccessRequests(t.Context(), srv.Key.CRID); return err },
		"approve": func() error {
			_, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
			return err
		},
		"deny": func() error { return client.DenyAccessRequest(t.Context(), srv.Key.CRID, testRequestCode) },
		"remove": func() error {
			_, err := client.RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID})
			return err
		},
	} {
		err := call()
		var shown interface{ UserMessage() string }
		if !errors.Is(err, qurl.ErrRegisteredAgentResourceRequestDenied) || !errors.As(err, &shown) || shown.UserMessage() != msgAccessRouteRefused {
			t.Errorf("%s through a device credential: error = %v; if it succeeded, the SDK now lists the route: see this test's comment", name, err)
		}
	}
	if got := len(srv.Requests()); got != sent {
		t.Fatalf("a refused request was sent: %d requests after the two that are allowed", got-sent)
	}
}
