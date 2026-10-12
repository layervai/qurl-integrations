package qurlapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

// Preview is the CLI-owned public metadata shape. SDK additions never enter JSON output automatically.
type Preview struct {
	Revision    int64           `json:"revision"`
	Description string          `json:"description,omitempty"`
	Media       []PreviewMedia  `json:"media,omitempty"`
	Uploads     []PreviewUpload `json:"uploads,omitempty"`
}

// PreviewMedia identifies one processed item, without signed storage URLs.
type PreviewMedia struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Alt    string `json:"alt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// PreviewUpload describes a pending encoder job.
type PreviewUpload struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Alt       string `json:"alt"`
	ExpiresAt int64  `json:"expires_at"`
}

// PreviewPatch preserves omitted fields and uses the latest revision.
type PreviewPatch struct {
	Revision    int64
	Description *string
	MediaIDs    *[]string
}

func mapPreview(value *qurl.ResourcePreview) *Preview {
	if value == nil {
		return nil
	}
	result := &Preview{Revision: value.Revision, Description: value.Description}
	for i := range value.Media {
		m := &value.Media[i]
		result.Media = append(result.Media, PreviewMedia{ID: m.ID, Kind: m.Kind, Alt: m.Alt, Width: m.Width, Height: m.Height})
	}
	for _, u := range value.Uploads {
		result.Uploads = append(result.Uploads, PreviewUpload{ID: u.ID, Status: u.Status, Alt: u.Alt, ExpiresAt: u.ExpiresAt})
	}
	return result
}

// Preview reads one owned CRID's explicitly published metadata.
func (c *client) Preview(ctx context.Context, id string) (*Preview, error) {
	result, err := c.sdk.Preview(ctx, id)
	return mapPreview(result), mapError(err)
}

// UpdatePreview applies the caller's latest revision; it never retries a conflict.
func (c *client) UpdatePreview(ctx context.Context, id string, patch PreviewPatch) (*Preview, error) {
	result, err := c.sdk.UpdatePreview(ctx, id, qurl.PreviewPatch{Revision: patch.Revision, Description: patch.Description, MediaIDs: patch.MediaIDs})
	return mapPreview(result), mapError(err)
}

// UploadPreview streams a local file directly to the signed S3 PUT, without
// the device credential. Processing keeps current media visible until ready.
func (c *client) UploadPreview(ctx context.Context, id, path, alt, replaceID string) (*Preview, error) {
	file, err := os.Open(path) // #nosec G304 -- The publisher explicitly selects the local file to upload.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	// TODO(upstream-contract): early hints mirror API media types and 10/25 MB limits.
	if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 25_000_000 {
		return nil, PreviewFileError("preview file must be a nonempty regular file of at most 25 MB")
	}
	head := make([]byte, 512)
	count, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	contentType := http.DetectContentType(head[:count])
	switch contentType {
	case "image/jpeg", "image/png", "image/webp", "image/gif", "video/mp4", "video/webm":
	default:
		return nil, PreviewFileError("choose a JPEG, PNG, WebP, GIF, MP4 or WebM file")
	}
	if strings.HasPrefix(contentType, "image/") && contentType != "image/gif" && stat.Size() > 10_000_000 {
		return nil, PreviewFileError("still images must be at most 10 MB")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	current, err := c.sdk.Preview(ctx, id)
	if err != nil {
		return nil, mapError(err)
	}
	form, err := c.sdk.CreatePreviewUpload(ctx, id, qurl.PreviewUploadRequest{Revision: current.Revision, ContentType: contentType, Size: stat.Size(), Alt: alt, ReplaceID: replaceID})
	if err != nil {
		return nil, mapError(err)
	}
	if err := sendPreviewFile(ctx, previewUploadHTTPClient(), form, file, stat.Size()); err != nil {
		return nil, err
	}
	return mapPreview(&form.Preview), nil
}
func sendPreviewFile(ctx context.Context, transport qurl.HTTPDoer, form *qurl.PreviewUploadForm, source io.Reader, size int64) error {
	target, err := url.Parse(form.URL)
	if err != nil || target.Scheme != schemeHTTPS || target.Host == "" || target.User != nil || target.Fragment != "" {
		return errors.New("invalid preview upload address")
	}
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
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return fmt.Errorf("preview upload failed; check preview status before trying again: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("preview upload failed (HTTP %d); check preview status before trying again", response.StatusCode)
	}
	return nil
}
func previewUploadHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// PreviewFileError reports a local file that the upload contract rejects.
type PreviewFileError string

func (e PreviewFileError) Error() string { return string(e) }
