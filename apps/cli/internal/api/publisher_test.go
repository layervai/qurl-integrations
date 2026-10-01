package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// TestPublisherWireFailsClosed pins the one decode rule every resource read
// shares: nothing but an exact, single, literal `"verified": true` verifies,
// and no shape of the publisher member can fail the read that carries it.
func TestPublisherWireFailsClosed(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		row  string
		want Publisher
	}{
		"absent":                  {row: `{}`},
		"null object":             {row: `{"publisher":null}`},
		"string":                  {row: `{"publisher":"Acme Docs"}`},
		"array":                   {row: `{"publisher":[{"name":"Acme Docs","verified":true}]}`},
		"number":                  {row: `{"publisher":1}`},
		"boolean":                 {row: `{"publisher":true}`},
		"empty object":            {row: `{"publisher":{}}`},
		"name only":               {row: `{"publisher":{"name":"Acme Docs"}}`, want: Publisher{Name: "Acme Docs"}},
		"null verified":           {row: `{"publisher":{"name":"Acme Docs","verified":null}}`, want: Publisher{Name: "Acme Docs"}},
		"string verified":         {row: `{"publisher":{"name":"Acme Docs","verified":"true"}}`, want: Publisher{Name: "Acme Docs"}},
		"numeric verified":        {row: `{"publisher":{"name":"Acme Docs","verified":1}}`, want: Publisher{Name: "Acme Docs"}},
		"object verified":         {row: `{"publisher":{"verified":{"value":true}}}`},
		"array verified":          {row: `{"publisher":{"verified":[true]}}`},
		"capitalized key":         {row: `{"publisher":{"name":"Acme Docs","Verified":true}}`, want: Publisher{Name: "Acme Docs"}},
		"uppercase key":           {row: `{"publisher":{"VERIFIED":true}}`},
		"renamed key":             {row: `{"publisher":{"is_verified":true,"verified_at":"2026-03-01T00:00:00Z"}}`},
		"repeated true":           {row: `{"publisher":{"verified":true,"verified":true}}`},
		"false then true":         {row: `{"publisher":{"verified":false,"verified":true}}`},
		"true then false":         {row: `{"publisher":{"verified":true,"verified":false}}`},
		"verified beside":         {row: `{"verified":true,"publisher":{"name":"Acme Docs"}}`, want: Publisher{Name: "Acme Docs"}},
		"verified beside, absent": {row: `{"verified":true,"publisher_verified":true}`},
		"numeric name":            {row: `{"publisher":{"name":7,"verified":false}}`},
		// A repeated name is ambiguous: no occurrence wins, and the status
		// beside it is unaffected.
		"repeated name":           {row: `{"publisher":{"name":"Acme Docs","name":"LayerV Security"}}`},
		"repeated same name":      {row: `{"publisher":{"name":"Acme Docs","name":"Acme Docs"}}`},
		"name, then garbled name": {row: `{"publisher":{"name":"Acme Docs","name":7}}`},
		"garbled name, then name": {row: `{"publisher":{"name":null,"name":"Acme Docs"}}`},
		"repeated name, verified": {row: `{"publisher":{"name":"Acme Docs","verified":true,"name":"LayerV Security"}}`, want: Publisher{Verified: true}},
		"null name":               {row: `{"publisher":{"name":null,"verified":false}}`},
		"uppercase name key":      {row: `{"publisher":{"NAME":"Acme Docs"}}`},
		"explicit false":          {row: `{"publisher":{"name":"Acme Docs","verified":false}}`, want: Publisher{Name: "Acme Docs"}},
		"explicit true":           {row: `{"publisher":{"name":"Acme Docs","verified":true}}`, want: Publisher{Name: "Acme Docs", Verified: true}},
		"spaced true":             {row: `{"publisher":{"verified" : true }}`, want: Publisher{Verified: true}},
		// A repeated publisher member is ambiguous: no occurrence wins.
		"repeated, last verified":    {row: `{"publisher":{"verified":false},"publisher":{"verified":true}}`},
		"repeated, first verified":   {row: `{"publisher":{"name":"Acme Docs","verified":true},"publisher":{"verified":false}}`},
		"repeated, both verified":    {row: `{"publisher":{"name":"Acme Docs","verified":true},"publisher":{"name":"Acme Docs","verified":true}}`},
		"repeated three times":       {row: `{"publisher":{"verified":true},"publisher":null,"publisher":{"name":"Acme Docs","verified":true}}`},
		"repeated around a neighbor": {row: `{"publisher":{"verified":true},"crid":"x","publisher":{"name":"Acme Docs","verified":true}}`},
		"unknown members ignored":    {row: `{"publisher":{"name":"Acme Docs","verified":true,"badge":"gold"}}`, want: Publisher{Name: "Acme Docs", Verified: true}},
		// Only the exact, lowercase member is the publisher. encoding/json
		// would match these keys to the field without regard to case.
		"capitalized member":     {row: `{"Publisher":{"name":"Acme Docs","verified":true}}`},
		"uppercase member":       {row: `{"PUBLISHER":{"verified":true}}`},
		"exact, then other case": {row: `{"publisher":{"name":"Acme Docs","verified":false},"Publisher":{"name":"Acme Docs","verified":true}}`, want: Publisher{Name: "Acme Docs"}},
		"other case, then exact": {row: `{"Publisher":{"name":"Acme Docs","verified":true},"publisher":{"name":"Acme Docs","verified":false}}`, want: Publisher{Name: "Acme Docs"}},
		// The count is per object: a publisher member of a nested object is
		// not a repetition of the row's own.
		"nested object has its own": {row: `{"publisher":{"name":"Acme Docs","verified":true},"links":{"publisher":{"verified":false}}}`, want: Publisher{Name: "Acme Docs", Verified: true}},
		"only a nested one":         {row: `{"links":{"publisher":{"name":"Acme Docs","verified":true}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var row resourceRow
			if err := json.Unmarshal([]byte(test.row), &row); err != nil {
				t.Fatalf("publisher metadata failed the row decode: %v", err)
			}
			if got := row.Publisher.publisher(); got != test.want {
				t.Fatalf("publisher = %+v, want %+v", got, test.want)
			}
		})
	}
}

// A repeated publisher member is counted within one JSON object, not over
// the lifetime of a Go value. A row that is decoded into again reads exactly
// what its latest object says: a repetition in one object does not blank the
// next, and nothing of an earlier object, verified or not, is left behind.
func TestResourceRowReadsOnlyItsLatestObject(t *testing.T) {
	t.Parallel()
	const (
		verified = `{"crid":"first","publisher":{"name":"Acme Docs","verified":true}}`
		repeated = `{"publisher":{"name":"Acme Docs","verified":true},"publisher":{"name":"Acme Docs","verified":true}}`
		silent   = `{"description":"no publisher member"}`
	)
	want := Publisher{Name: "Acme Docs", Verified: true}

	t.Run("one value decoded into repeatedly", func(t *testing.T) {
		t.Parallel()
		var row resourceRow
		for step, test := range []struct {
			object string
			want   Publisher
		}{
			{object: verified, want: want},
			{object: repeated},
			{object: verified, want: want},
			{object: silent},
			{object: `null`},
			{object: verified, want: want},
		} {
			if err := json.Unmarshal([]byte(test.object), &row); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			if got := row.Publisher.publisher(); got != test.want {
				t.Fatalf("step %d: publisher after %s = %+v, want %+v", step, test.object, got, test.want)
			}
		}
		if err := json.Unmarshal([]byte(silent), &row); err != nil {
			t.Fatal(err)
		}
		if row.CRID != "" || row.Description != "no publisher member" {
			t.Fatalf("row after a later object = %+v, want only that object's fields", row)
		}
	})

	t.Run("one value across a stream", func(t *testing.T) {
		t.Parallel()
		decoder := json.NewDecoder(strings.NewReader(repeated + verified + silent + verified))
		var row resourceRow
		for step, test := range []Publisher{{}, want, {}, want} {
			if err := decoder.Decode(&row); err != nil {
				t.Fatalf("object %d: %v", step, err)
			}
			if got := row.Publisher.publisher(); got != test {
				t.Fatalf("object %d: publisher = %+v, want %+v", step, got, test)
			}
		}
	})

	t.Run("a list decoded into a reused slice", func(t *testing.T) {
		t.Parallel()
		rows := make([]resourceRow, 2, 4)
		for _, list := range []string{
			"[" + verified + "," + verified + "]",
			"[" + repeated + "," + verified + "," + silent + "]",
		} {
			if err := json.Unmarshal([]byte(list), &rows); err != nil {
				t.Fatal(err)
			}
		}
		if len(rows) != 3 {
			t.Fatalf("decoded %d rows, want 3", len(rows))
		}
		for i, test := range []Publisher{{}, want, {}} {
			if got := rows[i].Publisher.publisher(); got != test {
				t.Fatalf("row %d: publisher = %+v, want %+v", i, got, test)
			}
		}
	})

	// Publisher metadata never fails a row, but the row's own fields are
	// still held to their types.
	t.Run("a malformed row is still an error", func(t *testing.T) {
		t.Parallel()
		var row resourceRow
		if err := json.Unmarshal([]byte(`{"resource_id":7,"publisher":{"verified":true}}`), &row); err == nil {
			t.Fatalf("a numeric resource_id decoded into %+v", row)
		}
	})
}

// The readers are handed bytes an outer decoder already validated, but they
// do not depend on that: an object that is cut short, or followed by anything
// else, reads as unnamed and unverified rather than as whatever its members
// said before the input went wrong.
func TestPublisherReadersNeedOneCompleteObject(t *testing.T) {
	t.Parallel()
	const whole = `{"name":"Acme Docs","verified":true}`
	if got := parsePublisherWire([]byte(whole)).publisher(); got != (Publisher{Name: "Acme Docs", Verified: true}) {
		t.Fatalf("a complete object = %+v", got)
	}
	if got := publisherMember([]byte(`{"publisher":` + whole + `}`)).publisher(); got != (Publisher{Name: "Acme Docs", Verified: true}) {
		t.Fatalf("a complete row = %+v", got)
	}
	for name, object := range map[string]string{
		"empty":             ``,
		"no closing brace":  `{"name":"Acme Docs","verified":true`,
		"cut after a comma": `{"name":"Acme Docs","verified":true,`,
		"cut in a value":    `{"verified":true,"name":"Acme`,
		"trailing value":    whole + ` true`,
		"trailing object":   whole + whole,
		"trailing brace":    whole + `}`,
		"wrapped in array":  `[` + whole + `]`,
	} {
		if got := parsePublisherWire([]byte(object)).publisher(); got != (Publisher{}) {
			t.Errorf("%s: publisher object = %+v, want the unverified zero value", name, got)
		}
	}
	for name, row := range map[string]string{
		"no closing brace": `{"publisher":` + whole,
		"trailing value":   `{"publisher":` + whole + `} true`,
		"trailing row":     `{"publisher":` + whole + `}{"publisher":` + whole + `}`,
	} {
		if got := publisherMember([]byte(row)).publisher(); got != (Publisher{}) {
			t.Errorf("%s: row publisher = %+v, want the unverified zero value", name, got)
		}
	}
}

// A struct that only tags a field with the publisher type reads nothing into
// it: the type has no decoder of its own, so a new carrier cannot pick up
// encoding/json's last-member-wins by forgetting to count.
func TestPublisherWireHasNoDecoderOfItsOwn(t *testing.T) {
	t.Parallel()
	var carrier struct {
		Publisher publisherWire `json:"publisher"`
	}
	if err := json.Unmarshal([]byte(`{"publisher":{"name":"Acme Docs","verified":true}}`), &carrier); err != nil {
		t.Fatal(err)
	}
	if got := carrier.Publisher.publisher(); got != (Publisher{}) {
		t.Fatalf("publisher decoded by the struct decoder = %+v, want the unverified zero value", got)
	}
}

// An all-zeros created_at or expires_at is an unset date. Every owner read
// that carries a resource row drops it, so no rendering can print year 1 as a
// creation date or list a resource with no expiry as expired.
func TestResourceReadsDropAZeroDate(t *testing.T) {
	date := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		value string
		want  *time.Time
	}{
		"zero time": {value: `"0001-01-01T00:00:00Z"`},
		"null":      {value: `null`},
		"absent":    {},
		"a date":    {value: `"2026-03-01T00:00:00Z"`, want: &date},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			dates := ""
			if test.value != "" {
				dates = `,"created_at":` + test.value + `,"expires_at":` + test.value
			}
			row := fmt.Sprintf(`{"resource_id":%q,"crid":%q,"type":"url","status":"active","target_url":"https://example.com/data","allowed_device_keys":[]%s}`,
				srv.Key.ResourceID, srv.Key.CRID, dates)
			answer := func(status int, body string) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
				}
			}
			srv.Script(http.MethodGet, "/v1/resources", answer(http.StatusOK, `{"data":[`+row+`],"meta":{"has_more":false}}`))
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, answer(http.StatusOK, `{"data":{"resource":`+row+`}}`))
			srv.Script(http.MethodPost, "/v1/resources", answer(http.StatusCreated, `{"data":`+row+`,"meta":{}}`))
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, answer(http.StatusOK, `{"data":`+row+`}`))
			client := newTestClient(t, srv, nil)
			check := func(read string, createdAt, expiresAt *time.Time) {
				t.Helper()
				for key, got := range map[string]*time.Time{"created_at": createdAt, "expires_at": expiresAt} {
					if (got == nil) != (test.want == nil) || (got != nil && !got.Equal(*test.want)) {
						t.Errorf("%s: %s = %v, want %v", read, key, got, test.want)
					}
				}
			}

			page, err := client.List(context.Background(), ListOptions{})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("List = %+v, %v; want one row", page, err)
			}
			check("List", page.Items[0].CreatedAt, page.Items[0].ExpiresAt)
			resource, err := client.Resource(context.Background(), srv.Key.CRID)
			if err != nil {
				t.Fatalf("Resource: %v", err)
			}
			check("Resource", resource.CreatedAt, resource.ExpiresAt)
			published, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}
			check("Publish", published.CreatedAt, published.ExpiresAt)
			granted, err := client.SetDeviceGrants(context.Background(), srv.Key.CRID, nil)
			if err != nil {
				t.Fatalf("SetDeviceGrants: %v", err)
			}
			check("SetDeviceGrants", granted.CreatedAt, granted.ExpiresAt)
		})
	}
}

