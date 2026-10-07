package qurlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

const existingTarget = "https://example.com/data"

// firstCreateTime is the clock of the tests that judge whether a resource
// was made by the publish under test: the moment its first create is sent.
var firstCreateTime = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

// clockedClient is a client whose clock stands at firstCreateTime.
func clockedClient(t *testing.T, srv *apitest.Server) Client {
	t.Helper()
	client, err := New(&Config{
		BaseURL: srv.URL, APIKey: "lv_test_apitestingvalue123456789", Version: "test",
		Sleep: func(time.Duration) {},
		Now:   func() time.Time { return firstCreateTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// sentCreates returns the body members of every create request the mock
// received, in order.
func sentCreates(t *testing.T, srv *apitest.Server) []map[string]json.RawMessage {
	t.Helper()
	var bodies []map[string]json.RawMessage
	for _, request := range srv.Requests() {
		if request.Method != http.MethodPost || request.Path != "/v1/resources" {
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(request.Body, &body); err != nil {
			t.Fatalf("create request body %q: %v", request.Body, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// sentRequests returns "METHOD path" for every request the mock received.
func sentRequests(srv *apitest.Server) []string {
	requests := srv.Requests()
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return lines
}

// wantSecondCreateWithoutPrivacy fails the test unless the mock received
// exactly two create requests: the first stating private, the second equal
// to it in every member but without one for privacy.
func wantSecondCreateWithoutPrivacy(t *testing.T, srv *apitest.Server) {
	t.Helper()
	creates := sentCreates(t, srv)
	if len(creates) != 2 {
		t.Fatalf("create requests = %d, want 2: %v", len(creates), sentRequests(srv))
	}
	if string(creates[0]["private"]) != "true" {
		t.Fatalf("the first create did not state private: %v", creates[0])
	}
	if _, stated := creates[1]["private"]; stated {
		t.Fatalf("the second create stated a privacy: %v", creates[1])
	}
	delete(creates[0], "private")
	if len(creates[0]) != len(creates[1]) {
		t.Fatalf("the second create is not the first without its privacy: %v then %v", creates[0], creates[1])
	}
	for member, value := range creates[0] {
		if string(creates[1][member]) != string(value) {
			t.Fatalf("the second create changed %q: %s then %s", member, value, creates[1][member])
		}
	}
}

// TestPublishKeepsAnExistingPublicResourceWhenNoPrivacyWasNamed pins the
// publish of a person who never chose a privacy and whose target is already
// published as public, by the service's rules and by an older service's, for
// a URL and for a Connector resource. The create that states the default is
// refused, the same create without a privacy returns the resource that
// exists, and the result is that public resource, marked as kept.
func TestPublishKeepsAnExistingPublicResourceWhenNoPrivacyWasNamed(t *testing.T) {
	for _, older := range []bool{false, true} {
		for _, connectorID := range []string{"", "local-app"} {
			srv := apitest.NewServer(t)
			if older {
				srv.PlayPublicByDefault()
			}
			srv.SetResourceAccess(false)
			srv.SetPublishFoundExisting(true)
			target := existingTarget
			if connectorID != "" {
				target = ""
			}
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), target, PublishOptions{KeepExistingPublic: true, ConnectorID: connectorID})
			if err != nil {
				t.Fatalf("older %t, connector %q: %v", older, connectorID, err)
			}
			if !result.KeptPublic || result.Private == nil || *result.Private || result.FoundExisting == nil || !*result.FoundExisting || result.CRID != srv.Key.CRID {
				t.Fatalf("older %t, connector %q: result = %+v, want the existing public resource marked as kept", older, connectorID, result)
			}
			wantSecondCreateWithoutPrivacy(t, srv)
			if lines := sentRequests(srv); len(lines) != 2 {
				t.Fatalf("older %t, connector %q: requests = %v, want the two creates alone", older, connectorID, lines)
			}
		}
	}
}

// TestPublishKeepsNothingWhenAPrivacyWasNamed pins when the refusal stands.
// A caller that named a privacy, or a device list, asked for something the
// existing resource is not, so no second create is sent: the conflict is the
// answer. So is a refusal that says the existing resource is private, or
// that its device list differs, and any refusal that is not such a conflict.
func TestPublishKeepsNothingWhenAPrivacyWasNamed(t *testing.T) {
	for _, test := range []struct {
		name            string
		opts            PublishOptions
		existingPrivate bool
		want            ExistingAccess
	}{
		{name: "nothing named, and the caller did not say so", want: ExistingAccessPublic},
		{name: "a device list", opts: PublishOptions{KeepExistingPublic: true, AllowedDeviceKeys: []string{"a"}}, want: ExistingAccessPublic},
		{name: "public for a private resource", opts: PublishOptions{KeepExistingPublic: true, Public: true}, existingPrivate: true, want: ExistingAccessPrivate},
	} {
		for _, older := range []bool{false, true} {
			srv := apitest.NewServer(t)
			want := test.want
			if older {
				srv.PlayPublicByDefault()
				want = ExistingAccessUnknown
			}
			srv.SetResourceAccess(test.existingPrivate)
			srv.SetPublishFoundExisting(true)
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, test.opts)
			var conflict *PublishAccessConflictError
			if result != nil || !errors.As(err, &conflict) || conflict.Existing != want {
				t.Fatalf("%s, older %t: publish = %+v, %v, want conflict %d", test.name, older, result, err, want)
			}
			if lines := sentRequests(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
				t.Fatalf("%s, older %t: requests = %v, want the one create", test.name, older, lines)
			}
		}
	}

	for _, test := range []struct {
		name         string
		status       int
		code, detail string
	}{
		{name: "the device-list code", status: http.StatusBadRequest, code: apitest.CodeDeviceKeysMismatch, detail: "d"},
		{name: "another invalid input", status: http.StatusBadRequest, code: "invalid_input", detail: "target_url is not allowed"},
		{name: "the service is busy", status: http.StatusServiceUnavailable, code: "service_unavailable", detail: "try again"},
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, test.status, test.code, "Refused", test.detail)
		})
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
		if result != nil || err == nil {
			t.Fatalf("%s: publish = %+v, %v", test.name, result, err)
		}
		if lines := sentRequests(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
			t.Fatalf("%s: requests = %v, want the one create", test.name, lines)
		}
	}
}

// publicExistsRefusal is the service's answer to a create that states
// private for a target that is already published as public.
func publicExistsRefusal(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, apitest.CodePrivacyMismatch, "Privacy Mismatch", "d")
	}
}

// TestPublishHoldsTheSecondCreateToWhatWasMeant pins what the answer to the
// create without a privacy may be. It is the one create that leaves privacy
// to the service, so a public resource is kept only when the answer says it
// already existed. A private resource is what a publish with no flag asks
// for. A public resource that was just made is one nobody asked for: it is
// deleted and the publish fails without a result. One whose answer does not
// say whether it existed is neither kept nor deleted.
func TestPublishHoldsTheSecondCreateToWhatWasMeant(t *testing.T) {
	row := func(srv *apitest.Server, private any) map[string]any {
		data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active"}
		if private != nil {
			data["private"] = private
		}
		return data
	}
	deletion := func(srv *apitest.Server) string { return "DELETE /v1/resources/" + srv.Key.CRID }

	for _, test := range []struct {
		name    string
		private any
		meta    map[string]any
		check   func(*testing.T, *apitest.Server, *Published, error)
	}{
		{
			name: "public, already existed", private: false, meta: map[string]any{"found_existing": true},
			check: func(t *testing.T, _ *apitest.Server, result *Published, err error) {
				if err != nil || !result.KeptPublic || *result.Private {
					t.Fatalf("publish = %+v, %v, want the public resource marked as kept", result, err)
				}
			},
		},
		{
			name: "private, already existed", private: true, meta: map[string]any{"found_existing": true},
			check: func(t *testing.T, _ *apitest.Server, result *Published, err error) {
				if err != nil || result.KeptPublic || !*result.Private || !*result.FoundExisting {
					t.Fatalf("publish = %+v, %v, want the private resource, not marked", result, err)
				}
			},
		},
		{
			name: "private, just made", private: true, meta: map[string]any{"found_existing": false},
			check: func(t *testing.T, _ *apitest.Server, result *Published, err error) {
				if err != nil || result.KeptPublic || !*result.Private || *result.FoundExisting {
					t.Fatalf("publish = %+v, %v, want the new private resource, not marked", result, err)
				}
			},
		},
		{
			name: "public, and the answer does not say whether it existed", private: false,
			check: func(t *testing.T, srv *apitest.Server, result *Published, err error) {
				if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgPrivateUnconfirmed {
					t.Fatalf("publish = %+v, %v, want the unconfirmed-privacy failure", result, err)
				}
				if slices.Contains(sentRequests(srv), deletion(srv)) {
					t.Fatal("a resource that may have existed before was deleted")
				}
			},
		},
		{
			name: "no privacy in the answer", meta: map[string]any{"found_existing": true},
			check: func(t *testing.T, srv *apitest.Server, result *Published, err error) {
				if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgPrivateUnconfirmed {
					t.Fatalf("publish = %+v, %v, want the unconfirmed-privacy failure", result, err)
				}
				if slices.Contains(sentRequests(srv), deletion(srv)) {
					t.Fatal("a resource whose privacy is not known was deleted")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", publicExistsRefusal(t), func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusCreated, row(srv, test.private), test.meta)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
			wantSecondCreateWithoutPrivacy(t, srv)
			test.check(t, srv, result, err)
		})
	}
}

// TestPublishDeletesOnlyAResourceItsOwnRequestMade pins the two things that
// must both hold before the command deletes the public resource a second
// create answered with. A delete is final, so the answer's word that the
// resource is new is not enough: its creation time must also be no older
// than the moment this command sent its first create, less one minute for a
// local clock that runs ahead of the service's. An answer with no creation
// time, an older one, or one that is not a time is never acted on: nothing
// is deleted, no CRID is named, and the publisher is sent to look at what
// exists.
func TestPublishDeletesOnlyAResourceItsOwnRequestMade(t *testing.T) {
	at := func(offset time.Duration) string { return firstCreateTime.Add(offset).Format(time.RFC3339) }
	for _, test := range []struct {
		name      string
		createdAt any
		deleted   bool
	}{
		{name: "made after the first create", createdAt: at(2 * time.Second), deleted: true},
		{name: "made at the first create", createdAt: at(0), deleted: true},
		{name: "inside the clock tolerance", createdAt: at(-59 * time.Second), deleted: true},
		{name: "at the clock tolerance", createdAt: at(-time.Minute), deleted: true},
		{name: "just outside the clock tolerance", createdAt: at(-61 * time.Second)},
		{name: "made a day before", createdAt: at(-24 * time.Hour)},
		{name: "no creation time"},
		{name: "a creation time of zero", createdAt: "0001-01-01T00:00:00Z"},
		{name: "a creation time that is not a time", createdAt: "yesterday"},
		{name: "a creation time that is not a string", createdAt: 1772366400},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", publicExistsRefusal(t), func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active", "private": false}
				if test.createdAt != nil {
					data["created_at"] = test.createdAt
				}
				apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": false})
			})
			result, err := clockedClient(t, srv).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
			wantSecondCreateWithoutPrivacy(t, srv)
			var shown interface{ UserMessage() string }
			if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || !errors.As(err, &shown) {
				t.Fatalf("publish = %+v, %v, want a failure with a message and no result", result, err)
			}
			if strings.Contains(err.Error(), srv.Key.CRID) || strings.Contains(shown.UserMessage(), srv.Key.CRID) {
				t.Fatalf("the failure names the CRID: %v", err)
			}
			deletion := "DELETE /v1/resources/" + srv.Key.CRID
			lines := sentRequests(srv)
			if test.deleted {
				if shown.UserMessage() != msgUnaskedPublicDeleted || len(lines) != 3 || lines[2] != deletion {
					t.Fatalf("message %q, requests %v; want the deleted-resource failure after the two creates and the delete", shown.UserMessage(), lines)
				}
				return
			}
			if slices.Contains(lines, deletion) {
				t.Fatalf("requests = %v: a resource was deleted without evidence that this command made it", lines)
			}
			if shown.UserMessage() != msgPrivateUnconfirmed || !strings.Contains(shown.UserMessage(), "run `qurl list` to check") {
				t.Fatalf("message = %q, want the unconfirmed-privacy failure that sends the publisher to qurl list", shown.UserMessage())
			}
		})
	}

	// The new public resource could not be deleted: the failure says so and
	// how to find it, and still names no CRID.
	t.Run("the delete fails", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", publicExistsRefusal(t), func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active", "private": false, "created_at": at(time.Second),
			}, map[string]any{"found_existing": false})
		})
		srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
		})
		result, err := clockedClient(t, srv).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
		var shown interface{ UserMessage() string }
		if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || !errors.As(err, &shown) || shown.UserMessage() != msgUnaskedPublicNotDeleted {
			t.Fatalf("publish = %+v, %v, want the not-deleted failure", result, err)
		}
		if strings.Contains(shown.UserMessage(), srv.Key.CRID) || !strings.Contains(shown.UserMessage(), "`qurl list`") {
			t.Fatalf("the failure names the CRID or does not say how to find the resource: %q", shown.UserMessage())
		}
		if lines := sentRequests(srv); len(lines) != 3 {
			t.Fatalf("requests = %v, want the two creates and one delete, not retried", lines)
		}
	})

	// The clock is read once, before the first create is sent. Here an hour
	// passes while the first create is answered. Read afterwards, the clock
	// would make the new resource look an hour old and it would be left in
	// place; read before, the resource is newer than the command, and it is
	// deleted.
	t.Run("the clock is read before the first create", func(t *testing.T) {
		srv := apitest.NewServer(t)
		var mu sync.Mutex
		now, readings := firstCreateTime, 0
		srv.Script(http.MethodPost, "/v1/resources",
			func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				now = now.Add(time.Hour)
				mu.Unlock()
				publicExistsRefusal(t)(w, r)
			},
			func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "status": "active", "private": false, "created_at": at(time.Second),
				}, map[string]any{"found_existing": false})
			})
		client, err := New(&Config{
			BaseURL: srv.URL, APIKey: "lv_test_apitestingvalue123456789", Version: "test",
			Now: func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				readings++
				return now
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
		var shown interface{ UserMessage() string }
		mu.Lock()
		defer mu.Unlock()
		if !errors.As(err, &shown) || shown.UserMessage() != msgUnaskedPublicDeleted || readings != 1 {
			t.Fatalf("error = %v after %d clock readings, want the deleted-resource failure after one reading", err, readings)
		}
	})
}

