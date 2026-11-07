package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	paymentsConnect    = "connect"
	paymentsAccount    = "account"
	paymentsDisconnect = "disconnect"
)

type publisherStripeClient interface {
	ConnectPublisherStripe(context.Context) (string, error)
}

func paymentsCmd(opts *globalOpts) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "payments <connect|account|disconnect>",
		Short: "Connect Stripe to receive CRID payments; LayerV collects 3%",
		Long: `Connect Stripe to receive CRID payments; LayerV collects 3%. First run qurl account setup to link this device. Open the consent URL yourself, then finish setup in LayerV billing while signed in to that publisher account. Run qurl payments account after completion to see whether Stripe lets the account take payments.

qurl payments disconnect stops every subscription from renewing, then removes LayerV from the Stripe account. Buyers keep access until their paid period ends and are not charged again or refunded. New purchases stop until you connect the same or another account. Interactive runs confirm first; scripts and pipelines must pass --yes. Finishing a disconnect that already started, or releasing an account whose LayerV access was removed in Stripe, asks nothing more.`,
		Example: "  qurl payments account\n  qurl payments disconnect --yes",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation := args[0]
			if operation != paymentsConnect && operation != paymentsAccount && operation != paymentsDisconnect {
				return exitcode.UsageError(errors.New("choose payments connect, payments account or payments disconnect"))
			}
			if yes && operation != paymentsDisconnect {
				return exitcode.UsageError(errors.New("--yes applies only to payments disconnect"))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			if operation == paymentsConnect {
				payments, ok := client.ConsumerManagement().(publisherStripeClient)
				if !ok {
					return errors.New("publisher payments are unavailable in this client")
				}
				link, e := payments.ConnectPublisherStripe(cmd.Context())
				if e != nil {
					return e
				}
				return opts.printer().PublisherPayments(link)
			}
			account, ok := client.(qurlapi.PublisherStripe)
			if !ok {
				return errors.New("publisher payments are unavailable in this client")
			}
			if operation == paymentsAccount {
				status, e := account.PublisherStripeStatus(cmd.Context())
				if e != nil {
					return e
				}
				return opts.printer().PublisherStripeAccount(status)
			}
			return disconnectStripe(cmd.Context(), opts, account, yes)
		}}
	cmd.Flags().BoolVar(&yes, "yes", false, "disconnect without confirmation")
	return cmd
}

// disconnectStripe confirms unless --yes, which needs no status read so it can
// finish a disconnect even when Stripe cannot be read.
func disconnectStripe(ctx context.Context, opts *globalOpts, account qurlapi.PublisherStripe, yes bool) error {
	if !yes {
		status, err := account.PublisherStripeStatus(ctx)
		if err != nil {
			opts.printer().Notef("To disconnect without reading the account status, re-run with --yes.")
			return err
		}
		if status.AccountID == "" {
			return opts.printer().PublisherStripeDisconnected(false)
		}
		if status.Revoked {
			// Nothing LayerV can stop is left; finishing only releases the account.
			opts.printer().Notef("LayerV's access was already removed in Stripe. Cancel or refund remaining subscriptions there.")
		}
		// An unfinished disconnect was confirmed when it started.
		if !status.Disconnecting && !status.Revoked {
			confirmed, err := confirmStripeDisconnect(opts, status)
			if err != nil {
				return err
			}
			if !confirmed {
				opts.printer().Notef("Canceled — Stripe is still connected.")
				return nil
			}
		}
	}
	if err := account.DisconnectPublisherStripe(ctx); err != nil {
		return err
	}
	return opts.printer().PublisherStripeDisconnected(true)
}

// confirmStripeDisconnect states what a disconnect stops before asking.
func confirmStripeDisconnect(opts *globalOpts, status *qurlapi.PublisherStripeAccount) (bool, error) {
	renewals := fmt.Sprintf("%d subscriptions", status.RenewingSubscriptions)
	switch {
	case status.StatusUnavailable:
		// The count is unknown; never state one before an action that cannot be undone.
		renewals = "Every renewing subscription"
	case status.RenewingSubscriptions == 1:
		renewals = "1 subscription"
	}
	return confirmOnTerminal(opts, "disconnect", fmt.Sprintf("Disconnect Stripe account %s? %s will stop renewing; buyers keep access until their paid period ends. [y/N] ", status.AccountID, renewals))
}
