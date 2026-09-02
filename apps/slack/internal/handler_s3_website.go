package internal

import (
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

	"github.com/layervai/qurl-integrations/apps/slack/internal/connectorimage"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	// TODO(upstream-contract): keep this digest in lockstep with the
	// origins/s3-static-connector image promoted for Slack S3 website installs.
	defaultS3StaticConnectorImage = "ghcr.io/layervai/qurl-integrations/s3-static-connector@sha256:808620eb60fcb128cda086f4af80f69d876bc5bef5092dfedde3d94902e1c7c3"
	defaultS3WebsiteDescription   = "Slack qURL Connector install for S3 website"
	defaultS3WebsiteIndexDocument = "index.html"
	// TODO(upstream-contract): keep in lockstep with
	// origins/s3-static-connector's default LISTEN_ADDR=127.0.0.1:8080.
	s3WebsiteOriginPort              = 8080
	s3WebsiteUnexpectedFailureNotice = "S3 website qURL Connector setup stopped unexpectedly before install instructions were confirmed. If you received an enrollment-token DM from this attempt, discard it and run `/qurl-admin protect` again."
	s3WebsiteECSLogGroup             = "/ecs/qurl-s3-website"
	s3WebsiteOriginContainerName     = "s3-static-origin"
)

// S3OriginImageDigestRequired is the shared operator-facing remediation for
// S3 origin image overrides. Startup config and render-time defense-in-depth
// both use this exact text so their failure modes cannot drift.
const S3OriginImageDigestRequired = "S3 origin image reference must be digest-pinned as image@sha256:<64 lowercase hex>"

// TODO(upstream-contract): the generated Docker, Compose, and Kubernetes
// artifacts assume the qurl sidecar and origins/s3-static-connector both run
// as distroless nonroot UID/GID 65532. Keep host ownership, fsGroup,
// runAsUser, runAsGroup, and Secret defaultMode in lockstep with those image
// users.
// TODO(upstream-contract): origins/s3-static-connector treats S3_PREFIX="" as
// the bucket root. All renderers emit that explicit empty value.

