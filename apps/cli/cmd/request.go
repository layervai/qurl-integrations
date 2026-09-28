package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
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
local and transport failures exit nonzero. Requests are never retried.

The body limit is 1 MiB including surrounding whitespace. The request reads
standard input to end of file: redirect it from the null device when there is
no body, and impose a deadline on the process.

The envelope is returned unvalidated. For POST /v1/account/link, record the
link only after status 200 with an owner_id matching this device and an
account_id matching the intended account.`,
		Example: "  # Redirect stdin from the null device (/dev/null, or NUL on Windows) when there is no body.\n  qurl request GET /v1/me -o json < /dev/null",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.resolvedFormat != output.FormatJSON {
				return exitcode.UsageError(errors.New("request requires --output json"))
			}
			if err := qurlapi.ValidateRequestIdempotencyKey(idempotencyKey); err != nil {
				return exitcode.UsageError(err)
			}
			if err := qurlapi.ValidateRequestTarget(args[0], args[1]); err != nil {
				return exitcode.UsageError(err)
			}
			var body []byte
			if !opts.streams.InIsTTY {
				var err error
				body, err = io.ReadAll(io.LimitReader(opts.streams.In, qurlapi.MaxRequestBody+1))
				if err != nil {
					return fmt.Errorf("could not read request body: %w", err)
				}
				if len(body) > qurlapi.MaxRequestBody {
					return exitcode.UsageError(errors.New("request body exceeds 1 MiB including surrounding whitespace"))
				}
				body = bytes.TrimSpace(body)
				if len(body) > 0 && !json.Valid(body) {
					return exitcode.UsageError(errors.New("request body must be JSON"))
				}
			}
			if (args[0] == "GET" || args[0] == "DELETE") && len(body) > 0 {
				return exitcode.UsageError(errors.New("GET and DELETE requests must not include a body"))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			reply, err := qurlapi.Request(cmd.Context(), client, args[0], args[1], body, idempotencyKey)
			if err != nil {
				return err
			}
			return opts.printer().Request(reply)
		},
	}
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "stable nonsecret mutation key (32-256 letters, digits, hyphens or underscores)")
	return cmd
}
