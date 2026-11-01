package output

import "fmt"

// PublisherPayments prints connection details without payment credentials.
func (p *Printer) PublisherPayments(account, link string) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			AccountID        string `json:"account_id,omitempty"`
			ConnectURL       string `json:"connect_url,omitempty"`
			LayerVFeePercent int    `json:"layerv_fee_percent"`
		}{account, link, 3})
	}
	value := account
	if link != "" {
		value = link
	}
	if value == "" {
		value = "No Stripe account is connected. Run qurl payments connect."
	}
	_, err := fmt.Fprintln(p.out, value)
	if err == nil && !p.quiet {
		p.Notef("LayerV collects 3%% of each CRID payment, including subscription renewals.")
	}
	return err
}