var (
	// SECURITY: these four modal fields are interpolated into the intentionally
	// unquoted Compose heredoc in renderDockerComposeS3WebsiteInstructions.
	// Keep every pattern free of $, backticks, backslashes, and whitespace unless
	// that renderer first routes the value through a shell-quoted variable. The
	// ComposeShellMetacharacters test is the permanent cross-file guardrail.
	// Dotted buckets are intentionally narrower than AWS's general-purpose
	// grammar because the origin image does not support them. The helper below
	// adds AWS's reserved prefix/suffix rules to this shell-safe base shape.
	s3WebsiteBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	// TODO(upstream-contract): keep this commercial-region policy a SUBSET of
	// origins/s3-static-connector/render.sh's. The invariant is one-directional,
	// not equality: this form must never accept a region the origin rejects, but
	// it may be stricter, and today it is — render.sh allows us-east-0 and
	// us-east-01, which [1-9]\d* rejects here. Every other field is the same
	// shape (reserved bucket prefixes/suffixes, punctuation-only index, empty
	// prefix segments, and the 256-char prefix cap are all Slack-side only), so
	// the modal fails closed rather than emitting a manifest the origin refuses.
	// Pattern-based instead of an allowlist so new regions work without
	// redeploying the Slack app; unsupported partitions are excluded below and
	// AWS reports nonexistent commercial regions at deploy time.
	s3WebsiteRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[1-9]\d*$`)
	s3WebsitePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	// The origin image accepts only a bare INDEX_DOCUMENT file name; a nested
	// object path belongs in S3_PREFIX instead.
	s3WebsiteIndexPattern = regexp.MustCompile(`^[A-Za-z0-9._-]*[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

type s3WebsiteInstallArgs struct {
	Slug          string
	Alias         string
	Environment   tunnelInstallEnvironment
	Bucket        string
	Region        string
	Prefix        string
	IndexDocument string
	// Server-issued connector fields are populated after parsing, immediately
	// before rendering; Slack modal input can never supply them.
	ResourceID         string
	CRID               string
	ConnectorRoutingID string
	KnockResourceID    string
	ServingEpoch       uint64
	APIURL             string
	Hub                hubTrust
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
	log := slog.With(
		"command", "connector_setup_modal",
		"team_id", sanitizeS3WebsiteLogValue(payload.Team.ID),
		"user_id", sanitizeS3WebsiteLogValue(payload.User.ID),
		"view_id", sanitizeS3WebsiteLogValue(payload.View.ID),
	)
	if setupType != connectorSetupExistingService && setupType != connectorSetupS3Website {
		log.Warn("connector setup modal rejected unknown setup type")
		respondViewErrors(w, map[string]string{connectorSetupBlockType: "Choose one of the listed qURL Connector setup types."})
		return
	}
	log = log.With("setup_type", sanitizeS3WebsiteLogValue(setupType))

	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		log.Warn("connector setup modal metadata parse failed", "error", err)
		respondConnectorInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		log.Warn("connector setup modal metadata incomplete")
		respondConnectorInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	modalAge := h.now().Sub(time.Unix(meta.CreatedAtUnix, 0))
	if meta.CreatedAtUnix <= 0 || modalAge > tunnelInstallModalTTL || modalAge < -tunnelBootstrapSkew {
		log.Warn("connector setup modal expired", "created_at_unix", sanitizeS3WebsiteLogValue(strconv.FormatInt(meta.CreatedAtUnix, 10)), "modal_age_ms", sanitizeS3WebsiteLogValue(strconv.FormatInt(modalAge.Milliseconds(), 10)))
		respondConnectorInstallModalError(w, "This modal expired. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		log.Warn("connector setup modal team mismatch", "payload_team_id", sanitizeS3WebsiteLogValue(payload.Team.ID), "metadata_team_id", sanitizeS3WebsiteLogValue(meta.TeamID))
		respondConnectorInstallModalError(w, "This modal was opened for a different workspace. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		log.Warn("connector setup modal user mismatch", "payload_user_id", sanitizeS3WebsiteLogValue(payload.User.ID), "metadata_user_id", sanitizeS3WebsiteLogValue(meta.UserID))
		respondConnectorInstallModalError(w, "Only the admin who opened this modal can submit it. Run /qurl-admin protect and choose qURL Connector again to start a new setup.")
		return
	}
	// Preserve the slash-command timestamp across the chooser and install form.
	// Slack response_url values expire on roughly the same horizon as this TTL;
	// re-stamping here would let a valid second modal outlive its delivery URL.
	// Admin status is re-checked immediately before any enrollment token can mint.

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
		log.Error("connector setup next modal render failed", "error", err)
		respondConnectorInstallModalError(w, "Could not open qURL Connector setup. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: respActionUpdate,
		respFieldView:           json.RawMessage(view),
	})
}

func (h *Handler) handleS3WebsiteInstallSubmission(w http.ResponseWriter, payload *ViewSubmission) {
	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("S3 website install modal metadata parse failed", "error", sanitizeS3WebsiteLogValue(err.Error()), "team_id", sanitizeS3WebsiteLogValue(payload.Team.ID), "user_id", sanitizeS3WebsiteLogValue(payload.User.ID), "view_id", sanitizeS3WebsiteLogValue(payload.View.ID))
		respondS3WebsiteInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("S3 website install modal metadata incomplete", "team_id", sanitizeS3WebsiteLogValue(payload.Team.ID), "user_id", sanitizeS3WebsiteLogValue(payload.User.ID), "view_id", sanitizeS3WebsiteLogValue(payload.View.ID))
		respondS3WebsiteInstallModalError(w, "Could not verify this modal. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	log := slog.With(
		"command", "s3_website_install_modal",
		"team_id", sanitizeS3WebsiteLogValue(meta.TeamID),
		"channel_id", sanitizeS3WebsiteLogValue(meta.ChannelID),
		"user_id", sanitizeS3WebsiteLogValue(meta.UserID),
		"view_id", sanitizeS3WebsiteLogValue(payload.View.ID),
	)

	modalAge := h.now().Sub(time.Unix(meta.CreatedAtUnix, 0))
	if meta.CreatedAtUnix <= 0 || modalAge > tunnelInstallModalTTL || modalAge < -tunnelBootstrapSkew {
		log.Warn("S3 website install modal expired", "created_at_unix", sanitizeS3WebsiteLogValue(strconv.FormatInt(meta.CreatedAtUnix, 10)), "modal_age_ms", sanitizeS3WebsiteLogValue(strconv.FormatInt(modalAge.Milliseconds(), 10)))
		respondS3WebsiteInstallModalError(w, "This modal expired. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		log.Warn("S3 website install modal team mismatch", "payload_team_id", sanitizeS3WebsiteLogValue(payload.Team.ID), "metadata_team_id", sanitizeS3WebsiteLogValue(meta.TeamID))
		respondS3WebsiteInstallModalError(w, "This modal was opened for a different workspace. Run /qurl-admin protect and choose qURL Connector again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		log.Warn("S3 website install modal user mismatch", "payload_user_id", sanitizeS3WebsiteLogValue(payload.User.ID), "metadata_user_id", sanitizeS3WebsiteLogValue(meta.UserID))
		respondS3WebsiteInstallModalError(w, "Only the admin who opened this modal can submit it. Run /qurl-admin protect and choose qURL Connector again to start a new setup.")
		return
	}
	if h.cfg.AdminStore == nil {
		respondS3WebsiteInstallModalError(w, "Admin features are not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	}
	if h.aliasStore == nil {
		respondS3WebsiteInstallModalError(w, "Channel alias storage is not configured on this Secure Access Agent deployment. Contact the operator.")
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
	args, fieldErrors := parseS3WebsiteInstallModalArgs(payload.View.State.Values)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	req := &s3WebsiteInstallRequest{
		teamID:       meta.TeamID,
		enterpriseID: meta.EnterpriseID,
		channelID:    meta.ChannelID,
		userID:       meta.UserID,
		responseURL:  meta.ResponseURL,
		args:         args,
	}
	// S3 website setup is reachable only from the human admin chooser today, so
	// there is no agent-origin audit row to attach. If an agent-origin entry point
	// is added, mirror handleTunnelInstallSubmission's audit wiring before minting.

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

	slug, alias := parseConnectorSlugAndShortcut(values, s3WebsiteInstallBlockSlug, s3WebsiteInstallActionSlug, s3WebsiteInstallBlockShortcut, s3WebsiteInstallActionShortcut, fieldErrors)

	envRaw := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockEnvironment, s3WebsiteInstallActionEnvironment))
	env, envMsg := parseTunnelEnvironment(envRaw)
	if envMsg != "" {
		fieldErrors[s3WebsiteInstallBlockEnvironment] = envMsg
	}

	bucket := strings.TrimSpace(interactionStateText(values, s3WebsiteInstallBlockBucket, s3WebsiteInstallActionBucket))
	if !validS3WebsiteBucketName(bucket) {
		fieldErrors[s3WebsiteInstallBlockBucket] = "Use the exact lowercase name of an existing non-dotted general-purpose S3 bucket; reserved AWS names are not supported."
	}

	// AWS region names are case-insensitive operator input; bucket names are
	// deliberately not normalized because S3 bucket DNS names are lowercase.
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
	} else if len(index) > 128 || !s3WebsiteIndexPattern.MatchString(index) {
		// Match the origin image's object-key grammar, including valid dot-leading
		// names such as .htaccess. Requiring at least one letter or number rejects
		// punctuation-only operator mistakes such as ., -, and ___.
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
	// Mirror origins/s3-static-connector/render.sh's unsupported commercial
	// partition check. The Slack-side regex already rejects four-segment
	// us-gov/us-iso/us-isob values before this point.
	return !strings.HasPrefix(region, "cn-")
}

func validS3WebsiteBucketName(bucket string) bool {
	if !s3WebsiteBucketPattern.MatchString(bucket) {
		return false
	}
	for _, prefix := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(bucket, prefix) {
			return false
		}
	}
	for _, suffix := range []string{"-s3alias", "--ol-s3", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(bucket, suffix) {
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

// sanitizeS3WebsiteLogValue preserves diagnostic context while preventing a
// Slack- or API-controlled value from forging a second plain-text log entry.
// Production uses the shared JSON slog handler, which also escapes control
// bytes; this remains a defense-in-depth boundary if the handler changes.
func sanitizeS3WebsiteLogValue(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
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
				if panicCleanup.disableOnFail {
					disableSharingAfterInstallFailure(h.baseCtx, log, panicCleanup.client, panicCleanup.resource.ResourceID, "s3_website_unexpected_panic")
				}
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
		log.Error("S3 website install: enrollment-token DM delivery is not configured; refusing to mint", "slug", sanitizeS3WebsiteLogValue(args.Slug))
		_ = h.postResponse(log, req.responseURL, "S3 website qURL Connector setup needs Slack DM delivery for the temporary enrollment token. No enrollment token was minted. Ask the operator to update the qURL Slack app, then run `/qurl-admin protect` again.")
		return
	}
	build, failMsg, err := h.buildS3WebsiteInstall(ctx, log, req.teamID, req.channelID, req.userID, args, req.attemptID)
	if err != nil {
		_ = h.postResponse(log, req.responseURL, failMsg)
		return
	}
	panicCleanup = build

	log.Info("S3 website qURL Connector setup succeeded", "slug", sanitizeS3WebsiteLogValue(args.Slug), "shortcut", sanitizeS3WebsiteLogValue(args.Alias), "environment", sanitizeS3WebsiteLogValue(string(args.Environment)), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID))
	if err := h.postTunnelInstallDM(ctx, req.teamID, req.enterpriseID, req.userID, build.secretMessage); err != nil {
		log.Error("S3 website install: Slack DM delivery failed after enrollment token mint; revoking token before posting install instructions", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID))
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_dm_delivery_failed")
		if build.disableOnFail {
			disableSharingAfterInstallFailure(h.baseCtx, log, build.client, build.resource.ResourceID, "s3_website_dm_delivery_failed")
		}
		panicCleanup = nil
		message := "Slack could not deliver the qURL Connector enrollment token by DM, so the temporary token was revoked and the install instructions were not posted."
		if errors.Is(err, ErrSlackMissingScope) {
			message += " " + h.tunnelBootstrapDMSlackAppInstallMessage()
		} else {
			message += " Re-run `/qurl-admin protect` after DM delivery is available."
		}
		_ = h.postResponse(log, req.responseURL, message)
		return
	}

	switch h.postInstallInstructions(log, req.responseURL, build.message) {
	case tunnelInstallInstructionsDeliverySucceeded, tunnelInstallInstructionsDeliveryDegraded:
		panicCleanup = nil
	case tunnelInstallInstructionsDeliveryFailed:
		log.Error("S3 website install: Slack follow-up delivery failed after enrollment token mint; revoking token", "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID))
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_response_url_delivery_failed")
		if build.disableOnFail {
			disableSharingAfterInstallFailure(h.baseCtx, log, build.client, build.resource.ResourceID, "s3_website_response_url_delivery_failed")
		}
		panicCleanup = nil
		if err := h.postTunnelInstallDM(h.baseCtx, req.teamID, req.enterpriseID, req.userID, "The S3 website qURL Connector install instructions were not delivered, so the temporary enrollment token from the previous DM was revoked. Discard that token and run `/qurl-admin protect` again."); err != nil {
			log.Error("S3 website install: Slack discard DM delivery failed after enrollment token revoke", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID), "event", "s3_website_bootstrap_discard_dm_delivery_failed")
		}
		if !h.postResponse(log, req.responseURL, "Slack did not confirm delivery of the S3 website qURL Connector install instructions, so the enrollment token was revoked. If the install block from this attempt appears later, discard it because its token is no longer valid. Run `/qurl-admin protect` again.") {
			log.Error("S3 website install: Slack discard notice delivery failed after enrollment token revoke", "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID), "event", "s3_website_bootstrap_discard_notice_delivery_failed")
		}
	default:
		log.Error("S3 website install: unknown Slack follow-up delivery state after enrollment token mint; revoking token", "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID))
		safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "s3_website_unknown_response_url_delivery_state")
		if build.disableOnFail {
			disableSharingAfterInstallFailure(h.baseCtx, log, build.client, build.resource.ResourceID, "s3_website_unknown_response_url_delivery_state")
		}
		panicCleanup = nil
		if !h.postResponse(log, req.responseURL, "Slack returned an unexpected delivery state for the S3 website qURL Connector install instructions, so the enrollment token was revoked. If an install block from this attempt appears later, discard it because its token is no longer valid. Run `/qurl-admin protect` again.") {
			log.Error("S3 website install: unknown delivery-state discard notice failed after enrollment token revoke", "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(build.resource.ResourceID), "key_id", sanitizeS3WebsiteLogValue(build.key.KeyID), "event", "s3_website_unknown_delivery_discard_notice_failed")
		}
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

//nolint:gocognit,gocyclo // Keeping the ordered mutation and compensation transaction visible prevents cleanup ownership from drifting between stages.
func (h *Handler) buildS3WebsiteInstall(ctx context.Context, log *slog.Logger, teamID, channelID, userID string, args *s3WebsiteInstallArgs, attemptID string) (*tunnelInstallBuild, string, error) {
	var c *client.Client
	var mintedKey *client.APIKey
	var resourceID string
	sharingNeedsCleanup := false
	buildComplete := false
	defer func() {
		if rec := recover(); rec != nil {
			if mintedKey != nil && !buildComplete {
				safeRevokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, mintedKey, "s3_website_build_panic")
			}
			if sharingNeedsCleanup && !buildComplete {
				disableSharingAfterInstallFailure(h.baseCtx, log, c, resourceID, "s3_website_build_panic")
			}
			panic(rec)
		}
		if sharingNeedsCleanup && !buildComplete {
			disableSharingAfterInstallFailure(h.baseCtx, log, c, resourceID, "s3_website_build_failed")
		}
	}()

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("S3 website install: failed to get API key", "error", sanitizeS3WebsiteLogValue(err.Error()))
		return nil, authErrorMessage(err), err
	}

	resource, err := c.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         args.Slug,
		FindOrCreate: true,
		Description:  defaultS3WebsiteDescription,
	})
	if err != nil {
		log.Error("S3 website install: create/find resource failed", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug))
		return nil, sanitizeAPIError(err, "Failed to create or find the qURL Connector resource"), err
	}
	resolvedArgs := *args
	resolvedArgs.Hub = h.cfg.ConnectorHub
	if err := resolvedArgs.pinConnectorResource(resource, h.cfg.ConnectorAPIURL); err != nil {
		if errors.Is(err, errConnectorAPIURLMissing) || errors.Is(err, errConnectorAPIURLInvalid) {
			log.Error("S3 website install: local connector API URL configuration invalid", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug))
			return nil, "S3 website qURL Connector setup is unavailable because this Slack deployment has an invalid QURL_ENDPOINT. No enrollment token was minted. Contact the operator.", err
		}
		resourceIDPresent := resource != nil && strings.TrimSpace(resource.ResourceID) != ""
		connectorRoutingIDPresent := resource != nil && strings.TrimSpace(resource.ConnectorRoutingID) != ""
		knockResourceIDPresent := resource != nil && strings.TrimSpace(resource.KnockResourceID) != ""
		log.Error("S3 website install: qURL API response missing pinned connector identity", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id_present", resourceIDPresent, "connector_routing_id_present", connectorRoutingIDPresent, "knock_resource_id_present", knockResourceIDPresent)
		return nil, "qURL Connector setup could not receive the complete routing details needed for enrollment. No enrollment token was minted. Please retry after the qURL API returns resource_id, connector_routing_id, and knock_resource_id for Connector resources.", fmt.Errorf("qURL Connector resource identity incomplete: %w", err)
	}
	resourceID = resource.ResourceID

	aliasStatus, err := h.ensureTunnelAlias(ctx, teamID, channelID, args.Alias, resolvedArgs.ResourceID)
	if err != nil {
		log.Error("S3 website install: channel shortcut bind failed", "error", sanitizeS3WebsiteLogValue(err.Error()), "shortcut", sanitizeS3WebsiteLogValue(args.Alias), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID))
		return nil, aliasStatus, err
	}
	previousSharing, err := c.GetSharing(ctx, resource.ResourceID)
	if err != nil {
		return nil, sanitizeAPIError(err, "Failed to read qURL Connector sharing state"), err
	}
	restarted, err := restartSharingForInstall(ctx, c, resource.ResourceID, previousSharing)
	if err != nil {
		return nil, sanitizeAPIError(err, "Failed to enable qURL Connector sharing"), err
	}
	// Preserve prior authoritative intent. A prior-on sidecar can reacquire the
	// rotated epoch through the stale-session path; only a prior-off install
	// owns a new on transition that must be compensated if setup fails.
	sharingNeedsCleanup = previousSharing.DesiredState == sharingDesiredOff
	resolvedArgs.CRID = restarted.CRID
	resolvedArgs.ServingEpoch = restarted.ServingEpoch

	preparedMessage, err := h.prepareS3WebsiteInstallMessage(&resolvedArgs)
	if err != nil {
		log.Error("S3 website install: render preflight failed", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID))
		return nil, sharingInstallFailureMessage("S3 website qURL Connector setup could not render the install instructions. No enrollment token was minted. Please retry or contact support.", previousSharing), err
	}

	key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Slack qURL Connector enrollment " + args.Slug,
		Kind:           client.CredentialKindEnrollmentToken,
		Target:         client.CredentialTargetConnector,
		Claims:         []client.CredentialClaim{{Type: client.CredentialClaimTypeConnector, ID: args.Slug}},
		ExpiresIn:      tunnelBootstrapTTL,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(teamID, channelID, userID, args.Slug, attemptID),
	})
	if err != nil {
		// Match the tunnel twin: APIError.Error() renders only "Title (Status):
		// Detail", so the bare error drops the server's invalid_fields — the part
		// that names the rejected key on a kind-first contract rejection.
		log.Error("S3 website install: enrollment token mint failed", withAPIErrorAttrs(err, "error", err, "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID))...)
		return nil, sharingInstallFailureMessage(sanitizeAPIError(err, "Failed to mint a qURL Connector enrollment token"), previousSharing), err
	}
	mintedKey = key
	if key.APIKey == "" {
		log.Error("S3 website install: create api key response missing plaintext", "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID), "key_id", sanitizeS3WebsiteLogValue(key.KeyID))
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_missing_plaintext")
		return nil, sharingInstallFailureMessage("The qURL API did not return an enrollment token. Please retry or contact support.", previousSharing), errMissingBootstrapPlaintext
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		log.Error("S3 website install: create api key response was not shell-renderable", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID), "key_id", sanitizeS3WebsiteLogValue(key.KeyID))
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_shell_validation_failed")
		return nil, sharingInstallFailureMessage("The qURL API returned an enrollment token in an unexpected format. Please retry or contact support.", previousSharing), err
	}

	msg, err := preparedMessage.render(&resolvedArgs, key, aliasStatus, resource.Description, h.now())
	if err != nil {
		log.Error("S3 website install: render failed after enrollment token mint", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID), "key_id", sanitizeS3WebsiteLogValue(key.KeyID))
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_message_render_failed")
		return nil, sharingInstallFailureMessage("S3 website qURL Connector setup could not render the install instructions. The temporary enrollment token was revoked. Please retry or contact support.", previousSharing), err
	}
	secretMsg, err := renderTunnelBootstrapSecretMessage(&tunnelInstallArgs{Slug: args.Slug}, key, h.now())
	if err != nil {
		log.Error("S3 website install: secret message render failed after enrollment token mint", "error", sanitizeS3WebsiteLogValue(err.Error()), "slug", sanitizeS3WebsiteLogValue(args.Slug), "resource_id", sanitizeS3WebsiteLogValue(resolvedArgs.ResourceID), "key_id", sanitizeS3WebsiteLogValue(key.KeyID))
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "s3_website_secret_message_render_failed")
		return nil, sharingInstallFailureMessage("S3 website qURL Connector setup could not render the enrollment-token DM. The temporary enrollment token was revoked. Please retry or contact support.", previousSharing), err
	}

	buildComplete = true
	return &tunnelInstallBuild{client: c, resource: resource, key: key, disableOnFail: sharingNeedsCleanup, message: msg, secretMessage: secretMsg}, "", nil
}

