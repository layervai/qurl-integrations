package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const creditPriceFlag = "credit-price"

func validateCreditPriceFlags(cmd *cobra.Command, price int64, conflicting ...string) error {
	if !qurlapi.ValidCreditPrice(price) {
		return exitcode.UsageError(fmt.Errorf("--%s must be a whole number from 1 to 10", creditPriceFlag))
	}
	for _, name := range append(conflicting, consumerContract, consumerEmailDomainFlag, ruleFlag, ruleFileFlag, stripeAccountFlag, stripePriceFlag, paymentAccessSecondsFlag) {
		if cmd.Flags().Changed(name) {
			return exitcode.UsageError(fmt.Errorf("--%s cannot be combined with --%s", creditPriceFlag, name))
		}
	}
	return nil
}

func (o *globalOpts) configureCreditPrice(cmd *cobra.Command, args []string, credits, version int64, yes bool) error {
	if err := validateCreditPriceFlags(cmd, credits, modeFlag); err != nil {
		return err
	}
	if len(args) != 2 || (cmd.Flags().Changed(consumerVersion) && (version < 0 || version >= 1<<53-1)) {
		return exitcode.UsageError(errors.New("give a CRID and, optionally, its current policy --version"))
	}
	ctx := cmd.Context()
	assessment, err := cridux.Assess(args[1])
	if err != nil {
		return err
	}
	if err := applyCRIDGuards(o.printer(), assessment, o.productionEndpoint(), yes); err != nil {
		return err
	}
	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	result, err := client.SetCreditPrice(ctx, assessment.Input, credits, version)
	if err != nil {
		return err
	}
	return o.printer().ConsumerManagement(result)
}
