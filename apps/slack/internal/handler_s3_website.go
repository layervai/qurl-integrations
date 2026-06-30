package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	defaultS3StaticConnectorImage = "ghcr.io/layervai/qurl-integrations/s3-static-connector:main"
	defaultS3WebsiteIndexDocument = "index.html"
	// origins/s3-static-connector listens on 127.0.0.1:8080 by default.
	s3WebsiteOriginPort              = defaultTunnelLocalPort
	s3WebsiteUnexpectedFailureNotice = "S3 website qURL Connector setup stopped unexpectedly before install instructions were confirmed. If you received a bootstrap-key DM from this attempt, discard it and run `/qurl-admin protect` again."
	s3WebsiteECSLogGroup             = "/ecs/qurl-s3-website"
)

var (
	s3WebsiteBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	s3WebsiteRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[1-9]\d*$`)
	s3WebsitePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	s3WebsiteIndexPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type s3WebsiteInstallArgs struct {
	Slug          string
	Alias         string
	Environment   tunnelInstallEnvironment
	Bucket        string
	Region        string
	Prefix        string
	IndexDocument string
}

type s3WebsiteInstallRequest struct {
	teamID       string
	enterpriseID string
	channelID    string
	userID       string
	responseURL  string
	args         *s3WebsiteInstallArgs
	attemptID    string
}

type preparedS3WebsiteInstallMessage struct {
	connectorImage   string
	originImage      string
	environmentLabel string
	instructions     string
	imageNote        string
}

func (h *Handler) handleConnectorSetupSubmission(w http.ResponseWriter, payload *ViewSubmission) {
	setupType := strings.TrimSpace(interactionStateText(payload.View.State.Values, connectorSetupBlockType, connectorSetupActionType))
	if setupType != connectorSetupExistingService && setupType != connectorSetupS3Website {
		respondViewErrors(w, map[string]string{connectorSetupBlockType: "Choose one of the listed qURL Connector setup types."})
		return
	}

	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("connector setup modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("connector setup modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID || payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("connector setup modal identity mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	// This chooser only opens the setup-specific modal; the mutating install
	// submissions enforce TTL from this freshly rendered modal.
	meta.CreatedAtUnix = h.now().Unix()

	var (
		view []byte
		err  error
	)
	switch setupType {
	case connectorSetupExistingService:
		view, err = TunnelInstallModal(&meta)
	case connectorSetupS3Website:
		view, err = S3WebsiteInstallModal(&meta)
	}
	if err != nil {
		slog.Error("connector setup next modal render failed", "error", err, "setup_type", setupType)
		respondTunnelInstallModalError(w, "Could not open qURL Connector setup. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: respActionUpdate,
		respFieldView:           json.RawMessage(view),
	})
}

func (h *Handler) handleS3WebsiteInstallSubmission(w http.ResponseWriter, payload *ViewSubmission) {
	args, fieldErrors := parseS3WebsiteInstallModalArgs(payload.View.State.Values)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("S3 website install modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondS3WebsiteInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("S3 website install modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondS3WebsiteInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	log := slog.With(
		"command", "s3_website_install_modal",
		"team_id", meta.TeamID,
		"channel_id", meta.ChannelID,
		"user_id", meta.UserID,
		"view_id", payload.View.ID,
	)
	req := &s3WebsiteInstallRequest{
		teamID:       meta.TeamID,
		enterpriseID: meta.EnterpriseID,
		channelID:    meta.ChannelID,
		userID:       meta.UserID,
		responseURL:  meta.ResponseURL,
		args:         args,
	}

	modalAge := h.now().Sub(time.Unix(meta.CreatedAtUnix, 0))
	if meta.CreatedAtUnix <= 0 || modalAge > tunnelInstallModalTTL || modalAge < -tunnelBootstrapSkew {
		log.Warn("S3 website install modal expired", "created_at_unix", meta.CreatedAtUnix, "modal_age_ms", modalAge.Milliseconds())
		respondS3WebsiteInstallModalError(w, "This modal expired. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		log.Warn("S3 website install modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID)
		respondS3WebsiteInstallModalError(w, "This modal was opened for a different workspace. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		log.Warn("S3 website install modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID)
		respondS3WebsiteInstallModalError(w, "Only the admin who opened this modal can submit it. Run /qurl-admin protect and choose qURL Connector again to start a new setup.")
		return
	}
	if h.cfg.AdminStore == nil {
		respondS3WebsiteInstallModalError(w, "Admin features are not configured on this Secure Access Agent deployment.")
		return
	}
	if h.aliasStore == nil {
		respondS3WebsiteInstallModalError(w, "Channel alias storage is not configured on this Secure Access Agent deployment.")
		return
	}

	adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, meta.TeamID, meta.UserID)
	if err != nil {
		log.Error("S3 website install modal admin check failed", "error", err)
		respondS3WebsiteInstallModalError(w, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		log.Warn("S3 website install modal denied: non-admin")
		respondS3WebsiteInstallModalError(w, "This command is admin-only.")
		return
	}

	setupStartedAt := time.Unix(meta.CreatedAtUnix, 0)
	req.attemptID = tunnelBootstrapModalAttemptID(payload.View.ID, setupStartedAt)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processS3WebsiteInstall(ctx, log, req)
	}) {
		respondS3WebsiteInstallModalError(w, modalBusyMsg)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

func parseS3WebsiteInstallModalArgs(values map[string]map[string]interactionStateValue) (args *s3WebsiteInstallArgs, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	slug := strings.TrimPrefix(strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockSlug, s3WebsiteInstallActionSlug)), "$")
	if !tunnelSlugPattern.MatchString(slug) {
		fieldErrors[s3WebsiteInstallBlockSlug] = "Use 3-64 lowercase letters, numbers, and hyphens. Start with a letter and end with a letter or number."
	}

	shortcutRaw := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockShortcut, s3WebsiteInstallActionShortcut))
	alias := slug
	if shortcutRaw != "" && !strings.HasPrefix(shortcutRaw, "$") {
		shortcutRaw = "$" + shortcutRaw
	}
	if shortcutRaw != "" {
		var aliasReason string
		alias, aliasReason = validateChannelShortcutToken(shortcutRaw)
		if aliasReason != "" {
			fieldErrors[s3WebsiteInstallBlockShortcut] = aliasReason
		}
	}

	envRaw := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockEnvironment, s3WebsiteInstallActionEnvironment))
	env, envMsg := parseTunnelEnvironment(envRaw)
	if envRaw == "" || envMsg != "" {
		fieldErrors[s3WebsiteInstallBlockEnvironment] = "Choose one of the listed target environments."
	}

	bucket := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockBucket, s3WebsiteInstallActionBucket))
	if !s3WebsiteBucketPattern.MatchString(bucket) {
		fieldErrors[s3WebsiteInstallBlockBucket] = "Use a non-dotted S3 bucket name with 3-63 lowercase letters, numbers, and hyphens."
	}

	region := strings.ToLower(strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockRegion, s3WebsiteInstallActionRegion)))
	if !isS3WebsiteCommercialRegion(region) {
		fieldErrors[s3WebsiteInstallBlockRegion] = "Use a standard AWS commercial region such as us-east-1."
	}

	prefix, prefixMsg := normalizeS3WebsitePrefix(interactionStateText(values, s3WebsiteInstallBlockPrefix, s3WebsiteInstallActionPrefix))
	if prefixMsg != "" {
		fieldErrors[s3WebsiteInstallBlockPrefix] = prefixMsg
	}

	index := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockIndex, s3WebsiteInstallActionIndex))
	if index == "" {
		index = defaultS3WebsiteIndexDocument
	} else if len(index) > 128 || !s3WebsiteIndexPattern.MatchString(index) || strings.Contains(index, "/") || index == "." || index == ".." {
		fieldErrors[s3WebsiteInstallBlockIndex] = "Use a simple file name such as index.html, default.html, or home.html."
	}

	if len(fieldErrors) > 0 {
		return nil, fieldErrors
	}
	return &s3WebsiteInstallArgs{
		Slug:          slug,
		Alias:         alias,
		Environment:   env,
		Bucket:        bucket,
		Region:        region,
		Prefix:        prefix,
		IndexDocument: index,
	}, nil
}

func isS3WebsiteCommercialRegion(region string) bool {
	if !s3WebsiteRegionPattern.MatchString(region) {
		return false
	}
	// Mirror origins/s3-static-connector/render.sh's unsupported partition
	// prefixes. The Slack-side regex is otherwise a stricter preflight gate for
	// real AWS region suffixes, so rejected modal values fail before key mint.
	for _, unsupportedPrefix := range []string{"cn-", "us-gov-", "us-iso-", "us-isob-"} {
		if strings.HasPrefix(region, unsupportedPrefix) {
			return false
		}
	}
	return true
}

func normalizeS3WebsitePrefix(raw string) (prefix, reason string) {
	prefix = strings.Trim(strings.TrimSpace(raw), "/")
	if prefix == "" {
		return "", ""
	}
	if len(prefix) > 256 {
		return "", "Use an S3 prefix up to 256 characters."
	}
	if !s3WebsitePrefixPattern.MatchString(prefix) {
		return "", "Use letters, numbers, dots, underscores, hyphens, and slashes only."
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "Use a prefix without empty, dot, or dot-dot path segments."
		}
	}
	return prefix, ""
}

func respondS3WebsiteInstallModalError(w http.ResponseWriter, message string) {
	view, err := S3WebsiteInstallErrorModal(message)
	if err != nil {
		slog.Error("S3 website install modal error render failed", "error", err)
		respondViewErrors(w, map[string]string{s3WebsiteInstallBlockSlug: "S3 website qURL Connector setup failed. Contact support."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: respActionUpdate,
		respFieldView:           json.RawMessage(view),
	})
}

func (h *Handler) processS3WebsiteInstall(ctx context.Context, log *slog.Logger, req *s3WebsiteInstallRequest) {
	var panicCleanup *tunnelInstallBuild
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("S3 website install: panic in setup worker", "recover", rec, "stack", string(debug.Stack()))
			if panicCleanup != nil {
				safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, panicCleanup.client, panicCleanup.key, "s3_website_unexpected_panic")
			}
			h.postS3WebsiteInstallUnexpectedFailureNotice(log, req)
		}
	}()
	if req == nil || req.args == nil {
		log.Error("S3 website install: setup worker missing parsed modal args")
		h.postS3WebsiteInstallUnexpectedFailureNotice(log, req)
		return
	}
	args := req.args
	if h.cfg.PostDM == nil {
		log.Error("S3 website install: bootstrap-key DM delivery is not configured; refusing to mint", "slug", args.Slug)
		_ = h.postResponse(log, req.responseURL, "S3 website qURL Connector setup needs Slack DM delivery for the temporary bootstrap key. No bootstrap key was minted. Ask the operator to update the qURL Slack app, then run `/qurl-admin protect` again.")
		return
	}
	build, failMsg, err := h.buildS3WebsiteInstall(ctx, log, req.teamID, req.channelID, req.userID, args, req.attemptID)
	if err != nil {
		_ = h.postResponse(log, req.responseURL, failMsg)
		return
	}
	panicCleanup = build

	log.Info("S3 website qURL Connector setup succeeded", "slug", args.Slug, "shortcut", args.Alias, "environment", args.Environment, "resource_id", build.resource.ResourceID)
	if err := h.postTunnelInstallDM(ctx, req.teamID, req.enterpriseID, req.userID, build.secretMessage); err != nil {
		log.Error("S3 website install: Slack DM delivery failed after bootstrap key mint; revoking key before posting install instructions", "error", err, "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID)
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_dm_delivery_failed")
		panicCleanup = nil
		_ = h.postResponse(log, req.responseURL, "Slack could not deliver the qURL Connector bootstrap key by DM, so the temporary key was revoked and the install instructions were not posted. Re-run `/qurl-admin protect` after DM delivery is available.")
		return
	}

	switch h.postInstallInstructions(log, req.responseURL, build.message) {
	case tunnelInstallInstructionsDeliverySucceeded, tunnelInstallInstructionsDeliveryDegraded:
		panicCleanup = nil
	case tunnelInstallInstructionsDeliveryFailed:
		log.Error("S3 website install: Slack follow-up delivery failed after bootstrap key mint; revoking key", "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID)
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_response_url_delivery_failed")
		panicCleanup = nil
		_ = h.postTunnelInstallDM(h.baseCtx, req.teamID, req.enterpriseID, req.userID, "The S3 website qURL Connector install instructions were not delivered, so the temporary bootstrap key from the previous DM was revoked. Discard that key and run `/qurl-admin protect` again.")
		_ = h.postResponse(log, req.responseURL, "Slack did not confirm delivery of the S3 website qURL Connector install instructions, so the bootstrap key was revoked. If the install block from this attempt appears later, discard it because its key is no longer valid. Run `/qurl-admin protect` again.")
	default:
		log.Error("S3 website install: unknown Slack follow-up delivery state after bootstrap key mint; revoking key", "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID)
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_unknown_response_url_delivery_state")
		panicCleanup = nil
	}
}

func (h *Handler) postS3WebsiteInstallUnexpectedFailureNotice(log *slog.Logger, req *s3WebsiteInstallRequest) {
	if req == nil || strings.TrimSpace(req.responseURL) == "" {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("S3 website install: panic posting unexpected-failure notice", "recover", rec, "stack", string(debug.Stack()))
		}
	}()
	_ = h.postResponse(log, req.responseURL, s3WebsiteUnexpectedFailureNotice)
}

func (h *Handler) buildS3WebsiteInstall(ctx context.Context, log *slog.Logger, teamID, channelID, userID string, args *s3WebsiteInstallArgs, attemptID string) (*tunnelInstallBuild, string, error) {
	var c *client.Client
	var mintedKey *client.APIKey
	buildComplete := false
	defer func() {
		if rec := recover(); rec != nil {
			if mintedKey != nil && !buildComplete {
				safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, mintedKey, "s3_website_build_panic")
			}
			panic(rec)
		}
	}()

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("S3 website install: failed to get API key", "error", err)
		return nil, authErrorMessage(err), err
	}

	resource, err := c.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         args.Slug,
		FindOrCreate: true,
		Description:  defaultS3WebsiteDisplayName(args),
	})
	if err != nil {
		log.Error("S3 website install: create/find resource failed", "error", err, "slug", args.Slug)
		return nil, sanitizeAPIError(err, "Failed to create or find the qURL Connector resource"), err
	}
	if resource.ResourceID == "" {
		log.Error("S3 website install: qURL API response missing resource identity", "slug", args.Slug)
		return nil, "qURL Connector setup could not receive the resource needed for the Slack shortcut. No bootstrap key was minted. Please retry after the qURL API returns resource_id for connector resources.", errors.New("qURL Connector resource identity incomplete")
	}

	aliasStatus, err := h.ensureTunnelAlias(ctx, teamID, channelID, args.Alias, resource.ResourceID)
	if err != nil {
		log.Error("S3 website install: channel shortcut bind failed", "error", err, "shortcut", args.Alias, "resource_id", resource.ResourceID)
		return nil, aliasStatus, err
	}

	preparedMessage, err := h.prepareS3WebsiteInstallMessage(args)
	if err != nil {
		log.Error("S3 website install: render preflight failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		return nil, "S3 website qURL Connector setup could not render the install instructions. No bootstrap key was minted. Please retry or contact support.", err
	}

	key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Slack qURL Connector bootstrap " + args.Slug,
		Scopes:         []string{tunnelScopeAgent, tunnelScopeWrite},
		KeyType:        client.APIKeyTypeTunnelBootstrap,
		TunnelSlug:     args.Slug,
		ExpiresIn:      tunnelBootstrapTTL,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(teamID, channelID, userID, args.Slug, attemptID),
	})
	if err != nil {
		log.Error("S3 website install: bootstrap key mint failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		return nil, sanitizeAPIError(err, "Failed to mint a qURL Connector bootstrap key"), err
	}
	mintedKey = key
	if key.APIKey == "" {
		log.Error("S3 website install: create api key response missing plaintext", "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_missing_plaintext")
		return nil, "The qURL API did not return a bootstrap key. Please retry or contact support.", errMissingBootstrapPlaintext
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		log.Error("S3 website install: create api key response was not shell-renderable", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_shell_validation_failed")
		return nil, "The qURL API returned a bootstrap key in an unexpected format. Please retry or contact support.", err
	}

	msg, err := preparedMessage.render(args, key, aliasStatus, resource.Description, h.now())
	if err != nil {
		log.Error("S3 website install: render failed after bootstrap key mint", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_message_render_failed")
		return nil, "S3 website qURL Connector setup could not render the install instructions. The temporary bootstrap key was revoked. Please retry or contact support.", err
	}
	secretMsg, err := renderTunnelBootstrapSecretMessage(&tunnelInstallArgs{Slug: args.Slug}, key, h.now())
	if err != nil {
		log.Error("S3 website install: secret message render failed after bootstrap key mint", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_secret_message_render_failed")
		return nil, "S3 website qURL Connector setup could not render the bootstrap-key DM. The temporary bootstrap key was revoked. Please retry or contact support.", err
	}

	buildComplete = true
	return &tunnelInstallBuild{client: c, resource: resource, key: key, message: msg, secretMessage: secretMsg}, "", nil
}

func defaultS3WebsiteDisplayName(args *s3WebsiteInstallArgs) string {
	if args == nil || args.Bucket == "" {
		return "Slack qURL Connector install for S3 website"
	}
	if args.Prefix == "" {
		return "Slack qURL Connector install for S3 website " + args.Bucket
	}
	return "Slack qURL Connector install for S3 website " + args.Bucket + "/" + args.Prefix
}

func (h *Handler) prepareS3WebsiteInstallMessage(args *s3WebsiteInstallArgs) (preparedS3WebsiteInstallMessage, error) {
	connectorImage := strings.TrimSpace(h.cfg.TunnelImage)
	usingDefaultConnectorImage := connectorImage == ""
	if connectorImage == "" {
		connectorImage = defaultTunnelImage
	}
	if err := ValidateTunnelImageRef(connectorImage); err != nil {
		return preparedS3WebsiteInstallMessage{}, fmt.Errorf("qURL Connector image reference: %w", err)
	}
	originImage := defaultS3StaticConnectorImage
	if err := ValidateTunnelImageRef(originImage); err != nil {
		return preparedS3WebsiteInstallMessage{}, fmt.Errorf("S3 origin image reference: %w", err)
	}
	environmentLabel, err := args.Environment.label()
	if err != nil {
		return preparedS3WebsiteInstallMessage{}, err
	}
	instructions, err := h.renderS3WebsiteInstallInstructions(args, connectorImage, originImage)
	if err != nil {
		return preparedS3WebsiteInstallMessage{}, err
	}
	imageNote := tunnelImageNote(usingDefaultConnectorImage)
	if imageNote != "" {
		imageNote += "\n"
	}
	imageNote += "For production S3 websites, pin the S3 origin image to a tested digest after it soaks in your target environment."
	return preparedS3WebsiteInstallMessage{
		connectorImage:   connectorImage,
		originImage:      originImage,
		environmentLabel: environmentLabel,
		instructions:     instructions,
		imageNote:        imageNote,
	}, nil
}

func (p *preparedS3WebsiteInstallMessage) render(args *s3WebsiteInstallArgs, key *client.APIKey, aliasStatus, displayName string, now time.Time) (string, error) {
	if key == nil {
		return "", errors.New("bootstrap api key is missing")
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("S3 website qURL Connector `")
	b.WriteString(args.Slug)
	b.WriteString("`")
	if displayName != "" {
		b.WriteString(" — ")
		b.WriteString(escapeMrkdwnText(displayName))
	}
	b.WriteString(" is ready to install.\n")
	b.WriteString(aliasStatus)
	b.WriteString("\n\nInstall instructions are below. The temporary bootstrap key ")
	b.WriteString(tunnelBootstrapExpiryLabel(key, now))
	b.WriteString(" and was sent separately by DM. The first agent bootstrap response seeds the qURL resource identity, so the key is only for agent bootstrap and first start.")
	b.WriteString("\n\n")
	b.WriteString("qURL Connector image: `")
	b.WriteString(p.connectorImage)
	b.WriteString("`.\nS3 origin image: `")
	b.WriteString(p.originImage)
	b.WriteString("`.\n")
	b.WriteString(p.imageNote)
	b.WriteString("\nTarget environment: ")
	b.WriteString(p.environmentLabel)
	b.WriteString(".\nS3 bucket: `")
	b.WriteString(escapeMrkdwnCode(args.Bucket))
	b.WriteString("` in `")
	b.WriteString(escapeMrkdwnCode(args.Region))
	b.WriteString("`.\n\n")
	b.WriteString(p.instructions)
	b.WriteString("\n\nTreat the separate bootstrap-key DM as secret until the qURL Connector connects. After the first successful start, remove the mounted bootstrap key from the runtime. Keep the qURL agent-state directory, volume, or PVC; it stores the agent identity used on future restarts.\n\n")
	b.WriteString("Then users can run `/qurl get $")
	b.WriteString(args.Alias)
	b.WriteString("`.")
	return b.String(), nil
}