func (h *Handler) prepareS3WebsiteInstallMessage(args *s3WebsiteInstallArgs) (preparedS3WebsiteInstallMessage, error) {
	connectorImage := strings.TrimSpace(h.cfg.TunnelImage)
	usingDefaultConnectorImage := connectorImage == ""
	if usingDefaultConnectorImage {
		connectorImage = defaultTunnelImage
	}
	if err := ValidateTunnelImageRef(connectorImage); err != nil {
		return preparedS3WebsiteInstallMessage{}, fmt.Errorf("qURL Connector image reference: %w", err)
	}
	originImage := strings.TrimSpace(h.cfg.S3OriginImage)
	if originImage == "" {
		originImage = defaultS3StaticConnectorImage
	}
	if err := ValidateTunnelImageRef(originImage); err != nil {
		return preparedS3WebsiteInstallMessage{}, fmt.Errorf("S3 origin image reference: %w", err)
	}
	// readS3OriginImageConfig enforces this at startup; keep the render-time
	// check as defense-in-depth for tests and any future direct Handler wiring.
	if err := RequireS3OriginImageDigest(originImage); err != nil {
		return preparedS3WebsiteInstallMessage{}, err
	}
	environmentLabel, err := args.Environment.label()
	if err != nil {
		return preparedS3WebsiteInstallMessage{}, err
	}
	instructions, err := renderS3WebsiteInstallInstructions(args, connectorImage, originImage)
	if err != nil {
		return preparedS3WebsiteInstallMessage{}, err
	}
	imageNote := tunnelImageNote(usingDefaultConnectorImage)
	if imageNote != "" {
		imageNote += "\n"
	}
	imageNote += "S3 origin image is digest-pinned by default; set `QURL_S3_ORIGIN_IMAGE` to a tested digest when rotating it."
	return preparedS3WebsiteInstallMessage{
		connectorImage:   connectorImage,
		originImage:      originImage,
		environmentLabel: environmentLabel,
		instructions:     instructions,
		imageNote:        imageNote,
	}, nil
}

