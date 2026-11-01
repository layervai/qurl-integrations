package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	webhookListOperation   = "list"
	webhookCreateOperation = "create"
)

func webhooksCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "webhooks", Short: "Manage account webhooks with a fresh browser sign-in", Args: noArgs,
		Long: "Each command signs in to your browser account. Linked device-owner selection is not supported. Writes make one HTTP attempt. Creation and secret rotation have no idempotent replay guarantee. The signing secret is printed only by create and rotate-secret. Delivery response text is omitted because it can contain endpoint secrets.", RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.AddCommand(webhookListCmd(opts), webhookGetCmd(opts), webhookWriteCmd(opts, true), webhookWriteCmd(opts, false), webhookDeleteCmd(opts), webhookRotateCmd(opts), webhookEventsCmd(opts), webhookDeliveriesCmd(opts))
	return command
}

func webhookListCmd(opts *globalOpts) *cobra.Command {
	var limit int
	var cursor string
	list := &cobra.Command{Use: webhookListOperation, Short: "List one page of webhook metadata", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := qurlapi.ValidateWebhookPage(limit); err != nil {
			return exitcode.UsageError(err)
		}
		client, owner, err := opts.accountWebhookClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		page, err := client.ListWebhooks(cmd.Context(), limit, cursor)
		if err != nil {
			return err
		}
		for i := range page.Data {
			if page.Data[i].OwnerID != owner {
				return fmt.Errorf("%w: webhook owner differs from signed-in account", qurl.ErrInvalidAPIResponse)
			}
		}
		return opts.printer().WebhookPage(page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "page size (1–100)")
	list.Flags().StringVar(&cursor, "cursor", "", "opaque cursor from the previous page")
	return list
}

func webhookGetCmd(opts *globalOpts) *cobra.Command {
	get := &cobra.Command{Use: "get WEBHOOK_ID", Short: "Read webhook metadata without its secret", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateWebhookID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		client, owner, err := opts.accountWebhookClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		item, err := client.GetWebhook(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if item.OwnerID != owner {
			return fmt.Errorf("%w: webhook owner differs from signed-in account", qurl.ErrInvalidAPIResponse)
		}
		return opts.printer().Webhook(item, "")
	}}
	return get
}

func webhookDeleteCmd(opts *globalOpts) *cobra.Command {
	remove := &cobra.Command{Use: "delete WEBHOOK_ID", Short: "Delete a webhook subscription", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateWebhookID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountWebhookClient(cmd.Context(), true)
		if err != nil {
			return err
		}
		if err := client.DeleteWebhook(cmd.Context(), args[0]); err != nil {
			return err
		}
		return opts.printer().WebhookDeleted(args[0])
	}}
	return remove
}

func webhookRotateCmd(opts *globalOpts) *cobra.Command {
	rotate := &cobra.Command{Use: "rotate-secret WEBHOOK_ID", Short: "Replace the signing secret and print it once", Args: exactArgs(1), Long: "Immediately invalidates the old signing secret. Configure your receiver with the new secret. This command never retries automatically. After an uncertain result or failed output, explicitly rotate again and configure the latest secret.", RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateWebhookID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		client, owner, err := opts.accountWebhookClient(cmd.Context(), true)
		if err != nil {
			return err
		}
		item, err := client.RotateWebhookSecret(cmd.Context(), args[0])
		if err != nil {
			return webhookSecretFailure("rotate-secret", args[0], err)
		}
		if item.OwnerID != owner {
			return webhookSecretFailure("rotate-secret", args[0], fmt.Errorf("%w: webhook owner differs from signed-in account", qurl.ErrInvalidAPIResponse))
		}
		if err := opts.printer().Webhook(&item.Webhook, item.Secret); err != nil {
			return webhookSecretFailure("rotate-secret", args[0], err)
		}
		return nil
	}}
	return rotate
}

func webhookEventsCmd(opts *globalOpts) *cobra.Command {
	events := &cobra.Command{Use: "events", Short: "Read the server event catalog", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := opts.accountWebhookClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		items, err := client.WebhookEvents(cmd.Context())
		if err != nil {
			return err
		}
		return opts.printer().WebhookEvents(items)
	}}
	return events
}

func webhookDeliveriesCmd(opts *globalOpts) *cobra.Command {
	var deliveryLimit int
	var deliveryCursor string
	deliveries := &cobra.Command{Use: "deliveries WEBHOOK_ID", Short: "List one page of delivery metadata", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateWebhookID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		if err := qurlapi.ValidateWebhookPage(deliveryLimit); err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountWebhookClient(cmd.Context(), false)
		if err != nil {
			return err
		}
		page, err := client.WebhookDeliveries(cmd.Context(), args[0], deliveryLimit, deliveryCursor)
		if err != nil {
			return err
		}
		return opts.printer().WebhookDeliveries(page)
	}}
	deliveries.Flags().IntVar(&deliveryLimit, "limit", 20, "page size (1–100)")
	deliveries.Flags().StringVar(&deliveryCursor, "cursor", "", "opaque cursor from the previous page")
	return deliveries
}

