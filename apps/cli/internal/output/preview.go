package output

import (
	"errors"
	"fmt"
	"strconv"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const (
	msgPreviewIncomplete  = "qURL preview response is incomplete"
	msgPreviewDescription = "Description: %s\nRevision: %d\n"
	msgPreviewMedia       = "Media %s (%s): %s\n"
	msgPreviewUpload      = "Upload %s: %s\n"
)

// Preview quotes publisher text so controls cannot reach the terminal.
func (p *Printer) Preview(preview *qurlapi.Preview) error {
	if preview == nil {
		return errors.New(msgPreviewIncomplete)
	}
	if p.format == FormatJSON {
		document := struct {
			Revision    int64               `json:"revision"`
			Description escapedJSONString   `json:"description,omitempty"`
			Media       []previewMediaJSON  `json:"media,omitempty"`
			Uploads     []previewUploadJSON `json:"uploads,omitempty"`
		}{Revision: preview.Revision, Description: escapedJSONString(preview.Description)}
		for _, m := range preview.Media {
			document.Media = append(document.Media, previewMediaJSON{m.Width, m.Height, escapedJSONString(m.ID), escapedJSONString(m.Kind), escapedJSONString(m.Alt)})
		}
		for _, u := range preview.Uploads {
			document.Uploads = append(document.Uploads, previewUploadJSON{u.ExpiresAt, escapedJSONString(u.ID), escapedJSONString(u.Status), escapedJSONString(u.Alt)})
		}
		return p.writeJSON(document)
	}
	if p.quiet {
		if preview.Description == "" {
			return nil
		}
		_, err := fmt.Fprintf(p.out, "%s\n", p.previewText(preview.Description))
		return err
	}
	if _, err := fmt.Fprintf(p.out, msgPreviewDescription, p.previewText(preview.Description), preview.Revision); err != nil {
		return err
	}
	for i := range preview.Media {
		media := &preview.Media[i]
		if _, err := fmt.Fprintf(p.out, msgPreviewMedia, p.previewText(media.ID), p.previewText(media.Kind), p.previewText(media.Alt)); err != nil {
			return err
		}
	}
	for _, upload := range preview.Uploads {
		if _, err := fmt.Fprintf(p.out, msgPreviewUpload, p.previewText(upload.ID), p.previewText(upload.Status)); err != nil {
			return err
		}
	}
	return nil
}

type previewMediaJSON struct {
	Width  int               `json:"width"`
	Height int               `json:"height"`
	ID     escapedJSONString `json:"id"`
	Kind   escapedJSONString `json:"kind"`
	Alt    escapedJSONString `json:"alt"`
}
type previewUploadJSON struct {
	ExpiresAt int64             `json:"expires_at"`
	ID        escapedJSONString `json:"id"`
	Status    escapedJSONString `json:"status"`
	Alt       escapedJSONString `json:"alt"`
}

func (p *Printer) previewText(value string) string {
	runes := []rune(value)
	cut := len(runes) > 500
	if cut {
		value = string(runes[:500])
	}
	quoted := strconv.Quote(value)
	if p.ascii {
		quoted = strconv.QuoteToASCII(value)
	}
	if cut {
		quoted += p.ellipsis()
	}
	return quoted
}