// RequireS3OriginImageDigest keeps startup validation and render-time
// defense-in-depth on the same digest-pin contract.
func RequireS3OriginImageDigest(image string) error {
	// ClassifyPin validates the full reference; the explicit sha256 marker keeps
	// this operator contract narrower than other digest algorithms it accepts.
	if connectorimage.ClassifyPin(image) != connectorimage.Accepted || !strings.Contains(image, "@sha256:") {
		return errors.New(S3OriginImageDigestRequired)
	}
	return nil
}

func (p *preparedS3WebsiteInstallMessage) render(args *s3WebsiteInstallArgs, key *client.APIKey, aliasStatus, displayName string, now time.Time) (string, error) {
	if key == nil {
		return "", errors.New("bootstrap api key is missing")
	}
	var b strings.Builder
	b.WriteString("S3 website qURL Connector `")
	b.WriteString(args.Slug)
	b.WriteString("`")
	if displayName != "" && displayName != defaultS3WebsiteDescription {
		b.WriteString(" — ")
		b.WriteString(escapeMrkdwnText(displayName))
	}
	b.WriteString(" is ready to install.\n")
	b.WriteString(aliasStatus)
	b.WriteString("\n\nInstall instructions are below. The temporary enrollment token ")
	b.WriteString(tunnelBootstrapExpiryLabel(key, now))
	b.WriteString(" and was sent separately by DM. The generated qURL Connector config already includes the qURL resource details, so the enrollment token is only for the first enrollment.")
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
	b.WriteString("\n\nTreat the separate enrollment-token DM as secret until the qURL Connector connects. ")
	b.WriteString(tunnelBootstrapRetirementNote(args.Environment))
	b.WriteString(" Keep the qURL agent-state directory, volume, or PVC; it stores the agent identity used on future restarts.\n\n")
	b.WriteString("Then users can run `/qurl get $")
	b.WriteString(args.Alias)
	b.WriteString("`.")
	return b.String(), nil
}

