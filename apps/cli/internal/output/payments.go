package output

import "fmt"

// PaidAccessReady shows the stable buyer address once the policy is active.
func (p *Printer) PaidAccessReady(crid, link string, version int64) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			CRID    string `json:"crid"`
			Link    string `json:"link,omitempty"`
			Version int64  `json:"version"`
		}{crid, link, version})
	}
	if link == "" {
		link = crid
	}
	_, err := fmt.Fprintln(p.out, link)
	return err
}

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
