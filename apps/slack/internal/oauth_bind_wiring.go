package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// SlackdataBinder is the slice of slackdata.Store needed to bridge the OAuth
// callback's package-local WorkspaceMapping shape to the real workspace store.
type SlackdataBinder interface {
	BindWorkspace(ctx context.Context, m *slackdata.WorkspaceMapping, seedAdmin string) error
}

type oauthAdminStoreAdapter struct {
	store SlackdataBinder
}

// NewOAuthAdminStoreAdapter bridges *slackdata.Store to oauth.AdminStore.
func NewOAuthAdminStoreAdapter(store SlackdataBinder) oauth.AdminStore {
	return &oauthAdminStoreAdapter{store: store}
}

// BindWorkspace translates the OAuth mapping shape into slackdata's store shape.
func (a *oauthAdminStoreAdapter) BindWorkspace(ctx context.Context, m *oauth.WorkspaceMapping, seedAdmin string) error {
	return a.store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{
		TeamID:    m.TeamID,
		OwnerID:   m.OwnerID,
		CreatedAt: m.CreatedAt,
	}, seedAdmin)
}

// ClassifyOAuthBindError maps slackdata.Store's 409 bind conflicts to the
// oauth callback codes that decide idempotent re-entry vs. rebind refusal.
func ClassifyOAuthBindError(err error) oauth.BindConflictCode {
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		return ""
	}
	switch ae.Code {
	case slackdata.ErrCodeWorkspaceAlreadyBoundToCaller:
		return oauth.BindConflictAlreadyBoundToCaller
	case slackdata.ErrCodeWorkspaceAlreadyBound:
		return oauth.BindConflictAlreadyBound
	case slackdata.ErrCodeWorkspaceBindUnverified:
		return oauth.BindConflictUnverified
	default:
		// A 409 from slackdata with an unmapped Code means a new
		// conflict variant was added on the producer side without
		// this classifier being updated. Surface a warn so on-call
		// sees the drift on CloudWatch before users start reporting
		// "every rebind 500s."
		slog.Warn("classifyBindError: slackdata returned 409 with unmapped Code; defaulting to generic 500 (classifier and slackdata.ErrCodeWorkspace* have drifted)",
			"code", ae.Code, "title", ae.Title)
		return ""
	}
}
