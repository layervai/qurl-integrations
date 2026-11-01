package main

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

type publisherStripeClient interface {
	ConnectPublisherStripe(context.Context) (string, error)
	PublisherStripeAccount(context.Context) (string, error)
}

func paymentsCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "payments <connect|account>",
		Short: "Connect Stripe to receive CRID payments; LayerV collects 3%",
		Long:  "Connect Stripe to receive CRID payments; LayerV collects 3%. First run qurl account setup to link this device. Open the consent URL yourself, then finish setup in LayerV billing while signed in to that publisher account. Run qurl payments account after completion.",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "connect" && args[0] != "account" {
				return exitcode.UsageError(errors.New("choose payments connect or payments account"))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			payments, ok := client.ConsumerManagement().(publisherStripeClient)
			if !ok {
				return errors.New("publisher payments are unavailable in this client")
			}
			if args[0] == "connect" {
				link, e := payments.ConnectPublisherStripe(cmd.Context())
				if e != nil {
					return e
				}
				return opts.printer().PublisherPayments("", link)
			}
			account, e := payments.PublisherStripeAccount(cmd.Context())
			if e != nil {
				return e
			}
			return opts.printer().PublisherPayments(account, "")
		}}
}
