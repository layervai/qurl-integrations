package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

type untouchedAgentStateStore struct{}

func (untouchedAgentStateStore) LoadAgentState(context.Context) (*qurl.AgentState, error) {
	panic("cleartext endpoint validation loaded registered state")
}

func (untouchedAgentStateStore) SaveAgentState(context.Context, *qurl.AgentState) error {
	panic("cleartext endpoint validation saved registered state")
}

func TestClientConstructorsRejectCleartextNonLoopbackBaseURL(t *testing.T) {
	for _, test := range []struct {
		name string
		open func() error
	}{
		{name: "account", open: func() error {
			_, err := New(&Config{BaseURL: "http://api.example.com", APIKey: "lv_test_boundary", Version: "test"})
			return err
		}},
		{name: "registered", open: func() error {
			_, err := NewRegistered(context.Background(), &Config{
				BaseURL: "http://api.example.com", Version: "test",
			}, untouchedAgentStateStore{})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.open(); !errors.Is(err, qurl.ErrInvalidClientConfig) {
				t.Fatalf("cleartext non-loopback endpoint error = %v, want ErrInvalidClientConfig", err)
			}
		})
	}
}

func TestSharingLifecycleWireContract(t *testing.T) {
	srv := apitest.NewServer(t)
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	reply := func(desired DesiredState, epoch uint64, connection ConnectionState) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id":      srv.Key.ResourceID,
				"crid":             srv.Key.CRID,
				"desired_state":    desired,
				"serving_epoch":    epoch,
				"connection_state": connection,
			}, nil)
		}
	}
	srv.Script(http.MethodGet, path, reply(DesiredStateOff, 4, ConnectionStopped))
	srv.Script(http.MethodPut, path, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode PUT body: %v", err)
		}
		if len(body) != 1 || body["desired_state"] != "on" {
			t.Errorf("PUT body = %#v, want strict desired_state document", body)
		}
		reply(DesiredStateOn, 5, ConnectionConnecting)(w, r)
	})
	srv.Script(http.MethodPost, path+"/restart", reply(DesiredStateOn, 6, ConnectionConnecting))

	client := newTestClient(t, srv, nil)
	got, err := client.Sharing(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != DesiredStateOff || got.ServingEpoch != 4 || got.ConnectionState != ConnectionStopped {
		t.Fatalf("GET sharing = %+v", got)
	}
	got, err = client.SetSharing(context.Background(), srv.Key.CRID, DesiredStateOn)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != DesiredStateOn || got.ServingEpoch != 5 || got.ConnectionState != ConnectionConnecting {
		t.Fatalf("PUT sharing = %+v", got)
	}
	got, err = client.RestartSharing(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServingEpoch != 6 || got.DesiredState != DesiredStateOn {
		t.Fatalf("POST restart = %+v", got)
	}
}

func TestSharingLifecycleFailsClosedOnInvalidState(t *testing.T) {
	srv := apitest.NewServer(t)
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id":      srv.Key.ResourceID,
			"crid":             srv.Key.CRID,
			"desired_state":    "maybe",
			"serving_epoch":    1,
			"connection_state": "serving",
		}, nil)
	})
	client := newTestClient(t, srv, nil)
	if _, err := client.Sharing(context.Background(), srv.Key.CRID); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("invalid desired state error = %v", err)
	}
	if _, err := client.SetSharing(context.Background(), srv.Key.CRID, "maybe"); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
		t.Fatalf("invalid desired input error = %v", err)
	}
}

func TestSetSharingRejectsResponseThatDidNotApplyRequestedState(t *testing.T) {
	tests := []struct {
		name                string
		requested, returned DesiredState
		connection          ConnectionState
	}{
		{name: "requested on returned off", requested: DesiredStateOn, returned: DesiredStateOff, connection: ConnectionStopped},
		{name: "requested off returned on", requested: DesiredStateOff, returned: DesiredStateOn, connection: ConnectionServing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodPut, path, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": test.returned, "serving_epoch": 7,
					"connection_state": test.connection,
				}, nil)
			})
			client := newTestClient(t, srv, nil)
			if _, err := client.SetSharing(context.Background(), srv.Key.CRID, test.requested); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("SetSharing mismatch error = %v, want ErrInvalidAPIResponse", err)
			}
		})
	}
}

func TestSharingLifecycleRequiresCanonicalServingEpochField(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	base := fmt.Sprintf(`"resource_id":%q,"crid":%q,"desired_state":"off","connection_state":"stopped"`, key.ResourceID, key.CRID)
	tests := map[string]string{
		"missing":   base,
		"null":      base + `,"serving_epoch":null`,
		"string":    base + `,"serving_epoch":"0"`,
		"fraction":  base + `,"serving_epoch":0.0`,
		"exponent":  base + `,"serving_epoch":0e0`,
		"negative":  base + `,"serving_epoch":-1`,
		"overflow":  base + `,"serving_epoch":18446744073709551616`,
		"duplicate": base + `,"serving_epoch":0,"serving_epoch":0`,
	}
	for name, fields := range tests {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServerWithKey(t, key)
			srv.Script(http.MethodGet, "/v1/resources/"+key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":{%s}}`, fields)
			})
			if _, err := newTestClient(t, srv, nil).Sharing(context.Background(), key.CRID); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("Sharing() error=%v, want invalid API response", err)
			}
		})
	}

	t.Run("present zero stopped", func(t *testing.T) {
		srv := apitest.NewServerWithKey(t, key)
		srv.Script(http.MethodGet, "/v1/resources/"+key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":{%s,"serving_epoch":0,"future_field":{"allowed":true}}}`, base)
		})
		got, err := newTestClient(t, srv, nil).Sharing(context.Background(), key.CRID)
		if err != nil || got.ServingEpoch != 0 || got.ConnectionState != ConnectionStopped {
			t.Fatalf("Sharing()=%+v, %v, want stopped epoch zero", got, err)
		}
	})
}

func TestSharingLifecycleFailsClosedOnWrongResourceIdentity(t *testing.T) {
	requestKey := apitest.GenerateResourceKey(t)
	otherKey := apitest.GenerateResourceKey(t)
	tests := map[string]func(map[string]any){
		"malformed public key": func(row map[string]any) { row["resource_id"] = "not-a-public-key" },
		"malformed CRID":       func(row map[string]any) { row["crid"] = "not-a-crid" },
		"mismatched pair":      func(row map[string]any) { row["crid"] = otherKey.CRID },
		"different resource": func(row map[string]any) {
			row["resource_id"] = otherKey.ResourceID
			row["crid"] = otherKey.CRID
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServerWithKey(t, requestKey)
			row := map[string]any{
				"resource_id": requestKey.ResourceID, "crid": requestKey.CRID,
				"desired_state": "on", "serving_epoch": 1, "connection_state": "serving",
			}
			mutate(row)
			srv.Script(http.MethodGet, "/v1/resources/"+requestKey.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, row, nil)
			})
			client := newTestClient(t, srv, nil)
			if _, err := client.Sharing(context.Background(), requestKey.CRID); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("Sharing error = %v, want invalid API response", err)
			}
		})
	}
}

