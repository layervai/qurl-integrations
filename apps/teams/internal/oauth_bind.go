package internal

import (
	"context"
	"errors"
	"net/http"

	"github.com/layervai/qurl-integrations/apps/teams/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/teams/internal/teamsdata"
)

type oauthAdminStoreAdapter struct {
	store *teamsdata.Store
}

func NewOAuthAdminStore(store *teamsdata.Store) oauth.AdminStore {
	if store == nil {
		return nil
	}
	return &oauthAdminStoreAdapter{store: store}
}

func (a *oauthAdminStoreAdapter) BindWorkspace(ctx context.Context, m *oauth.WorkspaceMapping, seedAdmin string) error {
	return a.store.BindWorkspace(ctx, &teamsdata.WorkspaceMapping{
		TenantID:  m.TenantID,
		OwnerID:   m.OwnerID,
		CreatedAt: m.CreatedAt,
	}, seedAdmin)
}

func ClassifyOAuthBindError(err error) oauth.BindConflictCode {
	var terr *teamsdata.Error
	if !errors.As(err, &terr) || terr.StatusCode != http.StatusConflict {
		return ""
	}
	switch terr.Code {
	case teamsdata.ErrCodeWorkspaceAlreadyBoundToCaller:
		return oauth.BindConflictAlreadyBoundToCaller
	case teamsdata.ErrCodeWorkspaceAlreadyBound:
		return oauth.BindConflictAlreadyBound
	default:
		return ""
	}
}