// The three owner reads that carry a resource row all refuse to let a
// repeated publisher member verify.
func TestResourceReadsNeverVerifyFromARepeatedPublisher(t *testing.T) {
	srv := apitest.NewServer(t)
	row := fmt.Sprintf(`{"resource_id":%q,"crid":%q,"type":"url","status":"active","target_url":"https://example.com/data",`+
		`"publisher":{"name":"Acme Docs","verified":false},"publisher":{"name":"Acme Docs","verified":true}}`,
		srv.Key.ResourceID, srv.Key.CRID)
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	srv.Script(http.MethodGet, "/v1/resources", answer(http.StatusOK, `{"data":[`+row+`],"meta":{"has_more":false}}`))
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, answer(http.StatusOK, `{"data":{"resource":`+row+`}}`))
	srv.Script(http.MethodPost, "/v1/resources", answer(http.StatusCreated, `{"data":`+row+`,"meta":{}}`))
	client := newTestClient(t, srv, nil)

	page, err := client.List(context.Background(), ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Publisher != (Publisher{}) {
		t.Fatalf("List = %+v, %v; want the zero publisher", page, err)
	}
	resource, err := client.Resource(context.Background(), srv.Key.CRID)
	if err != nil || resource.Publisher != (Publisher{}) {
		t.Fatalf("Resource = %+v, %v; want the zero publisher", resource, err)
	}
	published, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil || published.Publisher != (Publisher{}) {
		t.Fatalf("Publish = %+v, %v; want the zero publisher", published, err)
	}
}

