package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

type deviceStripePayments interface {
	PublisherStripeAccount(context.Context) (string, error)
	ConnectPublisherStripeForDevice(context.Context, int) (string, error)
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
			cents, err := paidAccessCents(amount)
			if err != nil {
				return exitcode.UsageError(err)
			}
			if version < 0 || version >= 9007199254740991 ||
				(interval != "" && interval != "month" && interval != "year") ||
				accessSeconds < 0 || accessSeconds > 315360000 || (interval != "" && accessSeconds != 0) {
				return exitcode.UsageError(errors.New("check the policy version. Use month or year for subscriptions. Set an access duration only for a one-time payment"))
			}
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			if err = applyCRIDGuards(opts.printer(), assessment, opts.productionEndpoint(), yes); err != nil {
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
			if len(resource.CRID) != 60 {
				return errors.New("the service did not return the full CRID")
			}
			current := resource.ConsumerAccess
			currentVersion := resource.ConsumerAccessVersion
			if current != nil {
				currentVersion = current.Version
			}
			retry := current != nil && current.Version == version+1 && current.Mode == "contract" && current.ContractID == "paid-access" && current.Payment != nil
			if currentVersion != version && !retry {
				return exitcode.UsageError(fmt.Errorf("the current policy version is %d. Review it and retry with --version %d to replace it", currentVersion, currentVersion))
			}
			checkoutName := strings.TrimSpace(name)
			if !cmd.Flags().Changed("name") {
				preview, previewErr := client.Preview(ctx, resource.CRID)
				if previewErr != nil {
					return previewErr
				}
				checkoutName = preview.Name
			}
			if !validPublicName(checkoutName) {
				return exitcode.UsageError(errors.New("set --name to the app name buyers should see at checkout"))
			}
			management := client.ConsumerManagement()
			payments, ok := management.(deviceStripePayments)
			if !ok {
				return errors.New("publisher payments are unavailable in this client")
			}
			account, err := payments.PublisherStripeAccount(ctx)
			if err != nil {
				return err
			}
			if account == "" {
				opts.printer().Notef("Complete Stripe setup in your browser.")
				if err = connectStripeOnDevice(ctx, payments, opts.openBrowser); err != nil {
					return err
				}
			}
			price, err := payments.CreatePublisherStripePrice(ctx, checkoutName, cents, interval)
			if err != nil {
				return err
			}
			price.AccessSeconds = accessSeconds
			if retry && *current.Payment != *price {
				return exitcode.UsageError(fmt.Errorf("this CRID has a different paid offer at version %d. Review it before replacing it with --version %d", current.Version, current.Version))
			}
			policy := qurl.ConsumerAccessPolicy{Mode: "contract", Version: version + 1, ContractID: "paid-access", Payment: price}
			if err = management.UpdateConsumerAccess(ctx, assessment.Input, version, policy); err != nil {
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
	link, err := payments.ConnectPublisherStripeForDevice(ctx, address.Port)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "connect.stripe.com" ||
		parsed.User != nil || parsed.Path != "/oauth/authorize" || parsed.Query().Get("state") == "" {
		return errors.New("the API returned an invalid Stripe consent link")
	}
	state := parsed.Query().Get("state")
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
		if r.Host != address.String() || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 ||
			len(code) > 1024 || !received.CompareAndSwap(false, true) {
			http.Error(w, "This Stripe return is invalid or already used.", http.StatusBadRequest)
			return
		}
		var completionErr error
		if code == "" {
			completionErr = errors.New("Stripe setup was canceled. Run the command again when ready.")
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