func TestSharingLifecycleAcceptsPublicIDAndMatchingCRIDHandles(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	for _, id := range []string{key.ResourceID, key.CRID} {
		t.Run(id[:8], func(t *testing.T) {
			srv := apitest.NewServerWithKey(t, key)
			srv.Script(http.MethodGet, "/v1/resources/"+id+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": key.ResourceID, "crid": key.CRID,
					"desired_state": "on", "serving_epoch": 1, "connection_state": "serving",
				}, nil)
			})
			if _, err := newTestClient(t, srv, nil).Sharing(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartSharingNeverReplaysRateLimit(t *testing.T) {
	srv := apitest.NewServer(t)
	path := "/v1/resources/" + srv.Key.CRID + "/sharing/restart"
	srv.Script(http.MethodPost, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Rate limited", "result is ambiguous")
	})
	client := newTestClient(t, srv, nil)
	if _, err := client.RestartSharing(context.Background(), srv.Key.CRID); err == nil {
		t.Fatal("RestartSharing unexpectedly succeeded")
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodPost {
		t.Fatalf("restart requests = %#v, want exactly one POST", requests)
	}
}

func TestPublishNeverReplaysRateLimit(t *testing.T) {
	for _, test := range []struct {
		name string
		open func(*testing.T, *apitest.Server) Client
	}{
		{name: "account", open: func(t *testing.T, srv *apitest.Server) Client { return newTestClient(t, srv, nil) }},
		{name: "registered", open: newRegisteredTestClient},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.ScriptRepeat(http.MethodPost, "/v1/resources", maxAttempts, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Rate limited", "publish result is ambiguous")
			})
			if _, err := test.open(t, srv).Publish(context.Background(), "https://example.com", PublishOptions{}); err == nil {
				t.Fatal("Publish unexpectedly succeeded")
			}
			if got := len(srv.Requests()); got != 1 {
				t.Fatalf("publish requests = %d, want one", got)
			}
		})
	}
}

func TestRestartSharingNeverReplaysTransportError(t *testing.T) {
	var attempts int
	c, err := New(&Config{
		BaseURL: "https://api.invalid", APIKey: "key", Version: "test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("response lost")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RestartSharing(context.Background(), apitest.FixedResourceKey(t).CRID); err == nil {
		t.Fatal("RestartSharing unexpectedly succeeded")
	}
	if attempts != 1 {
		t.Fatalf("restart attempts=%d, want exactly one", attempts)
	}
}

func TestManagementMutationsDeclareExplicitNoReplay(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*testing.T, Client) error
	}{
		{name: "set sharing", call: func(t *testing.T, c Client) error {
			t.Helper()
			_, err := c.SetSharing(context.Background(), apitest.FixedResourceKey(t).CRID, DesiredStateOn)
			return err
		}},
		{name: "delete", call: func(t *testing.T, c Client) error {
			t.Helper()
			_, err := c.Delete(context.Background(), apitest.FixedResourceKey(t).CRID)
			return err
		}},
		{name: "replace device grants", call: func(t *testing.T, c Client) error {
			t.Helper()
			_, err := c.SetDeviceGrants(context.Background(), apitest.FixedResourceKey(t).CRID, nil)
			return err
		}},
		{name: "edit device grants", call: func(t *testing.T, c Client) error {
			t.Helper()
			_, err := c.EditDeviceGrants(context.Background(), apitest.FixedResourceKey(t).CRID, []string{"added-key"}, []string{"removed-key"})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			client, err := New(&Config{
				BaseURL: "https://api.invalid", APIKey: "key", Version: "test",
				HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					allowRetry, explicit := req.Context().Value(requestRetryIntentKey{}).(bool)
					if !explicit || allowRetry {
						t.Errorf("retry intent explicit/allowed = %t/%t, want true/false", explicit, allowRetry)
					}
					return nil, errors.New("response lost")
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := test.call(t, client); err == nil {
				t.Fatal("management mutation unexpectedly succeeded")
			}
			if attempts != 1 {
				t.Fatalf("management mutation attempts = %d, want one", attempts)
			}
		})
	}
}

func TestSharingResponseInvariants(t *testing.T) {
	valid := sharingRow{ResourceID: "resource", CRID: "crid", DesiredState: DesiredStateOn, ServingEpoch: 1, ConnectionState: ConnectionServing}
	tests := map[string]func(*sharingRow){
		"missing resource":   func(row *sharingRow) { row.ResourceID = "" },
		"missing crid":       func(row *sharingRow) { row.CRID = "" },
		"off but serving":    func(row *sharingRow) { row.DesiredState = DesiredStateOff },
		"on but stopped":     func(row *sharingRow) { row.ConnectionState = ConnectionStopped },
		"on with zero epoch": func(row *sharingRow) { row.ServingEpoch = 0 },
		"unknown connection": func(row *sharingRow) { row.ConnectionState = "lost" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			row := valid
			mutate(&row)
			if err := validateSharingRow(&row); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("validateSharingRow(%+v) error = %v", row, err)
			}
		})
	}
}

func TestListRejectsTunnelWithoutDesiredState(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{
			"resource_id": srv.Key.ResourceID,
			"crid":        srv.Key.CRID,
			"type":        "tunnel",
			"status":      "active",
		}}, map[string]any{"has_more": false})
	})
	client := newTestClient(t, srv, nil)
	if _, err := client.List(context.Background(), ListOptions{}); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("tunnel list error = %v, want invalid API response", err)
	}
}

func TestListRejectsInvalidTunnelIdentity(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	otherKey := apitest.GenerateResourceKey(t)
	tests := map[string]func(map[string]any){
		"missing CRID": func(row map[string]any) {
			delete(row, "crid")
		},
		"malformed CRID": func(row map[string]any) {
			row["crid"] = "not-a-crid"
		},
		"missing resource identity": func(row map[string]any) {
			delete(row, "resource_id")
		},
		"malformed resource identity": func(row map[string]any) {
			row["resource_id"] = "not-a-resource-key"
		},
		"mismatched CRID": func(row map[string]any) {
			row["crid"] = otherKey.CRID
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServerWithKey(t, key)
			row := map[string]any{
				"resource_id":   key.ResourceID,
				"crid":          key.CRID,
				"type":          "tunnel",
				"status":        "active",
				"desired_state": "off",
				"serving_epoch": 1,
			}
			mutate(row)
			srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{row}, map[string]any{"has_more": false})
			})
			client := newTestClient(t, srv, nil)
			if _, err := client.List(context.Background(), ListOptions{}); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("List error = %v, want invalid API response", err)
			}
		})
	}
}

func TestListRejectsMismatchedURLIdentity(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	other := apitest.GenerateResourceKey(t)
	srv := apitest.NewServerWithKey(t, key)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{
			"resource_id": key.ResourceID, "crid": other.CRID,
			"type": "url", "status": "active",
		}}, map[string]any{"has_more": false})
	})
	if _, err := newTestClient(t, srv, nil).List(context.Background(), ListOptions{}); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("List error = %v, want invalid API response", err)
	}
}

// testRequestID is the harness's fixed X-Request-Id value.
const testRequestID = "unit-req"