// The device-grants change answers with a resource row too, so it carries the
// publisher under the same rules as the other owner reads: a named publisher
// is reported, an older service's silence is the zero value, and a repeated
// member never verifies.
func TestDeviceGrantsAnswerCarriesThePublisherUnderTheSameRules(t *testing.T) {
	for name, test := range map[string]struct {
		publisher string
		want      Publisher
	}{
		"named":          {publisher: `,"publisher":{"name":"Acme Docs","verified":false}`, want: Publisher{Name: "Acme Docs"}},
		"older service":  {},
		"repeated never": {publisher: `,"publisher":{"name":"Acme Docs","verified":false},"publisher":{"name":"Acme Docs","verified":true}`},
		"garbled":        {publisher: `,"publisher":"verified"`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			body := fmt.Sprintf(`{"data":{"resource_id":%q,"crid":%q,"type":"url","status":"active",`+
				`"target_url":"https://example.com/data","allowed_device_keys":[]%s}}`,
				srv.Key.ResourceID, srv.Key.CRID, test.publisher)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			resource, err := newTestClient(t, srv, nil).SetDeviceGrants(context.Background(), srv.Key.CRID, nil)
			if err != nil {
				t.Fatalf("SetDeviceGrants: %v; publisher metadata must never fail the change", err)
			}
			if resource.Publisher != test.want {
				t.Fatalf("publisher = %+v, want %+v", resource.Publisher, test.want)
			}
		})
	}
}

