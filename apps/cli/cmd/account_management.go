package main

import (
	"context"
	"fmt"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const accountListOperation = "list"

func (o *globalOpts) accountManagementClient(ctx context.Context, write bool) (qurlapi.AccountManagement, string, error) {
	o.warnInsecureEndpoint()
	scopes := []string{accountAdminReadScope}
	if write {
		scopes = append(scopes, accountAdminWriteScope)
	}
	token, err := o.signInAdmin(ctx, o.accountConfig("", ""), o.openBrowser, scopes)
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
	if identity.AuthType != "jwt" || identity.Key != nil {
		return nil, "", fmt.Errorf("%w: account administration requires browser account authentication", qurl.ErrInvalidAPIResponse)
	}
	return client, identity.OwnerID, nil
}