func newTestClient(t *testing.T, srv *apitest.Server, sleeps *[]time.Duration) Client {
	t.Helper()
	cfg := Config{
		BaseURL:      srv.URL,
		APIKey:       "lv_test_apitestingvalue123456789",
		Version:      "test",
		NewRequestID: func() string { return testRequestID },
	}
	if sleeps != nil {
		cfg.Sleep = func(d time.Duration) { *sleeps = append(*sleeps, d) }
	} else {
		cfg.Sleep = func(time.Duration) {}
	}
	client, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestPublishSendsPinnedWireShape(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(false)
	client := newTestClient(t, srv, nil)

	// The mock enforces the pinned contract (type=url + target_url required),
	// so a successful publish IS the wire-shape assertion.
	res, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CRID != srv.Key.CRID || res.ResourceID != srv.Key.ResourceID {
		t.Errorf("mapped identity mismatch: %+v", res)
	}
	if res.TargetURL != "https://example.com/data" {
		t.Errorf("target = %q", res.TargetURL)
	}
	if res.FoundExisting == nil || *res.FoundExisting {
		t.Errorf("fresh publish FoundExisting = %v, want known false", res.FoundExisting)
	}

	srv.SetPublishFoundExisting(true)
	res, err = client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FoundExisting == nil || !*res.FoundExisting {
		t.Errorf("replayed publish FoundExisting = %v, want known true", res.FoundExisting)
	}
}

func TestPublishRejectsCRIDThatDoesNotCommitToResourceID(t *testing.T) {
	srv := apitest.NewServer(t)
	other := apitest.GenerateResourceKey(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
			"resource_id": srv.Key.ResourceID,
			"crid":        other.CRID,
			"target_url":  "https://example.com/data",
			"status":      "active",
		}, nil)
	})
	client := newTestClient(t, srv, nil)
	if _, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{}); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("mismatched publish identity error = %v, want ErrInvalidAPIResponse", err)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Fatalf("mismatched publish requests = %d, want one", got)
	}
}

func TestPublishRejectsMissingCRID(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
			"resource_id": srv.Key.ResourceID,
			"target_url":  "https://example.com",
			"status":      "active",
		}, nil)
	})
	_, err := newTestClient(t, srv, nil).Publish(context.Background(), "https://example.com", PublishOptions{})
	if !errors.Is(err, qurl.ErrInvalidAPIResponse) || !strings.Contains(err.Error(), "missing crid") {
		t.Fatalf("missing publish CRID error = %v, want invalid API response", err)
	}
}

func TestPublishPreservesOmittedFoundExistingAsUnknown(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.OmitPublishFoundExisting()
	client := newTestClient(t, srv, nil)

	res, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FoundExisting != nil {
		t.Errorf("omitted FoundExisting = %v, want unknown", *res.FoundExisting)
	}
}

func TestPublishValidatesTargetLocally(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)
	for name, target := range map[string]string{
		"empty":       "",
		"scheme":      "ftp://example.com",
		"no host":     "https://",
		"credentials": "https://user:pass@example.com/x",
	} {
		if _, err := client.Publish(context.Background(), target, PublishOptions{}); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidResourceRequest", name, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("local validation must not send requests, saw %d", got)
	}
}

func TestShareDark503PreservesSentinel(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", apitest.HandlerDark503(t))
	client := newTestClient(t, srv, nil)

	_, err := client.Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if !errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		t.Fatalf("err = %v, want the dark-surface sentinel preserved through mapping", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("typed Error with 503 must also be reachable, got %v", err)
	}
}

func TestShareStoppedConnectorPreservesProblemCodeAndDoesNotRetry(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", 2, apitest.HandlerConnectorStopped503(t))

	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	_, err := client.Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if !errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		t.Fatalf("err = %v, want the 503 sentinel preserved", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want typed API error", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Code != "connector_stopped" {
		t.Errorf("typed API error = %+v, want HTTP 503 connector_stopped", apiErr)
	}
	if len(sleeps) != 0 {
		t.Errorf("stopped Connector must not be retried; slept %v", sleeps)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestTransportRetries429ThenSucceeds(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", apitest.Handler429(t, 2), apitest.Handler429(t, 1))

	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	page, err := client.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Errorf("items = %d, want the default row", len(page.Items))
	}
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 1*time.Second {
		t.Errorf("sleeps = %v, want [2s 1s] from Retry-After", sleeps)
	}
	if got := len(srv.Requests()); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestTransportBackoffIsContextAware pins the round-4 disposition: a context
// canceled during the 429 backoff returns promptly with the context error
// instead of finishing the sleep. No Sleep is injected, so this exercises
// the real timer+select path against a 5-second Retry-After.
func TestTransportBackoffIsContextAware(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodGet, "/v1/resources", 3, apitest.Handler429(t, 5))

	cfg := Config{
		BaseURL:      srv.URL,
		APIKey:       "lv_test_apitestingvalueapitestingvalue0123456789abc",
		Version:      "test",
		NewRequestID: func() string { return testRequestID },
	}
	client, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = client.List(ctx, ListOptions{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("cancellation took %v; the backoff must return promptly, not wait out Retry-After", elapsed)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry after cancellation)", got)
	}
}

func TestTransportRetryAfterFallbackAndCap(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if d := retryDelay(resp, 1); d != 500*time.Millisecond {
		t.Errorf("fallback delay = %v", d)
	}
	resp.Header.Set("Retry-After", "not-a-number")
	if d := retryDelay(resp, 2); d != time.Second {
		t.Errorf("unparseable Retry-After delay = %v", d)
	}
	resp.Header.Set("Retry-After", "0")
	if d := retryDelay(resp, 1); d != 500*time.Millisecond {
		t.Errorf("zero Retry-After delay = %v, want fallback", d)
	}
	resp.Header.Set("Retry-After", "3600")
	if d := retryDelay(resp, 1); d != maxRetryAfter {
		t.Errorf("capped delay = %v, want %v", d, maxRetryAfter)
	}
	resp.Header.Set("Retry-After", "9223372037")
	if d := retryDelay(resp, 1); d != maxRetryAfter {
		t.Errorf("overflowing duration delay = %v, want %v", d, maxRetryAfter)
	}
	resp.Header.Set("Retry-After", "18446744073709551615")
	if d := retryDelay(resp, 1); d != maxRetryAfter {
		t.Errorf("maximum uint64 delay = %v, want %v", d, maxRetryAfter)
	}
}

func TestRetrySafeRequestAllowsOnlyExactShareRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		basePath string
		want     bool
	}{
		{name: "exact", path: "/v1/resources/qexample/share", want: true},
		{name: "exact with base path", path: "/proxy/v1/resources/qexample/share", basePath: "/proxy", want: true},
		{name: "wrong base path", path: "/other/v1/resources/qexample/share", basePath: "/proxy"},
		{name: "empty resource", path: "/v1/resources//share"},
		{name: "nested resource", path: "/v1/resources/qexample/sessions/share"},
		{name: "different version", path: "/v2/resources/qexample/share"},
		{name: "unrelated suffix", path: "/v1/admin/share"},
		{name: "trailing slash", path: "/v1/resources/qexample/share/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(
				context.Background(), http.MethodPost, "https://api.example"+tc.path, http.NoBody,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := retrySafeRequest(req, tc.basePath); got != tc.want {
				t.Errorf("retrySafeRequest(%q) = %t, want %t", tc.path, got, tc.want)
			}
		})
	}
}

