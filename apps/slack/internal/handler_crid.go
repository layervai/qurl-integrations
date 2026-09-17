package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackaudit"
)

// cridUsageMessage is separate from getUsageMessage because CRIDs are pasted
// without the `$` sigil and do not enter the channel alias resolver.
const cridUsageMessage = "Usage: `/qurl crid <CRID>` to create a qURL directly from a resource's permanent identifier."

// handleCRID implements `/qurl crid <CRID>`. Its asynchronous acknowledgement
// and Enter Portal rendering intentionally match `/qurl get`, while its mint
// path uses the SDK's CRID-addressed /share endpoint. Like `/qurl get`, Slack
// trusts the authenticated qurl-service response and accepts only a safe HTTPS
// link before it is rendered.
func (h *Handler) handleCRID(w http.ResponseWriter, values url.Values) {
	cmd, err := Parse(strings.TrimSpace(values.Get(fieldText)))
	if err != nil {
		if errors.Is(err, ErrInvalidCRID) {
			respondSlack(w, ":warning: "+cridUsageMessage)
			return
		}
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	if cmd.Subcommand != SubcmdCRID || cmd.CRID == "" {
		respondSlack(w, ":warning: "+cridUsageMessage)
		return
	}

	h.runAsync(w, string(SubcmdCRID), values, func(ctx context.Context, log *slog.Logger) {
		h.processCRID(ctx, log, values, cmd)
	})
}

func (h *Handler) processCRID(ctx context.Context, log *slog.Logger, values url.Values, cmd *Command) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	channelID := values.Get(fieldChannelID)
	userID := values.Get(fieldUserID)

	if channelID == "" {
		log.Warn("crid: empty channel_id; refusing channel-less invocation")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return
	}

	res, err := h.cridWork(ctx, log, cmd, teamID, enterpriseID, channelID, userID)
	h.finishGet(log, responseURL, res, err)
}

// cridWork is the Slack equivalent of `/qurl get` for a permanent CRID. It
// deliberately uses the same authenticated-service trust model as get: the
// SDK validates the CRID-addressed share response, and Slack validates that its
// rendered button URL is safe HTTPS. CLI-only link-signature verification needs
// deployment trust that the Slack app does not own.
func (h *Handler) cridWork(ctx context.Context, log *slog.Logger, cmd *Command, teamID, enterpriseID, channelID, userID string) (getResult, error) {
	if cmd.DM() && h.cfg.PostDMBlocks == nil {
		return getResult{}, &userError{msg: "DM delivery is not configured for this workspace. Re-run the command without `dm:true` to receive the link in-channel."}
	}
	if h.cfg.AuthProvider == nil || strings.TrimSpace(h.cfg.QURLEndpoint) == "" {
		log.Error("crid: required share configuration is missing")
		return getResult{}, &userError{msg: authFailureMessage}
	}

	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		log.Error("crid: API key lookup failed", "error", err)
		return getResult{}, &userError{msg: authErrorMessage(err)}
	}
	sdk, err := qurl.NewClient(qurl.BearerToken(apiKey), qurl.WithBaseURL(h.cfg.QURLEndpoint))
	if err != nil {
		log.Error("crid: create share client failed", "error", err)
		return getResult{}, &userError{msg: commonGetMintFailedMessage}
	}
	link, err := sdk.ShareResource(ctx, cmd.CRID, nil)
	if err != nil {
		log.Warn("crid: share failed", "error", err)
		return getResult{}, &userError{msg: commonGetMintFailedMessage}
	}
	if !isHTTPSURL(link.Link) {
		log.Error("crid: share returned empty or non-https qurl link", "has_link", link.Link != "")
		return getResult{}, &userError{msg: commonGetMintFailedMessage}
	}
	if reason := strings.TrimSpace(cmd.Reason()); reason != "" {
		slackaudit.LogQURLMintReason(log, slackaudit.QURLMintReasonAttrs(
			teamID, channelID, userID, cmd.CRID, truncateRunes(reason, getReasonAuditMaxRunes),
		)...)
	}

	fallbackText, blocks := renderGetSuccess(link.Link)
	if cmd.DM() {
		return getResult{text: h.deliverGetDM(ctx, log, teamID, enterpriseID, userID, fallbackText, blocks)}, nil
	}
	return getResult{text: fallbackText, blocks: blocks}, nil
}