// Hardening is deliberately asymmetric across the two containers every renderer
// below emits, and the asymmetry is load-bearing: only the Connector gets a
// read-only root filesystem (plus a bounded /tmp). The S3 origin keeps a
// writable root because origins/s3-static-connector/render.sh renders
// nginx.conf and envoy.yaml into RENDER_DIR (/etc/qurl/rendered, created and
// owned by uid 65532 in the image) on every start, so a read-only root would
// fail the origin at boot. Both containers still run nonroot with ALL
// capabilities dropped and no-new-privileges. Keep this split when editing the
// Docker, Compose, ECS, and Kubernetes renderers, and see apps/slack/docs/
// operating.md, which scopes its read-only-root claim to the Connector.
func renderS3WebsiteInstallInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
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
	if err := args.requirePinnedConnectorResource(); err != nil {
		return "", err
	}
	// Delegate rather than re-emit the route template: qurl-connector's strict
	// decoder schema belongs in one Go function, and renderTunnelConfigYAML
	// additionally revalidates the route identity at the renderer boundary.
	// Output is byte-identical for a fully pinned resource.
	return renderTunnelConfigYAML(&tunnelInstallArgs{
		Slug:               args.Slug,
		LocalPort:          s3WebsiteOriginPort,
		ResourceID:         args.ResourceID,
		CRID:               args.CRID,
		ConnectorRoutingID: args.ConnectorRoutingID,
		KnockResourceID:    args.KnockResourceID,
		ServingEpoch:       args.ServingEpoch,
	})
}