// TestPublishSecondCreateRefused pins a second create that is refused too.
// The same kind of refusal again means the first one stands: it is what the
// publisher is told. Any other failure of the second request is reported as
// itself, because running the command again may well succeed.
func TestPublishSecondCreateRefused(t *testing.T) {
	t.Run("an access conflict again", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", publicExistsRefusal(t), func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", apitest.LegacyAccessSettingsDetail)
		})
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
		var conflict *PublishAccessConflictError
		if result != nil || !errors.As(err, &conflict) || conflict.Existing != ExistingAccessPublic {
			t.Fatalf("publish = %+v, %v, want the first refusal: already published as public", result, err)
		}
		wantSecondCreateWithoutPrivacy(t, srv)
	})
	t.Run("the service is busy", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", publicExistsRefusal(t), func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
		})
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{KeepExistingPublic: true})
		var problem *Error
		if result != nil || !errors.As(err, &problem) || problem.StatusCode != http.StatusServiceUnavailable || errors.Is(err, ErrPublishAccessConflict) {
			t.Fatalf("publish = %+v, %v, want the second request's own failure", result, err)
		}
		if lines := sentRequests(srv); len(lines) != 2 {
			t.Fatalf("requests = %v, want the two creates, the second not retried", lines)
		}
	})
}