// TestDeleteIsIdempotent pins the delete contract: 204 and 404 are both
// success — the second delete of anything reports AlreadyGone, never an
// error — while other failures stay typed.
func TestDeleteIsIdempotent(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	result, err := client.Delete(context.Background(), " \t"+srv.Key.CRID+"\r\n")
	if err != nil || result.AlreadyGone {
		t.Fatalf("fresh delete: result=%+v err=%v", result, err)
	}

	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, apitest.HandlerNotFound404(t, "not_found"))
	result, err = client.Delete(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatalf("hard-deleted delete must succeed: %v", err)
	}
	if !result.AlreadyGone {
		t.Error("404 delete must report AlreadyGone")
	}

	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusForbidden, "insufficient_scope", "Forbidden", "not yours")
	})
	_, err = client.Delete(context.Background(), srv.Key.CRID)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "insufficient_scope" {
		t.Errorf("typed failure lost: %v", err)
	}
	if apiErr.RequestID != "req_test" {
		t.Errorf("meta.request_id not mapped: %+v", apiErr)
	}
}

// TestDark503IsNeverAutoRetried pins the no-retry rule for the dark surface:
// its Retry-After header reflects deployment state, not transience, so the
// transport must surface it on the first answer.
func TestDark503IsNeverAutoRetried(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ScriptRepeat(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", 2, apitest.HandlerDark503(t))

	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	_, err := client.Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if !errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled) {
		t.Fatalf("err = %v", err)
	}
	if len(sleeps) != 0 {
		t.Errorf("503 must not be retried; slept %v", sleeps)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestListParsesEnvelopeAndCursor(t *testing.T) {
	srv := apitest.NewServer(t)
	other := apitest.GenerateResourceKey(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "active" {
			t.Errorf("status param = %q", got)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{
			{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "target_url": "https://a.example",
				"type": "url", "status": "active",
				"description": "nightly export", "tags": []string{"ops", "nightly"},
			},
			{"resource_id": other.ResourceID, "crid": other.CRID,
				"target_url": "https://b.example", "type": "url", "status": "revoked"},
		}, map[string]any{"next_cursor": "cur2", "has_more": true})
	})
	client := newTestClient(t, srv, nil)

	page, err := client.List(context.Background(), ListOptions{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor != "cur2" || !page.HasMore {
		t.Errorf("page = %+v", page)
	}
	if page.Items[1].CRID != other.CRID || page.Items[1].ResourceID != other.ResourceID {
		t.Errorf("second row identity = %+v", page.Items[1])
	}
	// The publish-time metadata is what a sweeper recognizes a row by, so it
	// has to survive the projection rather than being dropped on the floor.
	if got := page.Items[0]; got.Type != "url" || got.Description != "nightly export" ||
		!slices.Equal(got.Tags, []string{"ops", "nightly"}) {
		t.Errorf("row metadata lost in projection: %+v", got)
	}
	// A row that carries none of it projects to zero values, never to
	// synthesized ones — an empty description is not a description.
	if got := page.Items[1]; got.Type != "url" || got.Description != "" || got.Tags != nil {
		t.Errorf("row without metadata gained some: %+v", got)
	}
}

func TestListRejectsURLWithoutCRID(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{
			"resource_id": srv.Key.ResourceID,
			"type":        "url",
			"status":      "active",
		}}, map[string]any{"has_more": false})
	})
	_, err := newTestClient(t, srv, nil).List(context.Background(), ListOptions{})
	if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("URL list without CRID = %v, want invalid API response", err)
	}
}

func TestResourceParsesURLDetailEnvelope(t *testing.T) {
	srv := apitest.NewServer(t)
	created := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	expires := created.Add(24 * time.Hour)
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"target_url": "https://aol.com", "type": "url", "status": "active",
			"created_at": created, "expires_at": expires,
		}}, nil)
	})
	client := newTestClient(t, srv, nil)

	resource, err := client.Resource(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	if resource.CRID != srv.Key.CRID || resource.ResourceID != srv.Key.ResourceID ||
		resource.TargetURL != "https://aol.com" || resource.Type != "url" || resource.Status != "active" ||
		resource.CreatedAt == nil || !resource.CreatedAt.Equal(created) ||
		resource.ExpiresAt == nil || !resource.ExpiresAt.Equal(expires) {
		t.Fatalf("resource detail projection = %+v", resource)
	}
}

func TestListRejectsMissingResourceID(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{
			"resource_id": " ", "target_url": "https://a.example", "type": "url", "status": "active",
		}}, map[string]any{"has_more": false})
	})
	client := newTestClient(t, srv, nil)

	if _, err := client.List(context.Background(), ListOptions{}); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("missing resource_id err = %v", err)
	}
}

func TestResourceRejectsDetailIdentityMismatch(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": "qnot-the-requested-crid",
			"target_url": "https://aol.com", "type": "url", "status": "active",
		}}, nil)
	})
	client := newTestClient(t, srv, nil)

	if _, err := client.Resource(context.Background(), srv.Key.CRID); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("identity-mismatched resource detail err = %v", err)
	}
}

func TestResourceDoesNotScanAfterUnstructuredNotFound(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	client := newTestClient(t, srv, nil)

	_, err := client.Resource(context.Background(), srv.Key.CRID)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("unstructured not-found err = %v", err)
	}
	if requests := srv.Requests(); len(requests) != 1 {
		t.Fatalf("unstructured not-found triggered list fallback: %#v", requests)
	}
}

func TestResourceDoesNotScanAfterStructuredNotFound(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, apitest.HandlerNotFound404(t, "not_found"))
	client := newTestClient(t, srv, nil)

	_, err := client.Resource(context.Background(), srv.Key.CRID)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("structured not-found err = %v", err)
	}
	if requests := srv.Requests(); len(requests) != 1 {
		t.Fatalf("structured not-found triggered list fallback: %#v", requests)
	}
}

