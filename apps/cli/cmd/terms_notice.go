package main

import (
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const termsVersion = "2.0"
const termsNotice = "By publishing you agree to the LayerV Terms: https://layerv.ai/terms\nYou are letting the people you approve reach this app on your computer. You choose who."

// beginTermsNotice keeps stdout scriptable and never blocks a command on state I/O.
func (o *globalOpts) beginTermsNotice() {
	if o.termsNoticeHandled {
		return
	}
	o.termsNoticeHandled = true
	dir, err := o.resolveShareStateDir("")
	shown := false
	if err == nil {
		shown, err = state.TermsNoticeShown(dir, termsVersion)
	}
	if shown {
		return
	}
	save := func() {
		if err == nil {
			err = state.MarkTermsNoticeShown(dir, termsVersion)
		}
		if err != nil {
			if log := o.verboseLogger(); log != nil {
				log("Terms notice state is unavailable; the notice can appear again")
			}
		}
	}
	if o.resolvedFormat != output.FormatJSON {
		o.printer().Notef("%s", termsNotice)
		save()
		return
	}
	o.pendingTermsNotice = termsNotice
	o.saveTermsNotice = save
}

// Save only after the JSON notice reaches stdout, including foreground publish.
func (o *globalOpts) finishTermsNotice(err error) error {
	if err == nil && o.saveTermsNotice != nil {
		o.saveTermsNotice()
		o.saveTermsNotice = nil
		o.pendingTermsNotice = ""
	}
	return err
}