func yamlSingleQuotedValues(values ...string) ([]string, error) {
	quoted := make([]string, len(values))
	for i, value := range values {
		var err error
		quoted[i], err = yamlSingleQuoted(value)
		if err != nil {
			return nil, err
		}
	}
	return quoted, nil
}

func (args *s3WebsiteInstallArgs) pinConnectorResource(resource *client.Resource, apiURL string) error {
	resourceID, connectorRoutingID, knockResourceID, err := pinConnectorResource(resource)
	if err != nil {
		return err
	}
	args.ResourceID = resourceID
	args.ConnectorRoutingID = connectorRoutingID
	args.KnockResourceID = knockResourceID
	args.APIURL = strings.TrimSpace(apiURL)
	return args.requirePinnedConnectorResource()
}

func (args *s3WebsiteInstallArgs) requirePinnedConnectorResource() error {
	if args == nil {
		return errors.New("S3 website install args are missing")
	}
	if err := requirePinnedConnectorResource(args.ResourceID, args.ConnectorRoutingID, args.KnockResourceID); err != nil {
		return err
	}
	return ValidateConnectorAPIURL(args.APIURL)
}

func renderDockerS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
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
CONNECTOR_CONTAINER="qurl-${QURL_CONNECTOR_ID}"
SECRET_DIR="/run/secrets/qurl/${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl/${QURL_CONNECTOR_ID}"
CONFIG_FILE="$PWD/qurl-share-${QURL_CONNECTOR_ID}.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF
$SUDO chmod 0644 "$CONFIG_FILE"

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
  --user 65532:65532 \
  --restart=unless-stopped \
  --cap-drop=ALL \
  --security-opt=no-new-privileges:true \
  -e S3_BUCKET="$S3_BUCKET" \
  -e AWS_REGION="$AWS_REGION" \
  -e S3_PREFIX="$S3_PREFIX" \
  -e INDEX_DOCUMENT="$INDEX_DOCUMENT" \
  -e CACHE_CONNECTOR_ID="$QURL_CONNECTOR_ID" \
  %s

docker run -d \
  --name "$CONNECTOR_CONTAINER" \
  --user 65532:65532 \
  --network "container:${ORIGIN_CONTAINER}" \
  --restart=unless-stopped \
  --read-only \
  --tmpfs /tmp:rw,size=64m \
  --cap-drop=ALL \
  --security-opt=no-new-privileges:true \
  --pids-limit=512 \
  -v "$AGENT_STATE_DIR:/var/lib/qurl" \
  -v "$SECRET_DIR:/run/secrets/qurl:ro" \
  -v "$CONFIG_FILE:/etc/qurl/share.yaml:ro" \
  -e QURL_ENDPOINT=%s \
  --entrypoint /usr/local/bin/qurl \
  %s daemon run \
    --state-dir /var/lib/qurl \
    --headless-config /etc/qurl/share.yaml \
    --enrollment-token-file /run/secrets/qurl/enrollment-token`, renderPortablePipefailShell(), renderSudoDetectionShell(), shellSingleQuote(args.Slug), shellSingleQuote(args.Bucket), shellSingleQuote(args.Region), shellSingleQuote(args.Prefix), shellSingleQuote(args.IndexDocument), configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/enrollment-token"`), shellSingleQuote(originImage), shellSingleQuote(endpoint), shellSingleQuote(connectorImage))

	block, err := slackCodeBlock(docker)
	if err != nil {
		return "", err
	}
	intro := "Run this whole block on the Linux Docker host that has IAM access to the private S3 bucket. The host or container runtime must provide AWS credentials with s3:GetObject on the objects and s3:ListBucket on the bucket; on EC2 Docker hosts using instance roles, IMDSv2 needs hop-limit 2 for container credential access. No static AWS key is needed in the generated qURL Connector setup. The block prompts for the enrollment token so the secret does not land in shell history."
	block, err = withHubTrustDockerEnv(block, args.Hub)
	if err != nil {
		return "", err
	}
	return intro + "\n\n" + block + "\n\nVerify with `docker logs -f qurl-" + args.Slug + "` and `docker logs -f qurl-s3-origin-" + args.Slug + "`; after qURL connects, delete the enrollment-token file. If you recreate the S3 origin container or Docker auto-restarts it after a crash, recreate or restart qURL too because it shares the origin container's network namespace.", nil
}

