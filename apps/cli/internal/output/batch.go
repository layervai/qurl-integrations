package output

import (
	"fmt"
	"strconv"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// BatchCreated keeps success links on stdout and quiet-mode item errors on stderr.
func (p *Printer) BatchCreated(result *qurlapi.BatchCreateResult) error {
	if p.format == FormatJSON {
		return p.writeJSON(result)
	}
	for _, item := range result.Results {
		if item.Success {
			if p.quiet {
				if _, err := fmt.Fprintln(p.out, item.QURLLink); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintf(p.out, "[%d] %s\n", item.Index, item.QURLLink); err != nil {
				return err
			}
		} else if p.quiet {
			p.Warnf("Item %d failed: %s: %s", item.Index, strconv.Quote(item.Error.Code), strconv.Quote(item.Error.Message))
		} else if _, err := fmt.Fprintf(p.out, "[%d] failed: %s: %s\n", item.Index, strconv.Quote(item.Error.Code), strconv.Quote(item.Error.Message)); err != nil {
			return err
		}
	}
	return nil
}
