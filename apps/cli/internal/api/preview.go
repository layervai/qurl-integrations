package qurlapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

func previewSDK(api Client) (*qurl.Client, error) {
	registered, ok := api.(*registeredClient)
	if !ok {
		return nil, fmt.Errorf("%w: preview requires a registered device", qurl.ErrInvalidClientConfig)
	}
	c, ok := registered.Client.(*client)
	if !ok {
		return nil, qurl.ErrInvalidClientConfig
	}
	return c.sdk, nil
}

// Preview reads one owned CRID's explicitly published metadata.
func Preview(ctx context.Context, api Client, id string) (*qurl.ResourcePreview, error) {
	sdk, err := previewSDK(api)
	if err != nil {
		return nil, err
	}
	result, err := sdk.Preview(ctx, id)
	return result, mapError(err)
}

// UpdatePreview applies the caller's latest revision; it never retries a conflict.
func UpdatePreview(ctx context.Context, api Client, id string, patch qurl.PreviewPatch) (*qurl.ResourcePreview, error) {
	sdk, err := previewSDK(api)
	if err != nil {
		return nil, err
	}
	result, err := sdk.UpdatePreview(ctx, id, patch)
	return result, mapError(err)
}

// UploadPreview streams a local file directly to the signed S3 PUT, without
// the device credential. Processing keeps current media visible until ready.
func UploadPreview(ctx context.Context, api Client, id, path, alt, replaceID string) (*qurl.ResourcePreview, error) {
	sdk, err := previewSDK(api)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path) // #nosec G304 -- The publisher explicitly selects the local file to upload.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 25_000_000 {
		return nil, errors.New("preview file must be a regular file of at most 25 MB")
	}
	head := make([]byte, 512)
	count, err := file.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	contentType := http.DetectContentType(head[:count])
	switch contentType {
	case "image/jpeg", "image/png", "image/webp", "image/gif", "video/mp4", "video/webm":
	default:
		return nil, errors.New("choose a JPEG, PNG, WebP, GIF, MP4 or WebM file")
	}
	if strings.HasPrefix(contentType, "image/") && contentType != "image/gif" && stat.Size() > 10_000_000 {
		return nil, errors.New("still images must be at most 10 MB")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	current, err := sdk.Preview(ctx, id)
	if err != nil {
		return nil, mapError(err)
	}
	form, err := sdk.CreatePreviewUpload(ctx, id, qurl.PreviewUploadRequest{Revision: current.Revision, ContentType: contentType, Size: stat.Size(), Alt: alt, ReplaceID: replaceID})
	if err != nil {
		return nil, mapError(err)
	}
	if err := sendPreviewFile(ctx, previewUploadHTTPClient(), form, file, stat.Size()); err != nil {
		return nil, err
	}
	return &form.Preview, nil
}
func sendPreviewFile(ctx context.Context, transport qurl.HTTPDoer, form *qurl.PreviewUploadForm, source io.Reader, size int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, form.URL, io.LimitReader(source, size))
	if err != nil {
		return err
	}
	request.ContentLength = size
	for name, value := range form.Headers {
		request.Header.Set(name, value)
	}
	response, err := transport.Do(request)
	if err != nil {
		return errors.New("preview upload failed; check preview status before trying again")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("preview upload failed (HTTP %d); check preview status before trying again", response.StatusCode)
	}
	return nil
}
func previewUploadHTTPClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