func renderDockerComposeS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(args)
	if err != nil {
		return "", err
	}
	originServiceName := "qurl-s3-origin-" + args.Slug
	connectorServiceName := "qurl-" + args.Slug
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		return "", err
	}
	quoted, err := yamlSingleQuotedValues(
		connectorImage, originImage, args.Slug, endpoint,
		args.Bucket, args.Region, args.Prefix, args.IndexDocument,
		originServiceName, connectorServiceName,
	)
	if err != nil {
		return "", err
	}
	quotedConnectorImage, quotedOriginImage, quotedSlug := quoted[0], quoted[1], quoted[2]
	quotedAPIURL, quotedBucket, quotedRegion := quoted[3], quoted[4], quoted[5]
	quotedPrefix, quotedIndex := quoted[6], quoted[7]
	quotedOriginService, quotedConnectorService := quoted[8], quoted[9]
	// The Compose heredoc is intentionally unquoted so the target host expands
	// ${AGENT_STATE_DIR}, ${SECRET_DIR}, and ${QURL_CONNECTOR_ID}. Interpolated
	// S3 fields reach this template only after strict modal validation, and image
	// refs remain safe only while
	// ValidateTunnelImageRef excludes shell metacharacters such as $, backticks,
	// backslashes, and whitespace. The API URL and raw origin service name are
	// assigned through shell-quoted variables first so
	// heredoc expansion is not recursive.
	compose := fmt.Sprintf(`set -eu
%s

%s

QURL_CONNECTOR_ID=%s
QURL_ENDPOINT_YAML=%s
ORIGIN_SERVICE_NAME=%s
SECRET_DIR="/run/secrets/qurl/${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl/${QURL_CONNECTOR_ID}"
CONFIG_FILE="$PWD/qurl-share-${QURL_CONNECTOR_ID}.yaml"
QURL_COMPOSE_FILE="$PWD/qurl-s3-website-${QURL_CONNECTOR_ID}.compose.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF
$SUDO chmod 0644 "$CONFIG_FILE"

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
%s
%s

cat > "$QURL_COMPOSE_FILE" <<QURL_COMPOSE_YAML_EOF
services:
  %s:
    image: %s
    user: "65532:65532"
    restart: unless-stopped
    cap_drop:
      - ALL
    security_opt:
      - 'no-new-privileges:true'
    environment:
      S3_BUCKET: %s
      AWS_REGION: %s
      S3_PREFIX: %s
      INDEX_DOCUMENT: %s
      CACHE_CONNECTOR_ID: %s
  %s:
    image: %s
    user: "65532:65532"
    restart: unless-stopped
    read_only: true
    tmpfs:
      - /tmp:rw,size=64m
    pids_limit: 512
    cap_drop:
      - ALL
    security_opt:
      - 'no-new-privileges:true'
    entrypoint: ['/usr/local/bin/qurl']
    command: ['daemon', 'run', '--state-dir', '/var/lib/qurl', '--headless-config', '/etc/qurl/share.yaml', '--enrollment-token-file', '/run/secrets/qurl/enrollment-token']
    network_mode: "service:${ORIGIN_SERVICE_NAME}"
    depends_on:
      %s:
        condition: service_started
    volumes:
      - ${AGENT_STATE_DIR}:/var/lib/qurl
      - ${SECRET_DIR}:/run/secrets/qurl:ro
      - ./qurl-share-${QURL_CONNECTOR_ID}.yaml:/etc/qurl/share.yaml:ro
    environment:
      QURL_ENDPOINT: ${QURL_ENDPOINT_YAML}
QURL_COMPOSE_YAML_EOF

docker compose -f "$QURL_COMPOSE_FILE" up -d`, renderPortablePipefailShell(), renderSudoDetectionShell(), shellSingleQuote(args.Slug), shellSingleQuote(quotedAPIURL), shellSingleQuote(originServiceName), configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/enrollment-token"`), quotedOriginService, quotedOriginImage, quotedBucket, quotedRegion, quotedPrefix, quotedIndex, quotedSlug, quotedConnectorService, quotedConnectorImage, quotedOriginService)

	block, err := slackCodeBlock(compose)
	if err != nil {
		return "", err
	}
	intro := "Run this from the Docker Compose project directory on a Linux host that has IAM access to the private S3 bucket. On EC2 Docker hosts using instance roles, IMDSv2 needs hop-limit 2 for container credential access. It writes a standalone Compose file for the private S3 origin plus qURL Connector, and prompts for the enrollment token so the secret does not land in shell history."
	block, err = withHubTrustComposeEnv(block, args.Hub)
	if err != nil {
		return "", err
	}
	return intro + "\n\n" + block + "\n\nVerify with `docker compose -f qurl-s3-website-" + args.Slug + ".compose.yaml logs -f qurl-" + args.Slug + "`; after qURL connects, delete the enrollment-token file. If the S3 origin service is recreated, restart qURL too because it shares the origin network namespace.", nil
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
	intro := strings.Join([]string{
		"Use this as an " + ecsFargateChecklistText + ".",
		"Write the temporary enrollment token to `enrollment-token` on a read-only qurl-bootstrap EFS access point; never place it in task environment or argv.",
		"Run both containers in the same task; Fargate awsvpc networking lets the qURL Connector reach the private S3 origin on `127.0.0.1:" + strconv.Itoa(s3WebsiteOriginPort) + "`.",
		"Both containers are essential, so a failure of either one restarts the whole task.",
		"The START dependency orders container launch only, so the qURL Connector may log local connection errors until the origin is listening.",
		"The task role needs s3:GetObject on the objects and s3:ListBucket on the bucket.",
		"Configure qurl-agent-state for POSIX UID/GID `65532:65532`, and mount qurl-config and qurl-bootstrap read-only. Do not share qurl-agent-state across concurrently running sidecars.",
		"Both generated containers drop every Linux capability.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the enrollment token from the separate DM as the read-only qurl-bootstrap/enrollment-token file. This message intentionally does not contain it.\n\n" +
		"2. Put share.yaml at `/etc/qurl/share.yaml` on the qurl-config EFS access point:\n\n" +
		configBlock + "\n\n" +
		"3. Add these two containers to the same task definition and replace each `" + ecsLogRegionPlaceholder + "` with the ECS task region:\n\n" +
		containerBlock + "\n\n" +
		"4. Create the CloudWatch Logs group `" + s3WebsiteECSLogGroup + "` in the ECS task region if it does not already exist.\n" +
		"5. Add EFS-backed qurl-agent-state, qurl-config, and qurl-bootstrap volumes. After qURL connects, deploy a warm-start revision without the bootstrap mount/flag, verify reconnect, then delete the enrollment-token file.", nil
}

func renderS3WebsiteECSContainerJSON(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		return "", err
	}
	// The S3 origin is the protected workload, so both containers are essential:
	// losing either one should fail/restart the ECS task.
	containers := []ecsContainerDefinition{
		{
			Name:      s3WebsiteOriginContainerName,
			Image:     originImage,
			User:      ecsConnectorUser,
			Essential: true,
			Environment: []ecsEnvironmentVar{
				{Name: "S3_BUCKET", Value: args.Bucket},
				{Name: "AWS_REGION", Value: args.Region},
				{Name: "S3_PREFIX", Value: args.Prefix},
				{Name: "INDEX_DOCUMENT", Value: args.IndexDocument},
				{Name: "CACHE_CONNECTOR_ID", Value: args.Slug},
			},
			LogConfiguration: awslogsConfiguration(s3WebsiteECSLogGroup, "origin"),
			LinuxParameters:  hardenedECSLinuxParameters(),
		},
		{
			Name:                   connectorContainerName,
			Image:                  connectorImage,
			EntryPoint:             []string{"/usr/local/bin/qurl"},
			Command:                []string{"daemon", "run", "--state-dir", "/var/lib/qurl-volume/state", "--headless-config", "/etc/qurl/share.yaml", "--enrollment-token-file", "/run/secrets/qurl/enrollment-token"},
			User:                   ecsConnectorUser,
			Essential:              true,
			ReadonlyRootFilesystem: true,
			Environment: append([]ecsEnvironmentVar{
				{Name: "QURL_ENDPOINT", Value: endpoint},
			}, hubTrustECSEnv(args.Hub)...),
			MountPoints: []ecsMountPoint{
				{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/qurl-volume"},
				{SourceVolume: "qurl-config", ContainerPath: "/etc/qurl", ReadOnly: true},
				{SourceVolume: "qurl-bootstrap", ContainerPath: "/run/secrets/qurl", ReadOnly: true},
			},
			LogConfiguration: awslogsConfiguration(s3WebsiteECSLogGroup, "qurl"),
			LinuxParameters:  hardenedECSLinuxParameters(),
			DependsOn: []ecsContainerDependency{
				{ContainerName: s3WebsiteOriginContainerName, Condition: "START"},
			},
		},
	}
	return marshalECSContainerJSON(containers, "ECS S3 website container JSON")
}

func renderKubernetesS3WebsiteInstructions(args *s3WebsiteInstallArgs, connectorImage, originImage string) (string, error) {
	names := kubernetesTunnelObjectNames(args.Slug)
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		return "", err
	}
	quoted, err := yamlSingleQuotedValues(
		names.configMap, names.agentPVC, names.secret, connectorImage, originImage,
		args.Slug, endpoint, args.Bucket, args.Region,
		args.Prefix, args.IndexDocument,
	)
	if err != nil {
		return "", err
	}
	quotedConfigMap, quotedAgentPVC, quotedSecret := quoted[0], quoted[1], quoted[2]
	quotedConnectorImage, quotedOriginImage, quotedSlug := quoted[3], quoted[4], quoted[5]
	quotedEndpoint, quotedBucket, quotedRegion := quoted[6], quoted[7], quoted[8]
	quotedPrefix, quotedIndex := quoted[9], quoted[10]
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
  share.yaml: |
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
QURL_K8S_YAML_EOF`, renderPortablePipefailShell(), shellSingleQuote(names.secret), renderBootstrapKeyPromptShell(), renderBootstrapKeyToCommandShell(`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=enrollment-token=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`), quotedConfigMap, indentLines(configYAML, 4), quotedAgentPVC)

	originContainer := fmt.Sprintf(`  - name: %s
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
      - name: S3_BUCKET
        value: %s
      - name: AWS_REGION
        value: %s
      - name: S3_PREFIX
        value: %s
      - name: INDEX_DOCUMENT
        value: %s
      - name: CACHE_CONNECTOR_ID
        value: %s`, s3WebsiteOriginContainerName, quotedOriginImage, quotedBucket, quotedRegion, quotedPrefix, quotedIndex, quotedSlug)
	patch := renderKubernetesConnectorPodSpec(&kubernetesConnectorPodSpecArgs{
		precedingContainers: originContainer,
		imageYAML:           quotedConnectorImage,
		slugYAML:            quotedSlug,
		endpointYAML:        quotedEndpoint,
		agentPVCYAML:        quotedAgentPVC,
		secretYAML:          quotedSecret,
		configMapYAML:       quotedConfigMap,
	})

	objectsBlock, err := slackCodeBlock(objects)
	if err != nil {
		return "", err
	}
	patchBlock, err := slackCodeBlock(patch)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Run this once in the target namespace, then deploy the S3 origin and qurl containers in the same pod so `127.0.0.1:" + strconv.Itoa(s3WebsiteOriginPort) + "` reaches the private S3 origin.",
		"The pod identity or node role needs s3:GetObject on the objects and s3:ListBucket on the bucket.",
		"The pod-level fsGroup makes the PVC mount root writable; qURL creates its nested owner-only state directory as UID 65532.",
		"The enrollment token is streamed through your local shell into `kubectl`; do not run this from a shared, recorded, or command-traced terminal session.",
		"After the pod connects, roll out a warm-start revision without `--enrollment-token-file` or its Secret mount. Verify reconnect, then delete the enrollment-token Secret.",
	}, "\n")
	rendered := intro + "\n\n" + objectsBlock + "\n\nPod spec additions:\nMerge the generated pod securityContext, add both runtime containers under `containers:`, and append the volumes. Do not duplicate existing YAML keys.\n\n" + patchBlock
	return withHubTrustKubernetesEnv(rendered, args.Hub)
}