func TestProblemErrorParsesPinnedEnvelope(t *testing.T) {
	var e *Error

	// The pinned envelope: error.{type,title,status,detail,instance,code} +
	// meta.request_id. code is the programmatic field.
	pinned := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"type":"https://api.layerv.ai/errors/revoked","title":"Resource Revoked","status":400,"detail":"prose that may change","instance":"/v1/x","code":"revoked"},"meta":{"request_id":"req_env"}}`)}).problem()
	if !errors.As(pinned, &e) || e.Code != "revoked" || e.Title != "Resource Revoked" || e.RequestID != "req_env" {
		t.Errorf("pinned envelope: %+v", e)
	}

	// The validation variant carries invalid_fields inside error; null must
	// be tolerated.
	validation := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"code":"invalid_request","title":"Bad Request","invalid_fields":{"target_url":"is required"}},"meta":{"request_id":"req_v"}}`)}).problem()
	if !errors.As(validation, &e) || e.InvalidFields["target_url"] != "is required" {
		t.Errorf("validation variant: %+v", e)
	}
	nullFields := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"error":{"code":"invalid_request","title":"Bad Request","invalid_fields":null},"meta":{"request_id":"req_n"}}`)}).problem()
	if !errors.As(nullFields, &e) || e.Code != "invalid_request" || len(e.InvalidFields) != 0 {
		t.Errorf("null invalid_fields must be tolerated: %+v", e)
	}

	// Flat fields stay accepted as an intermediary fallback.
	flat := (&restReply{status: 400, header: http.Header{},
		body: []byte(`{"code":"bad","title":"Bad","detail":"flat shape","request_id":"req_flat"}`)}).problem()
	if !errors.As(flat, &e) || e.Code != "bad" || e.Detail != "flat shape" || e.RequestID != "req_flat" {
		t.Errorf("flat shape: %+v", flat)
	}

	raw := (&restReply{status: 502, header: http.Header{}, body: []byte("<html>bad gateway</html>")}).problem()
	if !errors.As(raw, &e) || !strings.Contains(e.Detail, "bad gateway") {
		t.Errorf("raw body snippet: %+v", e)
	}

	retryHeader := http.Header{}
	retryHeader.Set("Retry-After", "7")
	limited := (&restReply{status: 429, header: retryHeader, body: []byte(`{}`)}).problem()
	if !errors.As(limited, &e) || e.RetryAfter != 7 {
		t.Errorf("retry-after: %+v", e)
	}
	retryHeader.Set("Retry-After", "3600")
	limited = (&restReply{status: 429, header: retryHeader, body: []byte(`{}`)}).problem()
	if !errors.As(limited, &e) || e.RetryAfter != 3600 {
		t.Errorf("full user-facing retry-after: %+v", e)
	}
}

func TestBodySnippetIsBoundedValidUTF8(t *testing.T) {
	input := strings.Repeat("a", maxSnippet-1) + "étail"
	got := bodySnippet([]byte(input))
	if !utf8.ValidString(got) {
		t.Fatalf("body snippet is not valid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", maxSnippet-1) + "..."; got != want {
		t.Fatalf("body snippet = %q, want %q", got, want)
	}
	if got := bodySnippet([]byte{'o', 'k', 0xff, 'x'}); !utf8.ValidString(got) || !strings.Contains(got, "\uFFFD") {
		t.Fatalf("invalid response bytes were not sanitized: %q", got)
	}
	got = bodySnippet([]byte("bad\x1b[31m\x7f\u202Eresponse"))
	if !strings.Contains(got, "\uFFFD") {
		t.Fatalf("response controls were not replaced: %q", got)
	}
	for _, r := range got {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			t.Fatalf("response snippet retained control rune %U: %q", r, got)
		}
	}
}

// TestMeFixtureStillSendsKeyPrefix pins the premise of the whoami goldens:
// the mock /v1/me still sends key_prefix, as the platform does, so a golden
// without it proves the CLI drops it. Deleting the fixture field as dead data
// would otherwise leave the goldens green with no guard behind them.
func TestMeFixtureStillSendsKeyPrefix(t *testing.T) {
	srv := apitest.NewServer(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/me", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer lv_test_fixture_prefix_guard")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Data struct {
			APIKey struct {
				KeyPrefix string `json:"key_prefix"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Data.APIKey.KeyPrefix != "lv_test_fixt" {
		t.Fatalf("mock /v1/me key_prefix = %q, want the bearer's first 12 chars", env.Data.APIKey.KeyPrefix)
	}
}

// TestMeParsesIdentityEnvelope pins the GET /v1/me success contract: the
// envelope decodes into the repo-owned Identity and the CLI headers ride the
// same shared transport.
func TestMeParsesIdentityEnvelope(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	id, err := client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.OwnerID != apitest.MeOwnerID || id.AuthType != "api_key" {
		t.Errorf("identity = %+v", id)
	}
	if id.Key == nil {
		t.Fatal("api_key block must project into Key")
	}
	if id.Key.KeyID != apitest.MeKeyID || id.Key.Kind != "api_key" {
		t.Errorf("key identity = %+v", id.Key)
	}
	if want := []string{"qurl:read", "qurl:resolve", "qurl:write"}; !slices.Equal(id.Key.Scopes, want) {
		t.Errorf("scopes = %v, want the platform's alphabetical %v", id.Key.Scopes, want)
	}
	if id.Key.ExpiresAt != nil {
		t.Errorf("non-expiring fixture must project a nil expiry, got %v", id.Key.ExpiresAt)
	}

	req := srv.Requests()[0]
	if req.Method != http.MethodGet || req.Path != "/v1/me" {
		t.Errorf("request = %s %s, want GET /v1/me", req.Method, req.Path)
	}
	if req.Header.Get("User-Agent") != "qurl-cli/test" || req.Header.Get("X-Request-Id") != testRequestID {
		t.Errorf("identity call missing the CLI headers: %+v", req.Header)
	}
}

// TestMeIdentityVariants covers the envelope's optional parts: an expiring
// key carries its expiry; a response without the api_key block (non-key
// authentication) projects a nil Key; a response missing owner_id is outside
// the contract.
func TestMeIdentityVariants(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"owner_id":  "own_expiring",
			"auth_type": "api_key",
			"api_key": map[string]any{
				"key_id":     "key_expiring0001",
				"kind":       "api_key",
				"scopes":     []string{"qurl:read"},
				"expires_at": "2026-03-15T00:00:00Z",
			},
		}, nil)
	})
	id, err := client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Key == nil || id.Key.ExpiresAt == nil || !id.Key.ExpiresAt.Equal(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expiring key projection = %+v", id.Key)
	}

	srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"owner_id":  "own_jwt",
			"auth_type": "jwt",
		}, nil)
	})
	id, err = client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.OwnerID != "own_jwt" || id.Key != nil {
		t.Errorf("keyless identity = %+v", id)
	}

	srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"auth_type": "api_key"}, nil)
	})
	if _, err := client.Me(context.Background()); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Errorf("missing owner_id: err = %v, want ErrInvalidAPIResponse", err)
	}
}

