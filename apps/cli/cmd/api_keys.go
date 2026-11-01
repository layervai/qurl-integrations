package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	accountAdminReadScope  = "qurl:read"
	accountAdminWriteScope = "qurl:write"
)

func apiKeysCmd(opts *globalOpts) *cobra.Command {
	var owner string
	command := &cobra.Command{Use: "api-keys", Short: "Manage account API keys with a fresh browser sign-in", Args: noArgs,
		Long: "Sign in in your browser for each account administration command. The account token stays in memory. Normal resource commands keep using this machine's device identity.", RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.PersistentFlags().StringVar(&owner, "owner", "", "account resource owner (required when the account has several owners)")
	var limit int
	var cursor, status string
	list := &cobra.Command{Use: "list", Short: "List one page of account credentials without secrets", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := qurlapi.ValidateAccountKeyListOptions(qurlapi.APIKeyListOptions{Limit: limit, Cursor: cursor, Status: status}); err != nil {
			return exitcode.UsageError(err)
		}
		client, selected, err := opts.accountKeyClient(cmd.Context(), owner, []string{accountAdminReadScope})
		if err != nil {
			return err
		}
		page, err := client.ListAPIKeys(cmd.Context(), qurlapi.APIKeyListOptions{Limit: limit, Cursor: cursor, Status: status})
		if err != nil {
			return err
		}
		return opts.printer().APIKeyPage(selected, page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "page size (1–100)")
	list.Flags().StringVar(&cursor, "cursor", "", "opaque cursor from the previous page")
	list.Flags().StringVar(&status, "status", "active", "credential status: active, revoked or all")
	create := apiKeyCreateCmd(opts, &owner)
	var updateName string
	update := &cobra.Command{Use: "update KEY_ID", Short: "Rename an account credential", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateAccountKeyID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		if err := qurlapi.ValidateAccountKeyName(updateName); err != nil {
			return exitcode.UsageError(err)
		}
		client, selected, err := opts.accountKeyClient(cmd.Context(), owner, []string{accountAdminReadScope, accountAdminWriteScope})
		if err != nil {
			return err
		}
		key, err := client.UpdateAccountAPIKey(cmd.Context(), args[0], updateName)
		if err != nil {
			return err
		}
		return opts.printer().APIKey(selected, key, "")
	}}
	update.Flags().StringVar(&updateName, "name", "", "new credential name (printable text, up to 100 UTF-8 bytes)")
	revoke := &cobra.Command{Use: "revoke KEY_ID", Short: "Revoke an account credential", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := qurlapi.ValidateAccountKeyID(args[0]); err != nil {
			return exitcode.UsageError(err)
		}
		client, selected, err := opts.accountKeyClient(cmd.Context(), owner, []string{accountAdminReadScope, accountAdminWriteScope})
		if err != nil {
			return err
		}
		if err := client.RevokeAccountAPIKey(cmd.Context(), args[0]); err != nil {
			return err
		}
		return opts.printer().APIKeyRevoked(selected, args[0])
	}}
	command.AddCommand(list, create, update, revoke)
	return command
}

func (o *globalOpts) accountKeyClient(ctx context.Context, requestedOwner string, scopes []string) (qurlapi.APIKeyManagement, string, error) {
	o.warnInsecureEndpoint()
	token, err := o.signInAdmin(ctx, o.accountConfig("", ""), o.openBrowser, scopes)
	if err != nil {
		return nil, "", err
	}
	owners, err := qurlapi.AccountOwners(ctx, o.accountConfig(token, ""))
	if err != nil {
		return nil, "", err
	}
	// Recovery prefers a device owner; administration must never choose one implicitly.
	if requestedOwner == "" && len(owners) != 1 {
		return nil, "", exitcode.UsageError(fmt.Errorf("account has several owners; choose --owner from %q", owners))
	}
	owner, err := selectAccountOwner(owners, requestedOwner)
	if err != nil {
		return nil, "", err
	}
	client, err := qurlapi.NewAPIKeyManagement(o.accountConfig(token, owner))
	return client, owner, err
}

func apiKeyCreateCmd(opts *globalOpts, owner *string) *cobra.Command {
	var createName, idempotencyKey string
	var scopes []string
	create := &cobra.Command{Use: "create", Short: "Create an account API key and print its secret", Args: noArgs,
		Long:    "Create an account API key after browser sign-in. The returned secret appears once in stdout (api_key in JSON; the secret alone with --quiet). Keep the result in your secret store. After an uncertain failure, repeat the same name/scopes with the reported --idempotency-key to recover the original response for 24 hours.",
		Example: "  qurl api-keys create --name 'CI reader' --scopes qurl:read --owner 'auth0|account' -o json", RunE: func(cmd *cobra.Command, _ []string) error {
			if idempotencyKey == "" {
				var nonce [32]byte
				if _, err := rand.Read(nonce[:]); err != nil {
					return err
				}
				idempotencyKey = hex.EncodeToString(nonce[:])
			}
			if err := qurlapi.ValidateAccountKeyCreation(createName, scopes, idempotencyKey); err != nil {
				return exitcode.UsageError(err)
			}
			managementScopes := []string{accountAdminReadScope, accountAdminWriteScope}
			for _, scope := range []string{"qurl:agent", "qurl:keys"} {
				if slices.Contains(scopes, scope) {
					managementScopes = append(managementScopes, scope)
				}
			}
			client, selected, err := opts.accountKeyClient(cmd.Context(), *owner, managementScopes)
			if err != nil {
				return err
			}
			key, err := client.CreateAccountAPIKey(cmd.Context(), createName, scopes, idempotencyKey)
			if err == nil {
				err = opts.printer().APIKey(selected, &key.APIKeyMetadata, key.Secret)
			}
			if err != nil {
				return fmt.Errorf("API key creation result unavailable; recover the same request with --idempotency-key %q: %w", idempotencyKey, err)
			}
			return nil
		}}
	create.Flags().StringVar(&createName, "name", "", "credential name (printable text, up to 100 UTF-8 bytes)")
	create.Flags().StringSliceVar(&scopes, "scopes", nil, "explicit comma-separated scopes (for example qurl:read,qurl:write)")
	create.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "recover an earlier create with the same name/scopes (32–256 characters)")
	return create
}