// TestPublishWithADeviceListThatDiffersIsAConflict pins the three ways a
// publish learns that the target is already published with another list of
// allowed devices. Each is a conflict, never an answer outside the contract:
// the service's code, an answer that accepted the request and returned the
// stored list of a resource that already existed, and the older service's
// refusal, which does not say what differs. Only a resource that was just
// made with another list is an answer the service should not have given.
func TestPublishWithADeviceListThatDiffersIsAConflict(t *testing.T) {
	asked := PublishOptions{AllowedDeviceKeys: []string{"asked"}}
	accepted := func(srv *apitest.Server, meta map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "allowed_device_keys": []string{"stored"},
			}, meta)
		}
	}

	t.Run("the service's code", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, "stored")
		srv.SetPublishFoundExisting(true)
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, asked)
		var conflict *PublishAccessConflictError
		var problem *Error
		if !errors.As(err, &conflict) || conflict.Existing != ExistingAccessOtherDevices || !errors.As(err, &problem) || problem.RequestID == "" {
			t.Fatalf("error = %v, want the device-list conflict with the service's request id", err)
		}
	})
	t.Run("an accepted request for a resource that existed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", accepted(srv, map[string]any{"found_existing": true}))
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, asked)
		var conflict *PublishAccessConflictError
		if result != nil || !errors.As(err, &conflict) || conflict.Existing != ExistingAccessOtherDevices || !errors.Is(err, ErrPublishAccessConflict) {
			t.Fatalf("publish = %+v, %v, want the device-list conflict", result, err)
		}
		if errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatalf("a list that differs on an existing resource was read as a broken answer: %v", err)
		}
	})
	t.Run("the older service", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayPublicByDefault()
		srv.SetResourceAccess(true, "stored")
		srv.SetPublishFoundExisting(true)
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, asked)
		var conflict *PublishAccessConflictError
		if !errors.As(err, &conflict) || conflict.Existing != ExistingAccessUnknown {
			t.Fatalf("error = %v, want the conflict that names nothing", err)
		}
	})
	// The same list in another order is the same list.
	t.Run("the same list", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, "b", "a")
		srv.SetPublishFoundExisting(true)
		if _, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, PublishOptions{AllowedDeviceKeys: []string{"a", "b"}}); err != nil {
			t.Fatal(err)
		}
	})
	for name, meta := range map[string]map[string]any{"just made": {"found_existing": false}, "not said": nil} {
		t.Run("a resource that was "+name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", accepted(srv, meta))
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), existingTarget, asked)
			if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || errors.Is(err, ErrPublishAccessConflict) {
				t.Fatalf("publish = %+v, %v, want an answer outside the contract", result, err)
			}
		})
	}
}
