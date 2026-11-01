package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type configTransport func(*http.Request) (*http.Response, error)

func (f configTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPublicConfigUsesExplicitOperatorOrigin(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("const QURL_LINK_CONFIG = {\n        issuerTrustStore: {\"fixture\":%q},\n      };", base64.RawURLEncoding.EncodeToString(der))
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = configTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://public.sandbox.example.com/" || req.Header.Get("Authorization") != "" || req.URL.User != nil {
			t.Fatal("bootstrap request changed its configured origin or carried credentials")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	config, err := loadPublicConfig("https://public.sandbox.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuerTrust(config); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("bootstrap requests: got %d want 1", calls)
	}
}

func TestPublicConfigRejectsUnsafeOriginsBeforeHTTP(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = configTransport(func(*http.Request) (*http.Response, error) {
		calls++
		t.Fatal("invalid origin reached HTTP")
		return nil, errors.New("invalid origin reached HTTP")
	})
	for _, endpoint := range []string{"", "http://public.example.com/", "https://user:secret@public.example.com/", "https://public.example.com/path", "https://public.example.com/?x=1", "https://public.example.com/?", "https://public.example.com/#x", "https://:443/"} {
		if _, err := loadPublicConfig(endpoint); err == nil {
			t.Fatal("unsafe bootstrap origin accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid origin caused a request")
	}
}

func TestPublicConfigKeepsRedirectAndBodyLimits(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, status := range []int{http.StatusFound, http.StatusOK} {
		calls := 0
		http.DefaultTransport = configTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			header.Set("Location", "https://other.example.com/")
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 1048577))), Request: req}, nil
		})
		if _, err := loadPublicConfig("https://public.example.com/"); err == nil {
			t.Fatal("redirect or oversized bootstrap body accepted")
		}
		if calls != 1 {
			t.Fatalf("request count: got %d want 1", calls)
		}
	}
}