// TestMeFailureCodes pins each platform failure the login/whoami flows key
// on: the typed Error carries the status and problem code for 401
// api_key_invalid, 401 api_key_expired, 403 insufficient_scope, and 403
// account_frozen.
func TestMeFailureCodes(t *testing.T) {
	cases := map[string]struct {
		handler    http.HandlerFunc
		wantStatus int
		wantCode   string
	}{
		"invalid": {apitest.HandlerAPIKeyInvalid401(t), http.StatusUnauthorized, "api_key_invalid"},
		"expired": {apitest.HandlerAPIKeyExpired401(t), http.StatusUnauthorized, "api_key_expired"},
		"scope":   {apitest.HandlerInsufficientScope403(t), http.StatusForbidden, "insufficient_scope"},
		"frozen":  {apitest.HandlerAccountFrozen403(t), http.StatusForbidden, "account_frozen"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/me", tc.handler)
			client := newTestClient(t, srv, nil)

			_, err := client.Me(context.Background())
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want the typed Error", err)
			}
			if apiErr.StatusCode != tc.wantStatus || apiErr.Code != tc.wantCode {
				t.Errorf("got HTTP %d %q, want HTTP %d %q", apiErr.StatusCode, apiErr.Code, tc.wantStatus, tc.wantCode)
			}
			if apiErr.RequestID == "" {
				t.Error("failure must carry the request id for support")
			}
		})
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "alphanumeric API key and bearer",
			in:   "key lv_live_abc123DEF456ghi789jkl and Bearer lv_test_zzz999yyy888xxx777www here",
			want: "key lv_*** and Bearer *** here",
		},
		{
			name: "complete URL-safe API key alphabet",
			in:   "key lv_live_Ab3-QRSTUVWXYZ0123456789_abcdefghijklmno here",
			want: "key lv_*** here",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Redact(test.in); got != test.want {
				t.Fatalf("Redact() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	_, err := New(&Config{APIKey: "lv_test_apitestingvalue123456789"})
	if !errors.Is(err, qurl.ErrInvalidClientConfig) {
		t.Errorf("err = %v, want ErrInvalidClientConfig", err)
	}
}

// TestShareAlwaysCarriesACredential pins the client seam under share. This
// package builds a client two ways, New from an account key and NewRegistered
// from device state, and neither returns one without a usable credential, so
// nothing here can send a share request without one. A client that either
// does build sends its credential with the share request.
func TestShareAlwaysCarriesACredential(t *testing.T) {
	srv := apitest.NewServer(t)

	for _, key := range []string{"", "   "} {
		if client, err := New(&Config{BaseURL: srv.URL, APIKey: key, Version: "test"}); client != nil || !errors.Is(err, qurl.ErrInvalidClientConfig) {
			t.Fatalf("New with credential %q = %v, %v; want ErrInvalidClientConfig", key, client, err)
		}
	}
	for _, tc := range []struct {
		name  string
		state func() *qurl.AgentState
	}{
		{"no device state", func() *qurl.AgentState { return nil }},
		{"no device credential", func() *qurl.AgentState {
			state := registeredAPIState(t)
			state.DeviceAPIKey = ""
			return state
		}},
		{"blank device credential", func() *qurl.AgentState {
			state := registeredAPIState(t)
			state.DeviceAPIKey = "   "
			return state
		}},
	} {
		client, err := NewRegistered(context.Background(), &Config{
			BaseURL: srv.URL, Version: "test", HTTPClient: srv.Client(),
		}, &registeredAPIStateStore{state: tc.state()})
		if client != nil || err == nil {
			t.Fatalf("NewRegistered with %s = %v, %v; want no client", tc.name, client, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Fatalf("refusing to build a client sent %d requests, want none", got)
	}

	for _, built := range []struct {
		name   string
		client Client
		want   string
	}{
		{"account key", newTestClient(t, srv, nil), "Bearer lv_test_apitestingvalue123456789"},
		{"device state", newRegisteredTestClient(t, srv), "Bearer " + registeredAPIState(t).DeviceAPIKey},
	} {
		if _, err := built.client.Share(context.Background(), srv.Key.CRID, ShareOptions{}); err != nil {
			t.Fatalf("Share with a client built from %s: %v", built.name, err)
		}
		requests := srv.Requests()
		last := requests[len(requests)-1]
		if last.Path != "/v1/resources/"+srv.Key.CRID+"/share" || last.Header.Get("Authorization") != built.want {
			t.Fatalf("share from a client built from %s = %s %s with %d Authorization bytes, want its own credential",
				built.name, last.Method, last.Path, len(last.Header.Get("Authorization")))
		}
	}
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("requests = %d, want exactly the two share requests", got)
	}
}

// TestShareMarksOnlyItsOwnNotFound pins which errors carry share's not-found
// marker: a 404 from the share operator, whatever its problem code, and
// nothing else. The owner-truthful answers share can also give keep their own
// codes, and another route's 404 keeps the guidance every route shares.
func TestShareMarksOnlyItsOwnNotFound(t *testing.T) {
	srv := apitest.NewServer(t)
	shareRoute := "/v1/resources/" + srv.Key.CRID + "/share"
	client := newTestClient(t, srv, nil)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		status  int
		want    bool
	}{
		{"share not-found code", apitest.HandlerNotFound404(t, "resource_not_found"), http.StatusNotFound, true},
		{"generic not-found code", apitest.HandlerNotFound404(t, "not_found"), http.StatusNotFound, true},
		{"no problem body", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, http.StatusNotFound, true},
		{"deleted by its owner", apitest.HandlerRevoked400(t), http.StatusBadRequest, false},
		{"retired", apitest.HandlerTombstoned410(t), http.StatusGone, false},
		{"missing scope", apitest.HandlerInsufficientScope403(t), http.StatusForbidden, false},
		{"stopped", apitest.HandlerConnectorStopped503(t), http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.Script(http.MethodPost, shareRoute, tc.handler)
			_, err := client.Share(context.Background(), srv.Key.CRID, ShareOptions{})
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
				t.Fatalf("Share error = %v, want a typed HTTP %d", err, tc.status)
			}
			if got := apiErr.ShareNotFound(); got != tc.want {
				t.Fatalf("ShareNotFound() = %t for HTTP %d %q, want %t", got, apiErr.StatusCode, apiErr.Code, tc.want)
			}
		})
	}

	t.Run("another route", func(t *testing.T) {
		srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", apitest.HandlerNotFound404(t, "not_found"))
		_, err := client.Sharing(context.Background(), srv.Key.CRID)
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("Sharing error = %v, want a typed HTTP 404", err)
		}
		if apiErr.ShareNotFound() {
			t.Fatal("a 404 from a route other than share carries share's not-found marker")
		}
	})

	if (*Error)(nil).ShareNotFound() {
		t.Fatal("a nil error reports share's not-found marker")
	}
}

// TestPublishStatesPrivacyInEveryCreateRequest pins the wire rule behind the
// private default: the create request always carries the private member, for
// a private resource and for a public one, for a URL and for a Connector. A
// request that left it out would get whatever the service does by default,
// and an older service makes a public resource.
func TestPublishStatesPrivacyInEveryCreateRequest(t *testing.T) {
	for _, connectorID := range []string{"", "stated-connector"} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("connector=%q/public=%t", connectorID, public), func(t *testing.T) {
				srv := apitest.NewServer(t)
				var sent map[string]json.RawMessage
				srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Error(err)
					}
					apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": !public}, nil)
				})
				target := "https://example.com"
				if connectorID != "" {
					target = ""
				}
				result, err := newTestClient(t, srv, nil).Publish(t.Context(), target, PublishOptions{Public: public, ConnectorID: connectorID})
				if err != nil {
					t.Fatal(err)
				}
				want := "true"
				if public {
					want = "false"
				}
				stated, present := sent["private"]
				if !present || string(stated) != want {
					t.Fatalf("create request stated private = %q (present %t), want %s; body members %v", stated, present, want, sent)
				}
				if result.Private == nil || *result.Private == public {
					t.Fatalf("result privacy = %v, want the confirmed %t", result.Private, !public)
				}
			})
		}
	}
}