func webhookWriteCmd(opts *globalOpts, create bool) *cobra.Command {
	var endpoint, description, status string
	var events []string
	name, short, args := "update WEBHOOK_ID", "Update supplied webhook fields", exactArgs(1)
	if create {
		name, short, args = webhookCreateOperation, "Create a webhook and print its one-time secret", noArgs
	}
	command := &cobra.Command{Use: name, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
		var input qurlapi.WebhookInput
		id := ""
		if !create {
			id = args[0]
			if err := qurlapi.ValidateWebhookID(id); err != nil {
				return exitcode.UsageError(err)
			}
		}
		if create || cmd.Flags().Changed("url") {
			input.URL = &endpoint
		}
		if create || cmd.Flags().Changed("events") {
			input.Events = &events
		}
		if cmd.Flags().Changed("description") {
			input.Description = &description
		}
		if !create && cmd.Flags().Changed("status") {
			input.Status = &status
		}
		if err := qurlapi.ValidateWebhookInput(input, create); err != nil {
			return exitcode.UsageError(err)
		}
		return opts.runWebhookWrite(cmd.Context(), id, input, create)
	}}
	command.Flags().StringVar(&endpoint, "url", "", "HTTPS receiver URL without credentials or a fragment")
	command.Flags().StringSliceVar(&events, "events", nil, "comma-separated event names; see webhooks events")
	command.Flags().StringVar(&description, "description", "", "description (up to 500 characters; empty clears it)")
	if !create {
		command.Flags().StringVar(&status, "status", "", "active or disabled")
	}
	return command
}

func (o *globalOpts) runWebhookWrite(ctx context.Context, id string, input qurlapi.WebhookInput, create bool) error {
	client, owner, err := o.accountWebhookClient(ctx, true)
	if err != nil {
		return err
	}
	if create {
		item, err := client.CreateWebhook(ctx, input)
		if err != nil {
			return webhookSecretFailure(webhookCreateOperation, "", err)
		}
		if item.OwnerID != owner {
			return webhookSecretFailure(webhookCreateOperation, item.ID, fmt.Errorf("%w: webhook owner differs from signed-in account", qurl.ErrInvalidAPIResponse))
		}
		if err := o.printer().Webhook(&item.Webhook, item.Secret); err != nil {
			return webhookSecretFailure(webhookCreateOperation, item.ID, err)
		}
		return nil
	}
	item, err := client.UpdateWebhook(ctx, id, input)
	if err != nil {
		return err
	}
	if item.OwnerID != owner {
		return fmt.Errorf("%w: webhook owner differs from signed-in account", qurl.ErrInvalidAPIResponse)
	}
	return o.printer().Webhook(item, "")
}

func (o *globalOpts) accountWebhookClient(ctx context.Context, write bool) (qurlapi.WebhookManagement, string, error) {
	o.warnInsecureEndpoint()
	scopes := []string{accountAdminReadScope}
	if write {
		scopes = append(scopes, accountAdminWriteScope)
	}
	token, err := o.signInAdmin(ctx, o.accountConfig("", ""), o.openBrowser, scopes)
	if err != nil {
		return nil, "", err
	}
	client, err := qurlapi.NewWebhookManagement(o.accountConfig(token, ""))
	if err != nil {
		return nil, "", err
	}
	identity, err := client.Me(ctx)
	if err != nil {
		return nil, "", err
	}
	if identity.AuthType != "jwt" || identity.Key != nil {
		return nil, "", fmt.Errorf("%w: webhooks require browser account authentication", qurl.ErrInvalidAPIResponse)
	}
	return client, identity.OwnerID, nil
}

type webhookSecretResultError struct {
	operation, id string
	cause         error
}

func (e *webhookSecretResultError) Error() string { return e.UserMessage() }
func (e *webhookSecretResultError) Unwrap() error { return e.cause }
func (e *webhookSecretResultError) UserMessage() string {
	if e.operation == webhookCreateOperation && e.id == "" {
		return "Webhook creation outcome is uncertain. Run qurl webhooks list before another create. If it exists, use rotate-secret to obtain a new secret and configure your receiver."
	}
	return fmt.Sprintf("Webhook secret output is uncertain. Explicitly run qurl webhooks rotate-secret %s and configure your receiver with the latest secret.", e.id)
}
func webhookSecretFailure(operation, id string, cause error) error {
	var apiError *qurlapi.Error
	if errors.As(cause, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 {
		return cause
	}
	return &webhookSecretResultError{operation: operation, id: id, cause: cause}
}
