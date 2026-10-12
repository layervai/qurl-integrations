package output

import (
	"errors"
	"fmt"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Preview quotes publisher text so controls cannot reach the terminal.
func (p *Printer) Preview(preview *qurlapi.Preview) error {
	if preview == nil {
		return errors.New("missing preview response")
	}
	if p.format == FormatJSON {
		return p.writeJSON(preview)
	}
	if p.quiet {
		if preview.Description == "" {
			return nil
		}
		_, err := fmt.Fprintf(p.out, "%q\n", preview.Description)
		return err
	}
	if _, err := fmt.Fprintf(p.out, "Description: %q\nRevision: %d\n", preview.Description, preview.Revision); err != nil {
		return err
	}
	for i := range preview.Media {
		media := &preview.Media[i]
		if _, err := fmt.Fprintf(p.out, "Media %q (%q): %q\n", media.ID, media.Kind, media.Alt); err != nil {
			return err
		}
	}
	for _, upload := range preview.Uploads {
		if _, err := fmt.Fprintf(p.out, "Upload %q: %q\n", upload.ID, upload.Status); err != nil {
			return err
		}
	}
	return nil
}
