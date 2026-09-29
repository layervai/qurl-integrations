package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func requestCmd(opts *globalOpts) *cobra.Command {
	var idempotencyKey string
	cmd := &cobra.Command{
		Use:   "request METHOD PATH",
		Short: "Make a device-authorized JSON request for a supervising app",
		Long: `Read an optional JSON body from standard input and return status, safe
headers, and body as JSON. Only the registered device's existing resource
routes are allowed. HTTP errors are returned in the envelope with exit zero;
local and transport failures exit nonzero. The CLI never retries; a
supervisor that retries a mutation passes the same --idempotency-key, which
protects only routes where the qURL service implements idempotency.

Only externally supervised namespaces are accepted, so the command never
enrolls a device implicitly. POST, PUT, and PATCH read standard input to end
of file; redirect it from the null device when there is no body, and impose a
deadline on the process. GET and DELETE never read standard input. The body
limit is 1 MiB including surrounding whitespace.

The envelope is returned unvalidated. For POST /v1/account/link, record the
link only after status 200 with an owner_id matching this device and an
account_id matching the intended account.`,
		Example: "  qurl request GET /v1/me --supervision external -o json\n  # Redirect stdin from the null device (/dev/null, or NUL on Windows) when a mutation has no body.\n  qurl request DELETE /v1/resources/r_abc/sessions --supervision external -o json",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRequestInvocation(opts, args[0], args[1], idempotencyKey); err != nil {
				return err
			}
			body, err := readRequestBody(opts.streams, args[0])
			if err != nil {
				return err
			}
			client, err := openRequestClient(cmd.Context(), opts)
			if err != nil {
				return err
			}
			reply, err := qurlapi.Request(cmd.Context(), client, args[0], args[1], body, idempotencyKey)
			if err != nil {
				return err
			}
			return opts.printer().RequestEnvelope(reply)
		},
	}
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "stable nonsecret key a supervisor reuses when retrying a mutation; effective only where the service implements idempotency (32-256 letters, digits, hyphens or underscores)")
	return cmd
}

// readRequestBody reads a bounded JSON body for POST, PUT and PATCH. GET and
// DELETE never read stdin, so an inherited pipe cannot stall them.
func readRequestBody(streams *output.Streams, method string) ([]byte, error) {
	if method == http.MethodGet || method == http.MethodDelete {
		return nil, nil
	}
	if streams.InIsTTY {
		return nil, exitcode.UsageError(errors.New("request reads a " + method + " body from standard input; pipe JSON or redirect it from the null device"))
	}
	body, err := io.ReadAll(io.LimitReader(streams.In, qurlapi.MaxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("could not read request body: %w", err)
	}
	if len(body) > qurlapi.MaxRequestBody {
		return nil, exitcode.UsageError(fmt.Errorf("request body exceeds %d MiB including surrounding whitespace", qurlapi.MaxRequestBody>>20))
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && !json.Valid(body) {
		return nil, exitcode.UsageError(errors.New("request body must be JSON"))
	}
	return body, nil
}

// validateRequestInvocation checks everything local before a device can be
// opened; Request re-checks the target and key as the library contract.
func validateRequestInvocation(opts *globalOpts, method, target, idempotencyKey string) error {
	if opts.resolvedFormat != output.FormatJSON {
		return exitcode.UsageError(errors.New("request requires --output json"))
	}
	if opts.resolvedSupervision != connectorstate.RuntimeSupervisionExternal {
		return exitcode.UsageError(errors.New("request requires --supervision external (or " + connectorstate.EnvRuntimeSupervision + "=external)"))
	}
	// The device must already exist: account authority must never enroll or
	// recover it on the supervisor's behalf.
	if accountKeyConfigured(opts.lookupEnv) {
		return exitcode.UsageError(fmt.Errorf("request cannot be combined with %s or %s", auth.EnvAPIKey, auth.EnvAPIKeyFile))
	}
	if err := requireLocalKeyProvider(opts.lookupEnv, "request"); err != nil {
		return exitcode.UsageError(err)
	}
	if err := qurlapi.ValidateRequestIdempotencyKey(idempotencyKey); err != nil {
		return exitcode.UsageError(err)
	}
	if err := qurlapi.ValidateRequestTarget(method, target); err != nil {
		return exitcode.UsageError(err)
	}
	if idempotencyKey != "" && method == http.MethodGet {
		return exitcode.UsageError(errors.New("--idempotency-key applies only to mutations"))
	}
	return nil
}

// openRequestClient opens the existing supervised device. Account keys were
// refused before this, so a device that needs recovery gets the supervised
// remedy, and a bare missing credential means no enrolled device.
func openRequestClient(ctx context.Context, opts *globalOpts) (qurlapi.Client, error) {
	client, err := opts.newClient(ctx)
	switch {
	case errors.Is(err, auth.ErrAnonymousRecovery):
		return nil, fmt.Errorf("%w: %w", auth.ErrExternalAnonymousRecovery, err)
	case errors.Is(err, auth.ErrNoCredential) && !errors.Is(err, auth.ErrAnonymousRecovery) && !errors.Is(err, auth.ErrAccountRecoveryState):
		return nil, fmt.Errorf("%w: %w", auth.ErrExternalDeviceMissing, err)
	case err != nil:
		return nil, err
	}
	// Refuse a connector-scoped device with login's remedy rather than a raw
	// service refusal on the first management call.
	if opts.nativeRuntime != nil {
		store, err := opts.nativeRuntime.Handoff()
		if err != nil {
			return nil, err
		}
		if err := requireExternalOwnerScopedAgentState(ctx, store); err != nil {
			return nil, err
		}
	}
	return client, nil
}
