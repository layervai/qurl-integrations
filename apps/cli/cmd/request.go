package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func requestCmd(opts *globalOpts) *cobra.Command {
	var idempotencyKey string
	cmd := &cobra.Command{
		Use: "request METHOD PATH", Short: "Make a device-authorized JSON request for a supervising app",
		Long:    "Read an optional JSON body from standard input and return status, safe headers, and body as JSON. Only the registered device's existing resource routes are allowed. HTTP errors are returned in the envelope with exit zero; local and transport failures exit nonzero. Requests are never retried.",
		Example: "  qurl request GET /v1/me -o json < /dev/null",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.resolvedFormat != output.FormatJSON {
				return exitcode.UsageError(errors.New("request requires --output json"))
			}
			if err := qurlapi.ValidateRequestIdempotencyKey(idempotencyKey); err != nil {
				return exitcode.UsageError(err)
			}
			if err := qurlapi.ValidateRequestPath(args[1]); err != nil {
				return exitcode.UsageError(err)
			}
			switch args[0] {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				return exitcode.UsageError(errors.New("request method must be GET, POST, PUT, PATCH, or DELETE"))
			}
			var body []byte
			if !opts.streams.InIsTTY {
				var err error
				body, err = io.ReadAll(io.LimitReader(opts.streams.In, (1<<20)+1))
				if err != nil {
					return errors.New("could not read request body")
				}
				if len(body) > 1<<20 {
					return exitcode.UsageError(errors.New("request body exceeds 1 MiB"))
				}
				body = bytes.TrimSpace(body)
				if len(body) > 0 && !json.Valid(body) {
					return exitcode.UsageError(errors.New("request body must be JSON of at most 1 MiB"))
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
			return json.NewEncoder(opts.streams.Out).Encode(reply)
		},
	}
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "stable nonsecret mutation key (32-256 letters, digits, hyphens or underscores)")
	return cmd
}
