package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

const testOriginSecret = "origin-secret-5d1e"

func TestValidateOriginHeaderName(t *testing.T) {
	for name, want := range map[string]string{
		"x-vercel-protection-bypass":     "",
		"X-Vercel-Protection-Bypass":     "",
		"Authorization":                  "",
		"authorization":                  "",
		"CF-Access-Client-Secret":        "",
		"x-qurlish":                      "", // only the x-qurl- prefix is reserved
		"a!#$%&'*+-.^_`|~9":              "",
		strings.Repeat("h", 256):         "",
		strings.Repeat("h", 257):         msgOriginHeaderNameLength,
		"":                               msgOriginHeaderNameLength,
		"X Key":                          msgOriginHeaderNameToken,
		"X-Key:":                         msgOriginHeaderNameToken,
		"X-Kéy":                          msgOriginHeaderNameToken,
		"X-Key\r\nX-Injected":            msgOriginHeaderNameToken,
		"Host":                           msgOriginHeaderNameReserved,
		"COOKIE":                         msgOriginHeaderNameReserved,
		"Content-Length":                 msgOriginHeaderNameReserved,
		"Transfer-Encoding":              msgOriginHeaderNameReserved,
		"x-real-ip":                      msgOriginHeaderNameReserved,
		"X-Request-Id":                   msgOriginHeaderNameReserved,
		"Forwarded":                      msgOriginHeaderNameReserved,
		"X-Forwarded-For":                msgOriginHeaderNameReserved,
		"x-forwarded-anything":           msgOriginHeaderNameReserved,
		"X-QURL-Session":                 msgOriginHeaderNameReserved,
		"Proxy-Authorization":            msgOriginHeaderNameReserved,
		"x-forwarded":                    "", // the prefix is "x-forwarded-"
		"keep-alive":                     msgOriginHeaderNameReserved,
		"te":                             msgOriginHeaderNameReserved,
		"Expect":                         msgOriginHeaderNameReserved,
		"Via":                            msgOriginHeaderNameReserved,
		"Upgrade":                        msgOriginHeaderNameReserved,
		"Trailer":                        msgOriginHeaderNameReserved,
		"Connection":                     msgOriginHeaderNameReserved,
		"content-type":                   msgOriginHeaderNameReserved,
		"Content-Encoding":               msgOriginHeaderNameReserved,
		"proxy-":                         msgOriginHeaderNameReserved,
		"X-Forwarded-Proto-But-Longer-1": msgOriginHeaderNameReserved,
	} {
		err := ValidateOriginHeaderName(name)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != want {
			t.Errorf("ValidateOriginHeaderName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestValidateOriginHeaderValue(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  string
	}{
		"plain":                          {"abc123", ""},
		"inner space and tab":            {"Basic a b\tc", ""},
		"4096 bytes":                     {strings.Repeat("v", 4096), ""},
		"4096 bytes of two-byte runes":   {strings.Repeat("é", 2048), ""},
		"4097 bytes":                     {strings.Repeat("v", 4097), msgOriginHeaderValueLength},
		"4096 runes but 4097 bytes":      {strings.Repeat("v", 4095) + "é", msgOriginHeaderValueLength},
		"empty":                          {"", msgOriginHeaderValueLength},
		"invalid UTF-8":                  {"ab\xffcd", msgOriginHeaderValueUTF8},
		"leading space":                  {" abc", msgOriginHeaderValueSpace},
		"trailing space":                 {"abc ", msgOriginHeaderValueSpace},
		"leading tab":                    {"\tabc", msgOriginHeaderValueSpace},
		"trailing tab":                   {"abc\t", msgOriginHeaderValueSpace},
		"only spaces":                    {"   ", msgOriginHeaderValueSpace},
		"CRLF":                           {"abc\r\nX-Injected: 1", msgOriginHeaderValueControl},
		"bare CR":                        {"abc\rdef", msgOriginHeaderValueControl},
		"LF":                             {"abc\ndef", msgOriginHeaderValueControl},
		"NUL":                            {"abc\x00def", msgOriginHeaderValueControl},
		"DEL":                            {"abc\x7fdef", msgOriginHeaderValueControl},
		"unit separator":                 {"abc\x1fdef", msgOriginHeaderValueControl},
		"non-ASCII text is a fine value": {"pässwörd", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateOriginHeaderValue(tc.value)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.value != "" && strings.TrimSpace(tc.value) != "" && err != nil && strings.Contains(got, tc.value) {
				t.Fatal("the error repeats the value")
			}
		})
	}
}

// TestOriginHeaderGenericFormsHideTheValue pins that a stray %v, %+v, %#v,
// %s or JSON encoding of the option never prints the value.
func TestOriginHeaderGenericFormsHideTheValue(t *testing.T) {
	header := OriginHeader{Name: "X-Key", Value: testOriginSecret}
	opts := PublishOptions{OriginHeader: &header}
	forms := make([]string, 0, 7)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		forms = append(forms, fmt.Sprintf(verb, header), fmt.Sprintf(verb, opts.OriginHeader))
	}
	forms = append(forms, fmt.Sprintf("%+v", opts))
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	forms = append(forms, string(encoded))
	for _, form := range forms {
		if strings.Contains(form, testOriginSecret) {
			t.Fatalf("a generic form shows the value: %s", form)
		}
		if !strings.Contains(form, "X-Key") {
			t.Fatalf("a generic form lost the name: %s", form)
		}
	}
}

