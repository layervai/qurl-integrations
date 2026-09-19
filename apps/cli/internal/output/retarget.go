package output

import "fmt"

// Retargeted renders the local-only conversion receipt; quiet text emits its count.
func (p *Printer) Retargeted(changed int) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			Changed int `json:"changed"`
		}{changed})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, changed)
		return err
	}
	_, err := fmt.Fprintf(p.out, "Updated %d local share targets.\n", changed)
	return err
}
