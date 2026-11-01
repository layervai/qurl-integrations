package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const accountListOperation = "list"

func (o *globalOpts) accountManagementClient(ctx context.Context, write bool) (qurlapi.AccountManagement, string, error) {
	o.warnInsecureEndpoint()
	scopes := []string{accountAdminReadScope}
	if write {
		scopes = append(scopes, accountAdminWriteScope)
	}
	var token string
	var err error
	if o.accountAPIKeyAuth {
		token, _, err = auth.Resolve(o.lookupEnv)
		if errors.Is(err, auth.ErrNoCredential) {
			return nil, "", exitcode.UsageError(errors.New("--api-key-auth needs QURL_API_KEY_FILE or QURL_API_KEY"))
		}
		if err == nil {
			err = auth.ValidateKeyShape(token)
		}
	} else {
		token, err = o.signInAdmin(ctx, o.accountConfig("", ""), o.openBrowser, scopes)
	}
	if err != nil {
		return nil, "", err
	}
	client, err := qurlapi.NewAccountManagement(o.accountConfig(token, ""))
	if err != nil {
		return nil, "", err
	}
	identity, err := client.Me(ctx)
	if err != nil {
		return nil, "", err
	}
	if o.accountAPIKeyAuth {
		if identity.AuthType != "api_key" || identity.Key == nil || identity.Key.Kind != "api_key" || identity.Key.KeyID == "" {
			return nil, "", fmt.Errorf("%w: account automation needs a durable account API key; device and bootstrap credentials are not allowed", auth.ErrAccountManagementDenied)
		}
		for _, scope := range scopes {
			if !slices.Contains(identity.Key.Scopes, scope) {
				return nil, "", fmt.Errorf("%w: account automation requires %s", auth.ErrAccountManagementDenied, strings.Join(scopes, ", "))
			}
		}
		return client, identity.OwnerID, nil
	}
	if identity.AuthType != "jwt" || identity.Key != nil {
		return nil, "", fmt.Errorf("%w: account administration requires browser account authentication", qurl.ErrInvalidAPIResponse)
	}
	return client, identity.OwnerID, nil
}