func TestResourceReadsCarryPublisher(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)
	want := Publisher{Name: apitest.DefaultPublisherName}

	page, err := client.List(context.Background(), ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Publisher != want {
		t.Fatalf("List = %+v, %v; want publisher %+v", page, err, want)
	}
	resource, err := client.Resource(context.Background(), srv.Key.CRID)
	if err != nil || resource.Publisher != want || resource.CreatedAt == nil {
		t.Fatalf("Resource = %+v, %v; want publisher %+v and a creation date", resource, err, want)
	}
	published, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil || published.Publisher != want {
		t.Fatalf("Publish = %+v, %v; want publisher %+v", published, err, want)
	}
}

// An older service sends no publisher at all. Every owner read still
// succeeds and reports the unnamed, unverified zero value.
func TestResourceReadsFromOlderServiceAreUnverified(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.OmitPublisherMetadata()
	client := newTestClient(t, srv, nil)

	page, err := client.List(context.Background(), ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Publisher != (Publisher{}) {
		t.Fatalf("List = %+v, %v; want the zero publisher", page, err)
	}
	published, err := client.Publish(context.Background(), "https://example.com/data", PublishOptions{})
	if err != nil || published.Publisher != (Publisher{}) {
		t.Fatalf("Publish = %+v, %v; want the zero publisher", published, err)
	}
	link, err := client.Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if err != nil || link.Publisher != (Publisher{}) || link.ResourceCreatedAt != nil {
		t.Fatalf("Share = %+v, %v; want the zero publisher and no creation date", link, err)
	}
}

func TestShareCarriesPublisherAndCreationDate(t *testing.T) {
	srv := apitest.NewServer(t)
	link, err := newTestClient(t, srv, nil).Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if link.Publisher != (Publisher{Name: apitest.DefaultPublisherName}) || link.ResourceCreatedAt == nil || !link.ResourceCreatedAt.Equal(created) {
		t.Fatalf("share = %+v, want the mock publisher and %s", link, created)
	}
	// The metadata rode the share answer: no second request was made for it.
	if requests := srv.Requests(); len(requests) != 1 {
		t.Fatalf("share made %d requests, want exactly the share request", len(requests))
	}
}

// The share answer names the date resource_created_at, because its other
// fields describe the minted link. A bare created_at there is not the
// resource's creation date and must not be read as one.
func TestShareReadsOnlyTheResourceCreationDateKey(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"qurl":"https://qurl.link/#x","crid":%q,"created_at":"2026-03-01T00:00:00Z"}}`, srv.Key.CRID)
	})
	link, err := newTestClient(t, srv, nil).Share(context.Background(), srv.Key.CRID, ShareOptions{})
	if err != nil || link.ResourceCreatedAt != nil {
		t.Fatalf("Share = %+v, %v; a bare created_at must not become the resource's creation date", link, err)
	}
}

func TestSharingStateCarriesPublisherAndCreationDate(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	base := fmt.Sprintf(`"resource_id":%q,"crid":%q,"desired_state":"off","serving_epoch":0,"connection_state":"stopped"`, key.ResourceID, key.CRID)
	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		fields      string
		want        Publisher
		wantCreated bool
	}{
		"older service": {fields: base},
		"named":         {fields: base + `,"created_at":"2026-03-01T00:00:00Z","publisher":{"name":"Acme Docs","verified":false}`, want: Publisher{Name: "Acme Docs"}, wantCreated: true},
		"verified":      {fields: base + `,"publisher":{"name":"Acme Docs","verified":true}`, want: Publisher{Name: "Acme Docs", Verified: true}},
		// Descriptive metadata never fails a lifecycle read.
		"garbled publisher":    {fields: base + `,"publisher":"Acme Docs","created_at":"2026-03-01T00:00:00Z"`, wantCreated: true},
		"string verified":      {fields: base + `,"publisher":{"verified":"true"}`},
		"garbled created_at":   {fields: base + `,"created_at":"yesterday","publisher":{"name":"Acme Docs"}`, want: Publisher{Name: "Acme Docs"}},
		"numeric created_at":   {fields: base + `,"created_at":1772323200`},
		"null created_at":      {fields: base + `,"created_at":null,"publisher":null`},
		"zero-time created_at": {fields: base + `,"created_at":"0001-01-01T00:00:00Z"`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServerWithKey(t, key)
			srv.Script(http.MethodGet, "/v1/resources/"+key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":{%s}}`, test.fields)
			})
			got, err := newTestClient(t, srv, nil).Sharing(context.Background(), key.CRID)
			if err != nil {
				t.Fatalf("Sharing() = %v; metadata must not fail the read", err)
			}
			if got.Publisher != test.want {
				t.Fatalf("publisher = %+v, want %+v", got.Publisher, test.want)
			}
			if (got.CreatedAt != nil) != test.wantCreated || (test.wantCreated && !got.CreatedAt.Equal(created)) {
				t.Fatalf("created_at = %v, want present=%t", got.CreatedAt, test.wantCreated)
			}
		})
	}

	// The sharing row rejects a repeated known member; publisher metadata is
	// held to the same rule rather than letting the last one win.
	t.Run("repeated publisher is rejected", func(t *testing.T) {
		srv := apitest.NewServerWithKey(t, key)
		srv.Script(http.MethodGet, "/v1/resources/"+key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":{%s,"publisher":{"verified":false},"publisher":{"verified":true}}}`, base)
		})
		if got, err := newTestClient(t, srv, nil).Sharing(context.Background(), key.CRID); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatalf("Sharing() = %+v, %v; want an invalid API response", got, err)
		}
	})
}

