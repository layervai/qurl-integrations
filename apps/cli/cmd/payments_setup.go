package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const paidAccessContract = "paid-access"

type deviceStripePayments interface {
	PublisherStripeAccount(context.Context) (string, error)
	ConnectPublisherStripeForDevice(context.Context, int) (link, state string, err error)
	CompletePublisherStripe(context.Context, string, string) error
	CreatePublisherStripePrice(context.Context, string, int64, string) (*qurl.CRIDPaymentPolicy, error)
}

func paidAccessSetupCmd(opts *globalOpts) *cobra.Command {
	var name, amount, interval string
	var version, accessSeconds int64
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup <CRID>",
		Short: "Set a price, connect Stripe if needed, and turn on paid access",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cents, err := validatePaidAccessSetup(amount, interval, version, accessSeconds)
			if err != nil {
				return exitcode.UsageError(err)
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			if err := applyCRIDGuards(opts.printer(), assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}
			ctx := cmd.Context()
			client, err := opts.newClient(ctx)
			if err != nil {
				return err
			}
			resource, err := client.Resource(ctx, assessment.Input)
			if err != nil {
				return err
			}
			retry, err := isPaidAccessRetry(resource, version)
			if err != nil {
				return err
			}
			checkoutName, err := paidAccessCheckoutName(ctx, client, resource.CRID, name, cmd.Flags().Changed("name"))
			if err != nil {
				return err
			}
			management := client.ConsumerManagement()
			payments, ok := management.(deviceStripePayments)
			if !ok {
				return errors.New("publisher payments are unavailable in this client")
			}
			if err := ensurePublisherStripe(ctx, opts, payments); err != nil {
				return err
			}
			price, err := payments.CreatePublisherStripePrice(ctx, checkoutName, cents, interval)
			if err != nil {
				return err
			}
			price.AccessSeconds = accessSeconds
			if retry && *resource.ConsumerAccess.Payment != *price {
				return exitcode.UsageError(fmt.Errorf("this CRID has a different paid offer at version %d. Review it before replacing it with --version %d", version+1, version+1))
			}
			policy := qurl.ConsumerAccessPolicy{Mode: consumerContract, Version: version + 1, ContractID: paidAccessContract, Payment: price}
			if err := management.UpdateConsumerAccess(ctx, assessment.Input, version, policy); err != nil {
				return err
			}
			link := ""
			if opts.productionEndpoint() {
				link = "https://qurl.link/" + resource.CRID
			} else if strings.TrimRight(opts.resolvedEndpoint, "/") == "https://api.layerv.xyz" {
				link = "https://qurl.link.layerv.xyz/" + resource.CRID
			}
			return opts.printer().PaidAccessReady(resource.CRID, link, policy.Version)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name on Stripe Checkout (defaults to the public CRID name)")
	cmd.Flags().StringVar(&amount, "price", "", "price in US dollars (for example, 5.00)")
	cmd.Flags().StringVar(&interval, "interval", "", "subscription interval: month or year (empty means pay once)")
	cmd.Flags().Int64Var(&version, consumerVersion, 0, "current policy version; replacing a policy can revoke access")
	cmd.Flags().Int64Var(&accessSeconds, paymentAccessSecondsFlag, 0, "one-time access duration; zero lasts for the resource lifetime")
	cmd.Flags().BoolVar(&yes, "yes", false, "allow changes to a test CRID on a production endpoint")
	cmd.Long = "Set a price and connect Stripe if needed. Turning on paid access makes the CRID private."
	return cmd
}

func validatePaidAccessSetup(amount, interval string, version, accessSeconds int64) (int64, error) {
	if version < 0 || version >= 9007199254740991 {
		return 0, errors.New("invalid policy version")
	}
	if interval != "" && interval != "month" && interval != "year" {
		return 0, errors.New("use month or year for subscriptions")
	}
	if accessSeconds < 0 || accessSeconds > 315360000 || (interval != "" && accessSeconds != 0) {
		return 0, errors.New("set an access duration between 0 and 315360000 seconds only for a one-time payment")
	}
	return paidAccessCents(amount)
}

func isPaidAccessRetry(resource *qurlapi.ResourceSummary, version int64) (bool, error) {
	if len(resource.CRID) != 60 {
		return false, errors.New("the service did not return the full CRID")
	}
	currentVersion := resource.ConsumerAccessVersion
	if current := resource.ConsumerAccess; current != nil {
		currentVersion = current.Version
		if current.Version == version+1 && current.Mode == consumerContract && current.ContractID == paidAccessContract && current.Payment != nil {
			return true, nil
		}
	}
	if currentVersion != version {
		return false, exitcode.UsageError(fmt.Errorf("the current policy version is %d. Review it and retry with --version %d to replace it", currentVersion, currentVersion))
	}
	return false, nil
}

func paidAccessCheckoutName(ctx context.Context, client qurlapi.Client, crid, name string, override bool) (string, error) {
	name = strings.TrimSpace(name)
	if !override {
		preview, err := client.Preview(ctx, crid)
		if err != nil {
			return "", err
		}
		name = preview.Name
	}
	if !validPublicName(name) {
		return "", exitcode.UsageError(errors.New("set --name to the app name buyers should see at checkout"))
	}
	return name, nil
}

func ensurePublisherStripe(ctx context.Context, opts *globalOpts, payments deviceStripePayments) error {
	account, err := payments.PublisherStripeAccount(ctx)
	if err != nil || account != "" {
		return err
	}
	opts.printer().Notef("Complete Stripe setup in your browser.")
	return connectStripeOnDevice(ctx, payments, opts.openBrowser)
}

func paidAccessCents(price string) (int64, error) {
	const message = "set --price in US dollars from 0.50 to 999999.99, with up to two decimal places"
	dollars, fraction, decimal := strings.Cut(price, ".")
	if dollars == "" || len(dollars) > 6 || len(fraction) > 2 || (decimal && fraction == "") {
		return 0, errors.New(message)
	}
	digits := dollars + fraction + strings.Repeat("0", 2-len(fraction))
	if strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, errors.New(message)
	}
	cents, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || cents < 50 {
		return 0, errors.New(message)
	}
	return cents, nil
}

func connectStripeOnDevice(ctx context.Context, payments deviceStripePayments, openBrowser func(context.Context, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	address := listener.Addr().(*net.TCPAddr)
	link, state, err := payments.ConnectPublisherStripeForDevice(ctx, address.Port)
	if err != nil {
		return err
	}
	result := make(chan error, 1)
	var received atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stripe/return", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		code := query.Get("code")
		if r.Host != address.String() || len(query["state"]) != 1 || len(query["code"]) > 1 ||
			subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 ||
			len(code) > 1024 || !received.CompareAndSwap(false, true) {
			http.Error(w, "This Stripe return is invalid or already used.", http.StatusBadRequest)
			return
		}
		var completionErr error
		if code == "" {
			completionErr = errors.New("setup was canceled in Stripe. Run the command again when ready")
		} else {
			completionErr = payments.CompletePublisherStripe(ctx, state, code)
		}
		if completionErr == nil {
			_, _ = fmt.Fprintln(w, "Stripe is connected. Return to qURL to finish setting your price.")
		} else {
			http.Error(w, "Stripe setup did not finish. Return to qURL for details.", http.StatusBadRequest)
		}
		result <- completionErr
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024}
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	go func() { _ = server.Serve(listener) }()
	if openBrowser == nil || openBrowser(ctx, link) != nil {
		return errors.New("could not open Stripe in a browser on this device")
	}
	select {
	case err = <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