func (h *Handler) renderS3WebsiteInstallInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	switch args.Environment {
	case tunnelEnvDocker:
		return renderDockerS3WebsiteInstructions(args, connectorImage, originImage)
	case tunnelEnvCompose:
		return renderDockerComposeS3WebsiteInstructions(args, connectorImage, originImage)
	case tunnelEnvECSFargate:
		return renderECSS3WebsiteInstructions(args, connectorImage, originImage)
	case tunnelEnvKubernetes:
		return renderKubernetesS3WebsiteInstructions(args, connectorImage, originImage)
	default:
		return "", fmt.Errorf("unreachable S3 website install environment: %s", args.Environment)
	}
}

func renderS3WebsiteConnectorConfigYAML(args *s3WebsiteInstallArgs) (string, error) {
	quotedSlug, err := yamlSingleQuoted(args.Slug)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`routes:
  - id: %s
    type: http
    local_ip: 127.0.0.1
    local_port: %d`, quotedSlug, s3WebsiteOriginPort), nil
}

func renderDockerS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	docker := fmt.Sprintf(`set -eu
%s

%s

QURL_CONNECTOR_ID=%s
S3_BUCKET=%s
AWS_REGION=%s
S3_PREFIX=%s
INDEX_DOCUMENT=%s
ORIGIN_CONTAINER="qurl-s3-origin-${QURL_CONNECTOR_ID}"
CONNECTOR_CONTAINER="qurl-connector-${QURL_CONNECTOR_ID}"
SECRET_DIR="/run/secrets/qurl-connector/${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent"
CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
%s
%s

if docker ps -a --format '{{.Names}}' | grep -Fxq "$CONNECTOR_CONTAINER"; then
  docker rm -f "$CONNECTOR_CONTAINER" >/dev/null
fi
if docker ps -a --format '{{.Names}}' | grep -Fxq "$ORIGIN_CONTAINER"; then
  docker rm -f "$ORIGIN_CONTAINER" >/dev/null
fi

docker run -d \
  --name "$ORIGIN_CONTAINER" \
  --restart=on-failure:5 \
  -e S3_BUCKET="$S3_BUCKET" \
  -e AWS_REGION="$AWS_REGION" \
  -e S3_PREFIX="$S3_PREFIX" \
  -e INDEX_DOCUMENT="$INDEX_DOCUMENT" \
  -e CACHE_CONNECTOR_ID="$QURL_CONNECTOR_ID" \
  %s

docker run -d \
  --name "$CONNECTOR_CONTAINER" \
  --network "container:${ORIGIN_CONTAINER}" \
  --restart=on-failure:5 \
  -v "$AGENT_STATE_DIR:/var/lib/layerv/agent" \
  -v "$SECRET_DIR:$SECRET_DIR:ro" \
  -v "$CONFIG_FILE:/work/qurl-proxy.yaml:ro" \
  -e QURL_API_KEY_FILE="$SECRET_DIR/api_key" \
  -e QURL_CONNECTOR_ID="$QURL_CONNECTOR_ID" \
  %s`, renderPortablePipefailShell(), renderSudoDetectionShell(), shellSingleQuote(args.Slug), shellSingleQuote(args.Bucket), shellSingleQuote(args.Region), shellSingleQuote(args.Prefix), shellSingleQuote(args.IndexDocument), configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/api_key"`), shellSingleQuote(originImage), shellSingleQuote(connectorImage))

	block, err := slackCodeBlock(docker)
	if err != nil {
		return "", err
	}
	intro := "Run this whole block on the Linux Docker host that has IAM access to the private S3 bucket. The host or container runtime must provide AWS credentials with s3:GetObject on the objects and s3:ListBucket on the bucket; on EC2 Docker hosts using instance roles, IMDSv2 needs hop-limit 2 for container credential access. No static AWS key is needed in the generated qURL Connector setup. The block prompts for the bootstrap key so the secret does not land in shell history."
	return intro + "\n\n" + block + "\n\nVerify with `docker logs -f qurl-connector-" + args.Slug + "` and `docker logs -f qurl-s3-origin-" + args.Slug + "`; after the qURL Connector connects, delete the bootstrap key file. If you recreate the S3 origin container, recreate the qURL Connector container too because it shares the origin container's network namespace.", nil
}