func TestPublisherProfileReadAndChange(t *testing.T) {
	for name, open := range map[string]func(*testing.T, *apitest.Server) Client{
		"account key": func(t *testing.T, srv *apitest.Server) Client { return newTestClient(t, srv, nil) },
		// The registered device credential is the only credential a device
		// owner has, so the profile routes must be reachable with it.
		"registered device": newRegisteredTestClient,
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			client := open(t, srv)

			profile, err := client.Publisher(context.Background())
			if err != nil || *profile != (Publisher{Name: apitest.DefaultPublisherName}) {
				t.Fatalf("Publisher() = %+v, %v", profile, err)
			}
			profile, err = client.SetPublisherName(context.Background(), "Northwind Labs")
			if err != nil || *profile != (Publisher{Name: "Northwind Labs"}) {
				t.Fatalf("SetPublisherName() = %+v, %v", profile, err)
			}
			profile, err = client.SetPublisherName(context.Background(), "")
			if err != nil || *profile != (Publisher{}) {
				t.Fatalf("clearing the name = %+v, %v; want the zero publisher", profile, err)
			}

			requests := srv.Requests()
			if len(requests) != 3 {
				t.Fatalf("requests = %d, want 3", len(requests))
			}
			for index, method := range []string{http.MethodGet, http.MethodPatch, http.MethodPatch} {
				request := requests[index]
				if request.Method != method || request.Path != "/v1/me/publisher" || request.Query != "" ||
					!strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
					t.Errorf("request %d = %s %s?%s, authorized=%t", index, request.Method, request.Path, request.Query,
						request.Header.Get("Authorization") != "")
				}
			}
		})
	}
}