func TestPublishSendsTheFirstDeviceList(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Private bool     `json:"private"`
			Allowed []string `json:"allowed_device_keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !body.Private || !slices.Equal(body.Allowed, []string{"recipient-public-key"}) {
			t.Errorf("privacy lost: %+v", body)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "allowed_device_keys": body.Allowed}, nil)
	})
	if _, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com", PublishOptions{AllowedDeviceKeys: []string{"recipient-public-key"}}); err != nil {
		t.Fatal(err)
	}
}

// TestPublishRequiresTheAnswerToConfirmPrivacy pins the check on the create
// answer, in both directions: the row must carry the privacy that was asked
// for. A row with the other privacy, and a row that does not say, both fail
// with no result, so no caller can print a CRID for them.
func TestPublishRequiresTheAnswerToConfirmPrivacy(t *testing.T) {
	for _, connectorID := range []string{"", "confirmed-connector"} {
		for _, public := range []bool{false, true} {
			for _, answered := range []any{nil, false, true} {
				t.Run(fmt.Sprintf("connector=%q/public=%t/answered=%v", connectorID, public, answered), func(t *testing.T) {
					srv := apitest.NewServer(t)
					srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
						data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID}
						if answered != nil {
							data["private"] = answered
						}
						apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": true})
					})
					target := "https://example.com"
					if connectorID != "" {
						target = ""
					}
					result, err := newTestClient(t, srv, nil).Publish(t.Context(), target, PublishOptions{Public: public, ConnectorID: connectorID})
					if answered == !public {
						if err != nil || result.Private == nil || *result.Private == public {
							t.Fatalf("confirmed publish: result %+v, error %v", result, err)
						}
						return
					}
					if !errors.Is(err, qurl.ErrInvalidAPIResponse) || result != nil {
						t.Fatalf("unconfirmed publish returned %+v, error %v; want no result and an invalid-response error", result, err)
					}
					want := msgPrivateUnconfirmed
					if public {
						want = msgPublicUnconfirmed
					}
					var shown interface{ UserMessage() string }
					if !errors.As(err, &shown) || shown.UserMessage() != want || err.Error() != want {
						t.Fatalf("unconfirmed publish error = %q, want the message %q", err, want)
					}
				})
			}
		}
	}
}

// TestPublishDoesNotDependOnTheServiceDefault runs the same two publishes
// against a service whose default is private and one whose default is public.
// Each gets the privacy it asked for from both, because the request states
// it. If the request left the field out, one of the four would get the other
// privacy and fail the confirmation.
func TestPublishDoesNotDependOnTheServiceDefault(t *testing.T) {
	for _, publicByDefault := range []bool{false, true} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("service_default_public=%t/public=%t", publicByDefault, public), func(t *testing.T) {
				srv := apitest.NewServer(t)
				if publicByDefault {
					srv.PlayPublicByDefault()
				}
				result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{Public: public})
				if err != nil {
					t.Fatalf("Publish: %v", err)
				}
				if result.Private == nil || *result.Private == public {
					t.Fatalf("result privacy = %v, want %t", result.Private, !public)
				}
			})
		}
	}
}

// TestPublishAccessConflict pins how a publish refused because the target is
// already published with other access settings is recognized, and what the
// conflict then says. The privacy-mismatch code means the existing resource
// has the other privacy, and the device-list code means its list is another
// one. The detail an older service sends covers both and says neither, so it
// names nothing, whatever the request carried. Any other refusal stays the
// plain service problem, including an invalid-input answer with some other
// wording about device keys: only the older text is ever matched.
func TestPublishAccessConflict(t *testing.T) {
	const key = "recipient-public-key"
	for _, test := range []struct {
		name   string
		status int
		code   string
		detail string
		opts   PublishOptions
		want   ExistingAccess
		plain  bool
	}{
		{name: "code, private asked", status: 400, code: apitest.CodePrivacyMismatch, detail: "d", want: ExistingAccessPublic},
		{name: "code, public asked", status: 400, code: apitest.CodePrivacyMismatch, detail: "d", opts: PublishOptions{Public: true}, want: ExistingAccessPrivate},
		{name: "code, with a device list", status: 400, code: apitest.CodePrivacyMismatch, detail: "d", opts: PublishOptions{AllowedDeviceKeys: []string{key}}, want: ExistingAccessPublic},
		{name: "code in another case", status: 400, code: "PRIVACY_MISMATCH", detail: "d", want: ExistingAccessPublic},
		{name: "device code", status: 400, code: apitest.CodeDeviceKeysMismatch, detail: "d", opts: PublishOptions{AllowedDeviceKeys: []string{key}}, want: ExistingAccessOtherDevices},
		{name: "device code in another case", status: 400, code: "Device_Keys_Mismatch", detail: "d", opts: PublishOptions{AllowedDeviceKeys: []string{key}}, want: ExistingAccessOtherDevices},
		{name: "device code with the older detail", status: 400, code: apitest.CodeDeviceKeysMismatch, detail: apitest.LegacyAccessSettingsDetail, opts: PublishOptions{AllowedDeviceKeys: []string{key}}, want: ExistingAccessOtherDevices},
		{name: "older detail, private asked", status: 400, code: "invalid_input", detail: apitest.LegacyAccessSettingsDetail, want: ExistingAccessUnknown},
		{name: "older detail, public asked", status: 400, code: "invalid_input", detail: apitest.LegacyAccessSettingsDetail, opts: PublishOptions{Public: true}, want: ExistingAccessUnknown},
		{name: "older detail, with a device list", status: 400, code: "invalid_input", detail: apitest.LegacyAccessSettingsDetail, opts: PublishOptions{AllowedDeviceKeys: []string{key}}, want: ExistingAccessUnknown},
		{name: "another invalid input", status: 400, code: "invalid_input", detail: "target_url is not allowed", plain: true},
		{name: "another wording about device keys", status: 400, code: "invalid_input", detail: "allowed_device_keys differ from the device keys this resource has", opts: PublishOptions{AllowedDeviceKeys: []string{key}}, plain: true},
		{name: "the code on another status", status: 409, code: apitest.CodePrivacyMismatch, detail: "d", plain: true},
		{name: "the device code on another status", status: 409, code: apitest.CodeDeviceKeysMismatch, detail: "d", plain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, test.status, test.code, "Refused", test.detail)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", test.opts)
			if err == nil || result != nil {
				t.Fatalf("refused publish returned %+v, error %v", result, err)
			}
			var problem *Error
			if !errors.As(err, &problem) || problem.StatusCode != test.status || problem.RequestID != "req_test" {
				t.Fatalf("the service problem is not in the chain with its request id: %v", err)
			}
			var conflict *PublishAccessConflictError
			if test.plain {
				if errors.As(err, &conflict) || errors.Is(err, ErrPublishAccessConflict) {
					t.Fatalf("an unrelated refusal was read as an access conflict: %v", err)
				}
				return
			}
			if !errors.As(err, &conflict) || !errors.Is(err, ErrPublishAccessConflict) {
				t.Fatalf("error = %v, want an access conflict", err)
			}
			if conflict.Existing != test.want {
				t.Fatalf("existing access = %d, want %d", conflict.Existing, test.want)
			}
		})
	}
}

// TestPublishAccessConflictSaysWhatExists pins the four sentences, each to
// its case, so a refusal can never be shown with another one's text.
func TestPublishAccessConflictSaysWhatExists(t *testing.T) {
	for existing, want := range map[ExistingAccess]string{
		ExistingAccessPublic:       msgPublishExistingPublic,
		ExistingAccessPrivate:      msgPublishExistingPrivate,
		ExistingAccessOtherDevices: msgPublishOtherDevices,
		ExistingAccessUnknown:      msgPublishAccessDiffers,
		ExistingAccess(99):         msgPublishAccessDiffers,
	} {
		if got := (&PublishAccessConflictError{Existing: existing}).Error(); got != want {
			t.Errorf("existing %d: message %q, want %q", existing, got, want)
		}
	}
	if !strings.Contains(msgPublishExistingPublic, "as public") || !strings.Contains(msgPublishExistingPrivate, "as private") {
		t.Fatal("the two definite messages do not name the privacy that exists")
	}
	// The other two say nothing definite about privacy: the service did not.
	for _, message := range []string{msgPublishOtherDevices, msgPublishAccessDiffers} {
		if strings.Contains(message, "as public") || strings.Contains(message, "as private") {
			t.Fatalf("a message for a conflict that does not name the privacy states one: %q", message)
		}
	}
	if !errors.Is(&PublishAccessConflictError{}, ErrPublishAccessConflict) {
		t.Fatal("a conflict without a service problem does not match its sentinel")
	}
}

func TestDeviceGrantsRequireConfirmation(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		srv := apitest.NewServer(t)
		path := "/v1/resources"
		if method == http.MethodPatch {
			path += "/" + srv.Key.CRID
		}
		srv.Script(method, path, func(w http.ResponseWriter, _ *http.Request) {
			status := http.StatusOK
			if method == http.MethodPost {
				status = http.StatusCreated
			}
			apitest.WriteEnvelope(t, w, status, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "type": "url", "status": "active", "allowed_device_keys": []string{}}, nil)
		})
		client := newTestClient(t, srv, nil)
		var err error
		if method == http.MethodPost {
			_, err = client.Publish(t.Context(), "https://example.com", PublishOptions{AllowedDeviceKeys: []string{"requested-key"}})
		} else {
			_, err = client.SetDeviceGrants(t.Context(), srv.Key.CRID, []string{"requested-key"})
		}
		if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatalf("missing grants accepted for %s: %v", method, err)
		}
	}
}

// TestEditDeviceGrantsSendsOnlyTheEdit pins the wire shape of a change to
// single device keys: one PATCH with the keys to add and the keys to remove.
// It never carries allowed_device_keys, the member that replaces the complete
// list, and it omits an empty side instead of sending an empty array.
func TestEditDeviceGrantsSendsOnlyTheEdit(t *testing.T) {
	for _, test := range []struct {
		name        string
		add, remove []string
		want        string
	}{
		{name: "add", add: []string{"a", "b"}, want: `{"allowed_device_keys_add":["a","b"]}`},
		{name: "remove", remove: []string{"c"}, want: `{"allowed_device_keys_remove":["c"]}`},
		{name: "both", add: []string{"a"}, remove: []string{"c"}, want: `{"allowed_device_keys_add":["a"],"allowed_device_keys_remove":["c"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetResourceAccess(true, "c", "d")
			resource, err := newTestClient(t, srv, nil).EditDeviceGrants(t.Context(), srv.Key.CRID, test.add, test.remove)
			if err != nil {
				t.Fatalf("EditDeviceGrants: %v", err)
			}
			requests := srv.Requests()
			if len(requests) != 1 || requests[0].Method != http.MethodPatch || requests[0].Path != "/v1/resources/"+srv.Key.CRID {
				t.Fatalf("requests = %+v, want one PATCH of the resource", requests)
			}
			if got := string(requests[0].Body); got != test.want {
				t.Fatalf("body = %s, want %s", got, test.want)
			}
			if requests[0].Header.Get("Authorization") == "" {
				t.Fatal("the grant change carries no credential")
			}
			for _, key := range test.add {
				if !slices.Contains(resource.AllowedDeviceKeys, key) {
					t.Fatalf("resulting list %v lacks the added key %q", resource.AllowedDeviceKeys, key)
				}
			}
			for _, key := range test.remove {
				if slices.Contains(resource.AllowedDeviceKeys, key) {
					t.Fatalf("resulting list %v still has the removed key %q", resource.AllowedDeviceKeys, key)
				}
			}
			if !slices.Contains(resource.AllowedDeviceKeys, "d") {
				t.Fatalf("a key the change did not name was lost: %v", resource.AllowedDeviceKeys)
			}
		})
	}
}