func renderDockerComposeS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	quotedConnectorImage, err := yamlSingleQuoted(connectorImage)
	if err != nil {
		return "", err
	}
	quotedOriginImage, err := yamlSingleQuoted(originImage)
	if err != nil {
		return "", err
	}
	quotedSlug, err := yamlSingleQuoted(args.Slug)
	if err != nil {
		return "", err
	}
	quotedBucket, err := yamlSingleQuoted(args.Bucket)
	if err != nil {
		return "", err
	}
	quotedRegion, err := yamlSingleQuoted(args.Region)
	if err != nil {
		return "", err
	}
	quotedPrefix, err := yamlSingleQuoted(args.Prefix)
	if err != nil {
		return "", err
	}
	quotedIndex, err := yamlSingleQuoted(args.IndexDocument)
	if err != nil {
		return "", err
	}
	originServiceName := "qurl-s3-origin-" + args.Slug
	connectorServiceName := "qurl-connector-" + args.Slug
	quotedOriginService, err := yamlSingleQuoted(originServiceName)
	if err != nil {
		return "", err
	}
	quotedConnectorService, err := yamlSingleQuoted(connectorServiceName)
	if err != nil {
		return "", err
	}
	// The Compose heredoc is intentionally unquoted so the target host expands
	// ${AGENT_STATE_DIR}, ${SECRET_DIR}, and ${QURL_CONNECTOR_ID}. Interpolated
	// S3 fields reach this template only after strict modal validation.
	compose := fmt.Sprintf(`set -eu
%s

%s

QURL_CONNECTOR_ID=%s
SECRET_DIR="/run/secrets/qurl-connector/${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent"
CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"
QURL_COMPOSE_FILE="$PWD/qurl-s3-website-${QURL_CONNECTOR_ID}.compose.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
%s
%s

cat > "$QURL_COMPOSE_FILE" <<QURL_COMPOSE_YAML_EOF
services:
  %s:
    image: %s
    restart: on-failure:5
    environment:
      S3_BUCKET: %s
      AWS_REGION: %s
      S3_PREFIX: %s
      INDEX_DOCUMENT: %s
      CACHE_CONNECTOR_ID: %s
  %s:
    image: %s
    restart: on-failure:5
    network_mode: "service:%s"
    depends_on:
      %s:
        condition: service_started
    volumes:
      - ${AGENT_STATE_DIR}:/var/lib/layerv/agent
      - ${SECRET_DIR}:/run/secrets/qurl-connector:ro
      - ./qurl-proxy-${QURL_CONNECTOR_ID}.yaml:/work/qurl-proxy.yaml:ro
    environment:
      QURL_API_KEY_FILE: /run/secrets/qurl-connector/api_key
      QURL_CONNECTOR_ID: %s
QURL_COMPOSE_YAML_EOF

docker compose -f "$QURL_COMPOSE_FILE" up -d`, renderPortablePipefailShell(), renderSudoDetectionShell(), shellSingleQuote(args.Slug), configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/api_key"`), quotedOriginService, quotedOriginImage, quotedBucket, quotedRegion, quotedPrefix, quotedIndex, quotedSlug, quotedConnectorService, quotedConnectorImage, originServiceName, quotedOriginService, quotedSlug)

	block, err := slackCodeBlock(compose)
	if err != nil {
		return "", err
	}
	intro := "Run this from the Docker Compose project directory on a Linux host that has IAM access to the private S3 bucket. On EC2 Docker hosts using instance roles, IMDSv2 needs hop-limit 2 for container credential access. It writes a standalone Compose file for the private S3 origin plus qURL Connector, and prompts for the bootstrap key so the secret does not land in shell history."
	return intro + "\n\n" + block + "\n\nVerify with `docker compose -f qurl-s3-website-" + args.Slug + ".compose.yaml logs -f qurl-connector-" + args.Slug + "`; after the qURL Connector connects, delete the bootstrap key file. If you recreate or rename the S3 origin service, recreate the qURL Connector service too because it shares the origin service network namespace.", nil
}

