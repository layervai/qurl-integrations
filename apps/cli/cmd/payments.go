package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const paymentsAccount = "account"

type publisherStripeClient interface {
	ConnectPublisherStripe(context.Context) (string, error)
}

// publisherStripeAccountClient reads and ends the connection with direct API calls.
type publisherStripeAccountClient interface {
	PublisherStripeStatus(context.Context) (*qurlapi.PublisherStripeAccount, error)
	DisconnectPublisherStripe(context.Context) error
}

func paymentsCmd(opts *globalOpts) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "payments <connect|account|disconnect>",
		Short: "Connect Stripe to receive CRID payments; LayerV collects 3%",
		Long: `Connect Stripe to receive CRID payments; LayerV collects 3%. First run qurl account setup to link this device. Open the consent URL yourself, then finish setup in LayerV billing while signed in to that publisher account. Run qurl payments account after completion to see whether Stripe lets the account take payments.

qurl payments disconnect stops every subscription from renewing, then removes LayerV from the Stripe account. Buyers keep access until their paid period ends and are not charged again or refunded. New purchases stop until you connect the same or another account. Interactive runs confirm first; scripts and pipelines must pass --yes.`,
		Example: "  qurl payments account\n  qurl payments disconnect --yes",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation := args[0]
			if operation != "connect" && operation != paymentsAccount && operation != "disconnect" {
				return exitcode.UsageError(errors.New("choose payments connect, payments account or payments disconnect"))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			if operation == "connect" {
				payments, ok := client.ConsumerManagement().(publisherStripeClient)
				if !ok {
					return errors.New("publisher payments are unavailable in this client")
				}
				link, e := payments.ConnectPublisherStripe(cmd.Context())
				if e != nil {
					return e
				}
				return opts.printer().PublisherPayments("", link)
			}
			account, ok := client.(publisherStripeAccountClient)
			if !ok {
				return errors.New("publisher payments are unavailable in this client")
			}
			// --yes needs no status, so it can finish a disconnect even when Stripe cannot be read.
			if operation == paymentsAccount || !yes {
				status, e := account.PublisherStripeStatus(cmd.Context())
				if e != nil {
					return e
				}
				if operation == paymentsAccount {
					return opts.printer().PublisherStripeAccount(status)
				}
				if status.AccountID != "" && !status.Disconnecting {
					confirmed, e := confirmStripeDisconnect(opts, status)
					if e != nil {
						return e
					}
					if !confirmed {
						opts.printer().Notef("Canceled — Stripe is still connected.")
						return nil
					}
				}
			}
			if err := account.DisconnectPublisherStripe(cmd.Context()); err != nil {
				return err
			}
			return opts.printer().PublisherStripeDisconnected()
		}}
	cmd.Flags().BoolVar(&yes, "yes", false, "disconnect without confirmation")
	return cmd
}

// confirmStripeDisconnect states what a disconnect stops before asking.
func confirmStripeDisconnect(opts *globalOpts, status *qurlapi.PublisherStripeAccount) (bool, error) {
	renewals := fmt.Sprintf("%d subscriptions", status.RenewingSubscriptions)
	if status.RenewingSubscriptions == 1 {
		renewals = "1 subscription"
	}
	return confirmOnTerminal(opts, "disconnect", fmt.Sprintf("Disconnect Stripe account %s? %s will stop renewing; buyers keep access until their paid period ends. [y/N] ", status.AccountID, renewals))
}
