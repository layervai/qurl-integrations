package apitest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// accessAnswer is the part of an access-request answer these tests read.
type accessAnswer struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// accessCall sends one authenticated JSON request and decodes the answer,
// which may have no body.
func accessCall(t *testing.T, srv *Server, method, path, body string) (int, accessAnswer) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer lv_test_apitest")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded accessAnswer
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode %s %s answer: %v", method, path, err)
		}
	}
	return resp.StatusCode, decoded
}

type requestRow struct {
	Code     string `json:"request_code"`
	Name     string `json:"name"`
	DeviceID string `json:"device_id"`
	CRID     string `json:"crid"`
}

type personRow struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

func resourceState(t *testing.T, srv *Server) (accessRequests *bool, people []personRow) {
	t.Helper()
	_, answer := accessCall(t, srv, http.MethodGet, "/v1/resources/"+srv.Key.CRID, "")
	var data struct {
		Resource struct {
			AccessRequests *bool       `json:"access_requests"`
			People         []personRow `json:"allowed_passkeys"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(answer.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Resource.AccessRequests, data.Resource.People
}

// TestAccessRequestRoutesFollowTheContract walks the mock's publisher API for
// access requests: the two listings, an approval that moves a person from the
// pending requests to the approved people in one change, a denial that gives
// nobody access, a removal, and 404 for a code or a device id it does not
// have.
func TestAccessRequestRoutesFollowTheContract(t *testing.T) {
	srv := NewServer(t)
	srv.SetAccessRequests(true)
	srv.AddAccessRequest("482913", "Ana Lopez", "abcd-efgh-2345-mnop")
	srv.AddAccessRequest("175306", "Sam Okafor", "qrst-uvwx-yz67-abcd")
	base := "/v1/resources/" + srv.Key.CRID

	for path, wantCRID := range map[string]string{"/v1/access-requests": srv.Key.CRID, base + "/access-requests": ""} {
		status, answer := accessCall(t, srv, http.MethodGet, path, "")
		var rows []requestRow
		if err := json.Unmarshal(answer.Data, &rows); err != nil || status != http.StatusOK || len(rows) != 2 {
			t.Fatalf("GET %s = %d %s (%v)", path, status, answer.Data, err)
		}
		if rows[0].Code != "482913" || rows[0].Name != "Ana Lopez" || rows[0].DeviceID != "abcd-efgh-2345-mnop" || rows[0].CRID != wantCRID {
			t.Fatalf("GET %s first row = %+v", path, rows[0])
		}
	}

	status, answer := accessCall(t, srv, http.MethodPost, base+"/access-requests/482913/approve", "{}")
	var approved personRow
	if err := json.Unmarshal(answer.Data, &approved); err != nil || status != http.StatusOK || approved.DeviceID != "abcd-efgh-2345-mnop" || approved.Name != "Ana Lopez" {
		t.Fatalf("approve = %d %s (%v)", status, answer.Data, err)
	}
	if status, answer := accessCall(t, srv, http.MethodPost, base+"/access-requests/482913/approve", "{}"); status != http.StatusNotFound || answer.Error.Code != "not_found" {
		t.Fatalf("a second approval of the same code = %d %q, want 404 not_found", status, answer.Error.Code)
	}
	if status, _ := accessCall(t, srv, http.MethodDelete, base+"/access-requests/175306", ""); status != http.StatusNoContent {
		t.Fatalf("deny = %d, want 204", status)
	}
	if status, _ := accessCall(t, srv, http.MethodDelete, base+"/access-requests/175306", ""); status != http.StatusNotFound {
		t.Fatalf("a second denial = %d, want 404", status)
	}
	on, people := resourceState(t, srv)
	if on == nil || !*on || len(people) != 1 || people[0].DeviceID != "abcd-efgh-2345-mnop" {
		t.Fatalf("resource after one approval and one denial: access_requests %v, people %+v", on, people)
	}
	if status, _ := accessCall(t, srv, http.MethodDelete, base+"/allowed-passkeys/qrst-uvwx-yz67-abcd", ""); status != http.StatusNotFound {
		t.Fatalf("removal of a person who was never approved = %d, want 404", status)
	}
	if status, _ := accessCall(t, srv, http.MethodDelete, base+"/allowed-passkeys/abcd-efgh-2345-mnop", ""); status != http.StatusNoContent {
		t.Fatalf("removal = %d, want 204", status)
	}
	if _, people := resourceState(t, srv); len(people) != 0 {
		t.Fatalf("people after the removal: %+v", people)
	}
	if status, _ := accessCall(t, srv, http.MethodGet, "/v1/resources/another/access-requests", ""); status != http.StatusNotFound {
		t.Fatalf("listing of another resource = %d, want 404", status)
	}
}

// TestAccessRequestsSettingNeedsAPrivateResource pins the one rule of the
// setting: only a private resource may turn it on, at creation and by a
// change, and a refused change leaves it off.
func TestAccessRequestsSettingNeedsAPrivateResource(t *testing.T) {
	srv := NewServer(t)
	path := "/v1/resources/" + srv.Key.CRID
	if status, _ := accessCall(t, srv, http.MethodPatch, path, `{"access_requests":true}`); status != http.StatusOK {
		t.Fatalf("turning requests on for a private resource = %d", status)
	}
	if on, _ := resourceState(t, srv); on == nil || !*on {
		t.Fatalf("access_requests after turning them on = %v", on)
	}
	srv.SetAccessRequests(false)
	srv.SetResourceAccess(false)
	if status, answer := accessCall(t, srv, http.MethodPatch, path, `{"access_requests":true}`); status != http.StatusBadRequest || answer.Error.Code != "invalid_input" {
		t.Fatalf("turning requests on for a public resource = %d %q, want 400 invalid_input", status, answer.Error.Code)
	}
	if on, _ := resourceState(t, srv); on == nil || *on {
		t.Fatalf("a refused change turned requests on: %v", on)
	}
	const create = `{"type":"url","target_url":"https://example.com/data","private":%s,"access_requests":true}`
	if status, _ := accessCall(t, srv, http.MethodPost, "/v1/resources", strings.Replace(create, "%s", "false", 1)); status != http.StatusBadRequest {
		t.Fatalf("creating a public resource with access requests = %d, want 400", status)
	}
	if status, _ := accessCall(t, srv, http.MethodPost, "/v1/resources", strings.Replace(create, "%s", "true", 1)); status != http.StatusCreated {
		t.Fatalf("creating a private resource with access requests = %d, want 201", status)
	}
	if on, _ := resourceState(t, srv); on == nil || !*on {
		t.Fatalf("access_requests after the create = %v", on)
	}
}

// TestOlderServiceHasNoAccessRequests pins the mock of a service from before
// access requests: every one of their routes is 404, the setting is ignored
// at creation and in a change, and no row has it.
func TestOlderServiceHasNoAccessRequests(t *testing.T) {
	srv := NewServer(t)
	srv.AddAccessRequest("482913", "Ana Lopez", "abcd-efgh-2345-mnop")
	srv.PlayNoAccessRequests()
	base := "/v1/resources/" + srv.Key.CRID
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/access-requests"},
		{http.MethodGet, base + "/access-requests"},
		{http.MethodPost, base + "/access-requests/482913/approve"},
		{http.MethodDelete, base + "/access-requests/482913"},
		{http.MethodDelete, base + "/allowed-passkeys/abcd-efgh-2345-mnop"},
	} {
		if status, answer := accessCall(t, srv, route.method, route.path, "{}"); status != http.StatusNotFound || answer.Error.Code != "not_found" {
			t.Errorf("%s %s = %d %q, want 404 not_found", route.method, route.path, status, answer.Error.Code)
		}
	}
	if status, _ := accessCall(t, srv, http.MethodPatch, base, `{"access_requests":true}`); status != http.StatusOK {
		t.Fatalf("a change that carries only the unknown setting = %d, want 200", status)
	}
	if status, _ := accessCall(t, srv, http.MethodPost, "/v1/resources", `{"type":"url","target_url":"https://example.com/data","private":true,"access_requests":true}`); status != http.StatusCreated {
		t.Fatalf("a create that carries the unknown setting = %d, want 201", status)
	}
	if on, people := resourceState(t, srv); on != nil || people != nil {
		t.Fatalf("a row of that service has the setting or the people: %v %+v", on, people)
	}
}

// TestAccessRequestRoutesNeedACredential pins that every access-request route
// refuses a request with no credential and changes nothing.
func TestAccessRequestRoutesNeedACredential(t *testing.T) {
	srv := NewServer(t)
	srv.AddAccessRequest("482913", "Ana Lopez", "abcd-efgh-2345-mnop")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/resources/"+srv.Key.CRID+"/access-requests/482913/approve", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("approval without a credential = %d, want 401", resp.StatusCode)
	}
	if _, people := resourceState(t, srv); len(people) != 0 {
		t.Fatalf("an approval without a credential gave access: %+v", people)
	}
}