func TestPublishSendsTheOriginHeaderAndConfirmsTheName(t *testing.T) {
	srv := apitest.NewServer(t)
	published, err := clockedClient(t, srv).Publish(context.Background(), existingTarget, PublishOptions{
		Name: "App", OriginHeader: &OriginHeader{Name: "x-key", Value: testOriginSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.OriginHeaderName != "X-Key" {
		t.Fatalf("OriginHeaderName = %q", published.OriginHeaderName)
	}
	creates := sentCreates(t, srv)
	if len(creates) != 1 || string(creates[0]["origin_header"]) != `{"name":"x-key","value":"`+testOriginSecret+`"}` {
		t.Fatalf("creates = %q", creates)
	}
}

func TestPublishRefusesAnOriginHeaderItCannotSend(t *testing.T) {
	srv := apitest.NewServer(t)
	client := clockedClient(t, srv)
	for name, opts := range map[string]PublishOptions{
		"Connector":     {ConnectorID: "app", OriginHeader: &OriginHeader{Name: "X-Key", Value: testOriginSecret}},
		"invalid name":  {OriginHeader: &OriginHeader{Name: "Host", Value: testOriginSecret}},
		"invalid value": {OriginHeader: &OriginHeader{Name: "X-Key", Value: testOriginSecret + "\n"}},
	} {
		target := existingTarget
		if opts.ConnectorID != "" {
			target = ""
		}
		_, err := client.Publish(context.Background(), target, opts)
		if !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if reqs := srv.Requests(); len(reqs) != 0 {
		t.Fatalf("sent %d requests", len(reqs))
	}
}

// TestKeptPublicPublishHoldsTheOriginHeaderToo covers the second create that
// keeps an existing public resource: it carries the header, the service
// compares it, and the answer must name it.
func TestKeptPublicPublishHoldsTheOriginHeaderToo(t *testing.T) {
	for name, tc := range map[string]struct {
		existing string
		wantErr  error
	}{
		"same header":    {"X-Key", nil},
		"another header": {"X-Other", ErrOriginHeaderMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetPublishFoundExisting(true)
			srv.SetResourceAccess(false)
			srv.SetOriginHeader(tc.existing, testOriginSecret)
			published, err := clockedClient(t, srv).Publish(context.Background(), existingTarget, PublishOptions{
				KeepExistingPublic: true, OriginHeader: &OriginHeader{Name: "X-Key", Value: testOriginSecret},
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || !published.KeptPublic || published.OriginHeaderName != "X-Key" {
				t.Fatalf("published %+v, %v", published, err)
			}
			for _, create := range sentCreates(t, srv) {
				if _, ok := create["origin_header"]; !ok {
					t.Fatal("a create omitted the origin header")
				}
			}
		})
	}
}