func TestSetPublisherNameRefusalKeepsTheServiceReason(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	profile, err := client.SetPublisherName(context.Background(), "Acme Verified")
	if profile != nil || !errors.Is(err, qurl.ErrInvalidPublisherName) {
		t.Fatalf("SetPublisherName() = %+v, %v; want ErrInvalidPublisherName", profile, err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "invalid_input" {
		t.Fatalf("refusal lost the typed service error: %v", err)
	}
	if got := PublisherNameReason(err); got != "name must not contain the word verified" {
		t.Fatalf("reason = %q", got)
	}
	// The refused name was not stored.
	if current, err := client.Publisher(context.Background()); err != nil || current.Name != apitest.DefaultPublisherName {
		t.Fatalf("profile after a refusal = %+v, %v", current, err)
	}
}

func TestPublisherNameReason(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newTestClient(t, srv, nil)

	// A name that can never be valid is refused before any request.
	_, err := client.SetPublisherName(context.Background(), "bad\xffname")
	if !errors.Is(err, qurl.ErrInvalidPublisherName) || len(srv.Requests()) != 0 {
		t.Fatalf("invalid UTF-8 = %v after %d requests; want a local refusal", err, len(srv.Requests()))
	}
	if got := PublisherNameReason(err); got != "name is not valid UTF-8" {
		t.Fatalf("local reason = %q", got)
	}
	if got := PublisherNameReason(fmt.Errorf("publisher set: %w", err)); got != "name is not valid UTF-8" {
		t.Fatalf("wrapped local reason = %q", got)
	}

	// Only a publisher-name refusal has a reason; an unrelated failure that
	// happens to be a 400 must not be reported as a bad name.
	unrelated := &Error{StatusCode: http.StatusBadRequest, Code: "invalid_input", Detail: "something else"}
	if got := PublisherNameReason(unrelated); got != "" {
		t.Fatalf("unrelated error produced a publisher-name reason %q", got)
	}
	if got := PublisherNameReason(nil); got != "" {
		t.Fatalf("nil error produced a reason %q", got)
	}
}

func TestPublisherProfileFailuresPassThrough(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me/publisher", apitest.HandlerAccountFrozen403(t))
	srv.Script(http.MethodPatch, "/v1/me/publisher", apitest.HandlerAccountFrozen403(t))
	client := newTestClient(t, srv, nil)

	for name, call := range map[string]func() (*Publisher, error){
		"read": func() (*Publisher, error) { return client.Publisher(context.Background()) },
		"set":  func() (*Publisher, error) { return client.SetPublisherName(context.Background(), "Acme Docs") },
	} {
		profile, err := call()
		var apiErr *Error
		if profile != nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "account_frozen" {
			t.Errorf("%s = %+v, %v; want the typed 403", name, profile, err)
		}
		// A refusal that is not about the name must not read as a bad name.
		if errors.Is(err, qurl.ErrInvalidPublisherName) {
			t.Errorf("%s: a frozen account was reported as an invalid publisher name", name)
		}
	}
}

func TestPublisherProfileRejectsEmptyAnswer(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"request_id":"req_test"}}`))
	})
	if profile, err := newTestClient(t, srv, nil).Publisher(context.Background()); profile != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("Publisher() = %+v, %v; want an invalid API response", profile, err)
	}
	if profile, err := publisherFromSDK(nil); profile != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("nil SDK profile = %+v, %v", profile, err)
	}
}