func renderECSS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	containerJSON, err := renderS3WebsiteECSContainerJSON(args, connectorImage, originImage)
	if err != nil {
		return "", err
	}
	configBlock, err := slackCodeBlock(configYAML)
	if err != nil {
		return "", err
	}
	containerBlock, err := slackCodeBlock(containerJSON)
	if err != nil {
		return "", err
	}
	secretName := ecsConnectorContainerName + "-" + args.Slug
	intro := strings.Join([]string{
		"Use this as an ECS/Fargate task-definition checklist.",
		"Create the AWS Secrets Manager secret as `" + secretName + "` using the temporary bootstrap key delivered separately by DM.",
		"Run both containers in the same task; Fargate awsvpc networking lets the qURL Connector reach the private S3 origin on `127.0.0.1:" + strconv.Itoa(s3WebsiteOriginPort) + "`.",
		"The task role needs s3:GetObject on the objects and s3:ListBucket on the bucket.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the bootstrap key from the separate DM in AWS Secrets Manager. This install-instructions message intentionally does not contain the key.\n\n" +
		"2. Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point mounted into the task as the `qurl-config` volume:\n\n" +
		configBlock + "\n\n" +
		"3. Add these two containers to the same task definition. Replace `REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_" + args.Slug + "` with the full secret ARN shown by Secrets Manager, and replace `<region>` in the awslogs options:\n\n" +
		containerBlock + "\n\n" +
		"4. Add durable EFS-backed volumes named qurl-agent-state and qurl-config. After the qURL Connector logs show it connected, delete the bootstrap secret.", nil
}

func renderS3WebsiteECSContainerJSON(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	containers := []ecsContainerDefinition{
		{
			Name:      "s3-static-origin",
			Image:     originImage,
			Essential: true,
			Environment: []ecsEnvironmentVar{
				{Name: "S3_BUCKET", Value: args.Bucket},
				{Name: "AWS_REGION", Value: args.Region},
				{Name: "S3_PREFIX", Value: args.Prefix},
				{Name: "INDEX_DOCUMENT", Value: args.IndexDocument},
				{Name: "CACHE_CONNECTOR_ID", Value: args.Slug},
			},
			LogConfiguration: ecsLogConfiguration{
				LogDriver: ecsLogDriverAWSLogs,
				Options: map[string]string{
					ecsLogGroupOption:        s3WebsiteECSLogGroup,
					ecsLogRegionOption:       ecsLogRegionPlaceholder,
					ecsLogStreamPrefixOption: "origin",
				},
			},
		},
		{
			Name:      ecsConnectorContainerName,
			Image:     connectorImage,
			Essential: true,
			Environment: []ecsEnvironmentVar{
				{Name: ecsConnectorIDEnv, Value: args.Slug},
			},
			Secrets: []ecsSecret{
				{Name: tunnelEnvAPIKey, ValueFrom: "REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_" + args.Slug},
			},
			MountPoints: []ecsMountPoint{
				{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/layerv/agent"},
				{SourceVolume: "qurl-config", ContainerPath: "/work", ReadOnly: true},
			},
			LogConfiguration: ecsLogConfiguration{
				LogDriver: ecsLogDriverAWSLogs,
				Options: map[string]string{
					ecsLogGroupOption:        s3WebsiteECSLogGroup,
					ecsLogRegionOption:       ecsLogRegionPlaceholder,
					ecsLogStreamPrefixOption: ecsLogStreamPrefixQURL,
				},
			},
		},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(containers); err != nil {
		return "", fmt.Errorf("marshal ECS S3 website container JSON: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func renderKubernetesS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	names := kubernetesTunnelObjectNames(args.Slug)
	quotedConfigMap, err := yamlSingleQuoted(names.configMap)
	if err != nil {
		return "", err
	}
	quotedAgentPVC, err := yamlSingleQuoted(names.agentPVC)
	if err != nil {
		return "", err
	}
	quotedSecret, err := yamlSingleQuoted(names.secret)
	if err != nil {
		return "", err
	}
	quotedConnectorImage, err := yamlSingleQuoted(connectorImage)
	if err != nil {
		return "", err
	}
	quotedOriginImage, err := yamlSingleQuoted(originImage)
	if err != nil {
		return "", err
	}
	quotedSlug, err := yamlSingleQuoted(args.Slug)
	if err != nil {
		return "", err
	}
	quotedBucket, err := yamlSingleQuoted(args.Bucket)
	if err != nil {
		return "", err
	}
	quotedRegion, err := yamlSingleQuoted(args.Region)
	if err != nil {
		return "", err
	}
	quotedPrefix, err := yamlSingleQuoted(args.Prefix)
	if err != nil {
		return "", err
	}
	quotedIndex, err := yamlSingleQuoted(args.IndexDocument)
	if err != nil {
		return "", err
	}
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	objects := fmt.Sprintf(`set -eu
%s

QURL_BOOTSTRAP_SECRET=%s
%s
%s

kubectl apply -f - <<'QURL_K8S_YAML_EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  qurl-proxy.yaml: |
%s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
QURL_K8S_YAML_EOF`, renderPortablePipefailShell(), shellSingleQuote(names.secret), renderBootstrapKeyPromptShell(), renderBootstrapKeyToCommandShell(`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`), quotedConfigMap, indentLines(configYAML, 4), quotedAgentPVC)

	patch := fmt.Sprintf(`securityContext:
  fsGroup: 65532
  fsGroupChangePolicy: OnRootMismatch
containers:
  - name: s3-static-origin
    image: %s
    env:
      - name: S3_BUCKET
        value: %s
      - name: AWS_REGION
        value: %s
      - name: S3_PREFIX
        value: %s
      - name: INDEX_DOCUMENT
        value: %s
      - name: CACHE_CONNECTOR_ID
        value: %s
  - name: qurl-connector
    image: %s
    securityContext:
      runAsUser: 65532
      runAsGroup: 65532
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      seccompProfile:
        type: RuntimeDefault
    env:
      - name: QURL_API_KEY_FILE
        value: /run/secrets/qurl-connector/api_key
      - name: QURL_CONNECTOR_ID
        value: %s
    volumeMounts:
      - name: qurl-agent-state
        mountPath: /var/lib/layerv/agent
      - name: qurl-bootstrap
        mountPath: /run/secrets/qurl-connector
        readOnly: true
      - name: qurl-proxy
        mountPath: /work/qurl-proxy.yaml
        subPath: qurl-proxy.yaml
        readOnly: true
volumes:
  - name: qurl-agent-state
    persistentVolumeClaim:
      claimName: %s
  - name: qurl-bootstrap
    secret:
      secretName: %s
      defaultMode: 0440
  - name: qurl-proxy
    configMap:
      name: %s`, quotedOriginImage, quotedBucket, quotedRegion, quotedPrefix, quotedIndex, quotedSlug, quotedConnectorImage, quotedSlug, quotedAgentPVC, quotedSecret, quotedConfigMap)

	objectsBlock, err := slackCodeBlock(objects)
	if err != nil {
		return "", err
	}
	patchBlock, err := slackCodeBlock(patch)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Run this once in the target namespace, then deploy the S3 origin and qURL Connector containers in the same pod so `127.0.0.1:" + strconv.Itoa(s3WebsiteOriginPort) + "` reaches the private S3 origin.",
		"The pod identity or node role needs s3:GetObject on the objects and s3:ListBucket on the bucket.",
		"The bootstrap key is streamed through your local shell into `kubectl`; do not run this from a shared, recorded, or command-traced terminal session.",
		"Delete the bootstrap Secret after the qURL Connector logs show it connected.",
	}, "\n")
	return intro + "\n\n" + objectsBlock + "\n\nPod spec additions:\nAdd both containers under the same pod's `containers:` list, append the volumes under `volumes:`, and merge the `fsGroup` fields into the pod-level `securityContext:`. Do not duplicate existing YAML keys.\n\n" + patchBlock, nil
}
