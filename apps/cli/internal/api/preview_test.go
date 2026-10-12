package qurlapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

type previewErrorDoer struct{ err error }

func (d previewErrorDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }
func TestPreviewUploadKeepsCancellationWithoutSignedURL(t *testing.T) {
	signed := "https://storage.test/file?signature=secret"
	err := sendPreviewFile(context.Background(), previewErrorDoer{&url.Error{Op: "Put", URL: signed, Err: context.Canceled}}, &qurl.PreviewUploadForm{URL: signed}, strings.NewReader("x"), 1)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestUploadPreviewRejectsInvalidFilesBeforeAPI(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		size int64
	}{
		{"empty", nil, 0}, {"wrong type", []byte("plain text"), 0},
		{"too large", nil, 25_000_001},
		{"large still", []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10}, 10_000_001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.size > 0 {
				if err := os.Truncate(path, tc.size); err != nil {
					t.Fatal(err)
				}
			}
			_, err := (&client{}).UploadPreview(context.Background(), "crid", path, "alt", "")
			var invalid PreviewFileError
			if !errors.As(err, &invalid) {
				t.Fatalf("error %T: %v", err, err)
			}
		})
	}
}