// TestEditDeviceGrantsRequiresTheAnswerToShowTheChange pins the check on the
// answer: every added key on the returned list, no removed key on it. An
// older service that ignores the request and returns the list as it was
// fails that check, with the message that names the cause.
func TestEditDeviceGrantsRequiresTheAnswerToShowTheChange(t *testing.T) {
	for _, test := range []struct {
		name        string
		add, remove []string
		answered    []string
		wantErr     bool
	}{
		{name: "added", add: []string{"a"}, answered: []string{"x", "a"}},
		{name: "removed", remove: []string{"x"}, answered: []string{"a"}},
		{name: "both", add: []string{"a"}, remove: []string{"x"}, answered: []string{"a"}},
		{name: "removed to an empty list", remove: []string{"x"}},
		{name: "list unchanged after add", add: []string{"a"}, answered: []string{"x"}, wantErr: true},
		{name: "list unchanged after remove", remove: []string{"x"}, answered: []string{"x"}, wantErr: true},
		{name: "second added key missing", add: []string{"a", "b"}, answered: []string{"a"}, wantErr: true},
		{name: "added but not removed", add: []string{"a"}, remove: []string{"x"}, answered: []string{"x", "a"}, wantErr: true},
		{name: "removed but not added", add: []string{"a"}, remove: []string{"x"}, wantErr: true},
		{name: "no list after add", add: []string{"a"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "type": "url", "status": "active"}
				if test.answered != nil {
					data["allowed_device_keys"] = test.answered
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
			})
			resource, err := newTestClient(t, srv, nil).EditDeviceGrants(t.Context(), srv.Key.CRID, test.add, test.remove)
			if !test.wantErr {
				if err != nil || !slices.Equal(resource.AllowedDeviceKeys, test.answered) {
					t.Fatalf("EditDeviceGrants = %+v, %v; want the answered list %v", resource, err, test.answered)
				}
				return
			}
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) || resource != nil {
				t.Fatalf("an answer that does not show the change returned %+v, %v", resource, err)
			}
			var shown interface{ UserMessage() string }
			if !errors.As(err, &shown) || shown.UserMessage() != msgGrantEditUnconfirmed || !strings.Contains(err.Error(), "this service cannot add or remove single device grants yet") {
				t.Fatalf("error = %q, want the message that names the cause", err)
			}
		})
	}

	// The older service itself: it ignores the edit and answers 200.
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, "x")
	srv.PlayNoSingleGrantEdits()
	if _, err := newTestClient(t, srv, nil).EditDeviceGrants(t.Context(), srv.Key.CRID, []string{"a"}, nil); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("an ignored edit was reported as made: %v", err)
	}
}

// TestEditDeviceGrantsRefusesAnEditThatCannotBeOne pins the local refusals:
// nothing to change, and one key on both sides. Neither sends a request.
func TestEditDeviceGrantsRefusesAnEditThatCannotBeOne(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)
	for name, edit := range map[string][2][]string{
		"nothing":             {nil, nil},
		"empty lists":         {{}, {}},
		"a key on both sides": {{"a", "b"}, {"b"}},
	} {
		if _, err := client.EditDeviceGrants(t.Context(), srv.Key.CRID, edit[0], edit[1]); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("%s: error = %v, want an invalid-request refusal", name, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Fatalf("a refused edit sent %d requests", got)
	}
}

// TestEditDeviceGrantsShowsTheServiceRefusal pins that a refused edit is the
// service's problem, not a result.
func TestEditDeviceGrantsShowsTheServiceRefusal(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "at most 256 allowed device keys")
	})
	resource, err := newTestClient(t, srv, nil).EditDeviceGrants(t.Context(), srv.Key.CRID, []string{"a"}, nil)
	var problem *Error
	if resource != nil || !errors.As(err, &problem) || problem.StatusCode != http.StatusBadRequest || problem.Detail != "at most 256 allowed device keys" {
		t.Fatalf("refused edit = %+v, %v", resource, err)
	}
}
