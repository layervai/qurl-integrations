package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	resourceExposeUsage       = "Usage:\n• `/qurl-admin resource expose $<resource-alias> [as:$channel-alias]`\n• `/qurl-admin resource expose url:<target-url> as:$channel-alias`"
	resourceExposeSchemeHTTP  = "http"
	resourceExposeSchemeHTTPS = "https"
)

type resourceExposeArgs struct {
	ResourceAlias string
	TargetURL     string
	ChannelAlias  string
}

func stripResourcePrefix(text string) string {
	_, rest := slashVerb(text, "resource")
	return rest
}

func parseResourceExposeArgs(text string) (parsed *resourceExposeArgs, userMsg string) {
	tokens := strings.Fields(text)
	if len(tokens) < 2 || len(tokens) > 3 || tokens[0] != "expose" {
		return nil, resourceExposeUsage
	}

	args := &resourceExposeArgs{}
	if len(tokens) == 3 {
		if !strings.HasPrefix(tokens[2], "as:") {
			return nil, resourceExposeUsage
		}
		alias, reason := validateAliasTokenForNoun(strings.TrimPrefix(tokens[2], "as:"), "Channel alias", "channel alias")
		if reason != "" {
			return nil, reason + "\n\n" + resourceExposeUsage
		}
		args.ChannelAlias = alias
	}

	target := tokens[1]
	if strings.HasPrefix(target, "$") {
		alias, reason := validateAliasTokenForNoun(target, "Resource alias", "resource alias")
		if reason != "" {
			return nil, reason + "\n\n" + resourceExposeUsage
		}
		args.ResourceAlias = alias
		if args.ChannelAlias == "" {
			args.ChannelAlias = alias
		}
		return args, ""
	}

	if strings.HasPrefix(target, "url:") {
		targetURL := strings.TrimSpace(strings.TrimPrefix(target, "url:"))
		if targetURL == "" {
			return nil, "Missing URL after `url:`.\n\n" + resourceExposeUsage
		}
		parsed, err := url.Parse(targetURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != resourceExposeSchemeHTTP && parsed.Scheme != resourceExposeSchemeHTTPS) {
			return nil, "URL target must be an absolute http or https URL.\n\n" + resourceExposeUsage
		}
		if args.ChannelAlias == "" {
			return nil, "`url:<target-url>` requires `as:$channel-alias` so Slack users have a friendly name.\n\n" + resourceExposeUsage
		}
		args.TargetURL = targetURL
		return args, ""
	}

	return nil, "Target must be a resource alias like `$docs`, or `url:<target-url>` with `as:$channel-alias`.\n\n" + resourceExposeUsage
}

// handleResource routes `/qurl-admin resource expose ...`.
//
// Minimal scope: expose an existing URL resource to THIS Slack channel by
// binding a channel alias to the resource_id. Dashboard remains connector-blind,
// and Slack users never type opaque `r_...` resource IDs.
func (h *Handler) handleResource(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	args, userMsg := parseResourceExposeArgs(stripResourcePrefix(text))
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	teamID, channelID, ok := h.aliasValidate(w, values, "resource expose")
	if !ok {
		return
	}
	if !h.requireAliasAdminGate(w, teamID, values, AdminActionResourceExpose) {
		return
	}

	h.runAsync(w, "resource_expose", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.exposeURLResourceInChannel(ctx, log, teamID, channelID, args)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

func (h *Handler) exposeURLResourceInChannel(ctx context.Context, log *slog.Logger, teamID, channelID string, args *resourceExposeArgs) string {
	resource, err := h.resolveURLResourceForExpose(ctx, log, teamID, args)
	if err != nil {
		var userErr *userError
		if errors.As(err, &userErr) {
			return userErr.msg
		}
		log.Error("resource expose: unexpected resource lookup error", "error", err, "team_id", teamID)
		return "Failed to expose URL resource. Please try again."
	}

	err = h.aliasStore.BindChannelAlias(ctx, teamID, channelID, args.ChannelAlias, resource.ResourceID)
	if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", args.ChannelAlias, args.ChannelAlias)
	}
	if err != nil {
		log.Error("resource expose: alias bind failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.ChannelAlias, "resource_id", resource.ResourceID)
		return "Failed to expose URL resource. Please try again."
	}

	log.Info("URL resource exposed to Slack channel", "team_id", teamID, "channel_id", channelID, "channel_alias", args.ChannelAlias, "resource_alias", resource.Alias, "resource_id", resource.ResourceID)
	if resource.Alias != "" {
		return fmt.Sprintf("URL resource `$%s` is now available as `$%s` in this channel. Run `/qurl get $%s` to create a qURL.", resource.Alias, args.ChannelAlias, args.ChannelAlias)
	}
	return fmt.Sprintf("URL resource is now available as `$%s` in this channel. Run `/qurl get $%s` to create a qURL.", args.ChannelAlias, args.ChannelAlias)
}

func (h *Handler) resolveURLResourceForExpose(ctx context.Context, log *slog.Logger, teamID string, args *resourceExposeArgs) (*client.Resource, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("resource expose: API key lookup failed", "error", err, "team_id", teamID)
		return nil, &userError{msg: "Failed to look up URL resource. Please try again."}
	}
	// Bare-minimum import path: reuse the same first-page scan as /qurl list/get
	// URL-alias fallback. A qurl-service alias lookup would remove this boundary,
	// but this keeps Slack connector ownership without a backend API change.
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Warn("resource expose: resource lookup failed", "error", err, "team_id", teamID)
		return nil, &userError{msg: sanitizeAPIError(err, "Failed to look up URL resource")}
	}

	var matches []client.Resource
	for i := range page.Resources {
		resource := page.Resources[i]
		if resource.Status == client.StatusRevoked || !isURLResource(&resource) {
			continue
		}
		if args.ResourceAlias != "" && resource.Alias == args.ResourceAlias {
			matches = append(matches, resource)
			continue
		}
		if args.TargetURL != "" && resource.TargetURL == args.TargetURL {
			matches = append(matches, resource)
		}
	}
	if page.HasMore {
		log.Debug("resource expose: scanned first resource page only", "scan_limit", listResourcesScanLimit, "team_id", teamID)
	}
	if len(matches) == 0 {
		if args.ResourceAlias != "" {
			return nil, &userError{msg: fmt.Sprintf("No active URL resource `$%s` was found. Check the protected resource alias in the dashboard, then retry.", args.ResourceAlias)}
		}
		return nil, &userError{msg: "No active URL resource was found for that exact target URL. The `url:` value must match the dashboard URL exactly."}
	}
	if len(matches) > 1 {
		if args.ResourceAlias != "" {
			return nil, &userError{msg: fmt.Sprintf("`$%s` matches multiple active URL resources. Give the protected resource a unique alias in the dashboard, then retry.", args.ResourceAlias)}
		}
		return nil, &userError{msg: "That target URL matches multiple active URL resources. Give the protected resource a unique alias in the dashboard, then retry."}
	}
	return &matches[0], nil
}
