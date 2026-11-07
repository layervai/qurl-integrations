package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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

qurl payments disconnect stops every subscription from renewing, then removes LayerV from the Stripe account. Buyers keep access until their paid period ends and are not charged again or refunded. New purchases stop until you connect the same or another account. Interactive runs confirm first; scripts and pipelines must pass --yes. Finishing a disconnect that already started, or releasing an account whose LayerV access was removed in Stripe, asks nothing more. With --yes no status is read, so "disconnected": true then means that nothing is connected any more, whether or not something was.`,
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
			// connect goes through the SDK; account and disconnect use direct REST
			// calls because the SDK has neither the status members nor a disconnect.
			if operation == paymentsAccount {
				status, e := client.PublisherStripeStatus(cmd.Context())
				if e != nil {
					return e
				}
				// This CLI prints the 3% fee for a connected, readable account, so a fee
				// change must ship with a CLI release. Only that view refuses (JSON
				// included on purpose); revoked or unfinished states and a disconnect
				// never depend on the fee.
				readable := status.AccountID != "" && !status.Revoked && !status.Disconnecting && !status.StatusUnavailable
				if readable && status.LayerVFeePercent != qurlapi.LayerVFeePercent {
					return errors.New("LayerV's fee changed; upgrade the qurl CLI to see this account")
				}
				return opts.printer().PublisherStripeAccount(status)
			}
			return disconnectStripe(cmd.Context(), opts, client, yes)
		}}
	cmd.Flags().BoolVar(&yes, "yes", false, "disconnect without confirmation")
	return cmd
}

// disconnectStripe confirms unless --yes, which needs no status read so it can
// finish a disconnect even when Stripe cannot be read.
func disconnectStripe(ctx context.Context, opts *globalOpts, account qurlapi.PublisherStripe, yes bool) error {
	outcome := output.StripeDisconnectedUnread
	if !yes {
		outcome = output.StripeDisconnected
		status, err := account.PublisherStripeStatus(ctx)
		if err != nil {
			if statusReadMayRecover(err) {
				opts.printer().Notef("To disconnect without reading the account status, re-run with --yes.")
			}
			return err
		}
		if status.AccountID == "" {
			return opts.printer().PublisherStripeDisconnected(output.StripeNothingConnected)
		}
		if status.Revoked {
			// Nothing LayerV can stop is left; finishing only releases the account.
			outcome = output.StripeReleasedRevoked
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
	return opts.printer().PublisherStripeDisconnected(outcome)
}

// statusReadMayRecover reports a failure that --yes can get past: a 5xx or a
// network failure, a timeout included. A refused credential, an incompatible
// service and a canceled command (Ctrl-C) do not point to the flag that skips
// confirmation.
func statusReadMayRecover(err error) bool {
	var apiErr *qurlapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) && !errors.Is(err, context.Canceled)
}

// confirmStripeDisconnect states what a disconnect stops before asking.
func confirmStripeDisconnect(opts *globalOpts, status *qurlapi.PublisherStripeAccount) (bool, error) {
	renewals := fmt.Sprintf("%d subscriptions will stop renewing", status.RenewingSubscriptions)
	switch {
	case status.StatusUnavailable:
		// The count is unknown; never state one before an action that cannot be undone.
		renewals = "Every renewing subscription will stop renewing"
	case status.RenewingSubscriptions == 0:
		renewals = "No subscriptions are renewing"
	case status.RenewingSubscriptions == 1:
		renewals = "1 subscription will stop renewing"
	}
	return confirmOnTerminal(opts, "disconnect", fmt.Sprintf("Disconnect Stripe account %s? %s; buyers keep access until their paid period ends. [y/N] ", status.AccountID, renewals))
}
