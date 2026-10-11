package qurlapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

func TestPreviewUploadStreamsExactLengthWithoutAPICredential(t *testing.T) {
	source := "image bytes"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
			t.Error("API credential reached file storage")
		}
		if r.ContentLength != int64(len(source)) || len(r.TransferEncoding) > 0 {
			t.Errorf("wrong input length %d", r.ContentLength)
		}
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
			t.Error("upload must refuse overwrites")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != source {
			t.Errorf("file: %q %v", data, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	form := &qurl.PreviewUploadForm{URL: server.URL, Headers: map[string]string{"If-None-Match": "*", "Content-Type": "image/png"}}
	if err := sendPreviewFile(context.Background(), server.Client(), form, strings.NewReader(source), int64(len(source))); err != nil {
		t.Fatal(err)
	}
}
func TestPreviewUploadRefusesRedirectAndStopsShortSource(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "https://other.example.test")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = previewUploadHTTPClient().CheckRedirect
	form := &qurl.PreviewUploadForm{URL: server.URL, Headers: map[string]string{"If-None-Match": "*"}}
	if err := sendPreviewFile(context.Background(), client, form, strings.NewReader("body"), 4); err == nil {
		t.Fatal("followed redirect")
	}
	if err := sendPreviewFile(context.Background(), client, form, strings.NewReader("x"), 4); err == nil {
		t.Fatal("accepted incomplete file")
	}
}
