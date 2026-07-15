package internal

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	agentInspectLinkLabel       = "Slack agent summary preview"
	agentInspectFetchTimeout    = 15 * time.Second
	agentInspectBodyMaxBytes    = 128 << 10
	agentInspectSessionDuration = "1m"
	agentInspectTitleMaxRunes   = 120
	agentInspectMetaMaxRunes    = 240
	agentInspectHeadingMaxCount = 6
	agentInspectHeadingMaxRunes = 80
)

var (
	agentInspectTitlePattern   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	agentInspectHeadingPattern = regexp.MustCompile(`(?is)<h[123][^>]*>(.*?)</h[123]>`)
	agentInspectMetaTagPattern = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	agentInspectAttrPattern    = regexp.MustCompile(`(?is)([a-z_:][-a-z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+))`)
	agentInspectNoisePattern   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>|<svg[^>]*>.*?</svg>`)
	agentInspectTagPattern     = regexp.MustCompile(`(?is)<[^>]+>`)
	agentInspectSpacePattern   = regexp.MustCompile(`\s+`)
)

type inspectedResource struct {
	ResourceID string
	Resource   *client.Resource
	Via        string
}

type inspectedSnapshot struct {
	Status          string
	ContentType     string
	Title           string
	MetaDescription string
	Headings        []string
}

type protectedInspectableContentError struct {
	ContentType        string
	ContentDisposition string
}

func (e *protectedInspectableContentError) Error() string {
	parts := []string{"qURL fetch returned protected document or downloadable content"}
	if e.ContentType != "" {
		parts = append(parts, "content_type="+e.ContentType)
	}
	if e.ContentDisposition != "" {
		parts = append(parts, "content_disposition="+e.ContentDisposition)
	}
	return strings.Join(parts, " ")
}

// InspectToken resolves a token to a channel-reachable resource, mints a
// short-lived internal qURL, fetches the page behind it, and returns a compact
// snapshot for the LLM to summarize. The qURL itself is intentionally never
// surfaced to the model or the user.
func (b *agentBackend) InspectToken(ctx context.Context, tc *agent.TurnContext, token string) (string, error) {
	if b.store == nil {
		return agentBackendUnconfigured, nil
	}
	token = strings.TrimPrefix(strings.TrimSpace(token), "$")
	if token == "" {
		return "Provide a $alias or $slug to inspect.", nil
	}
	c, nudge, err := b.authClientForTurn(ctx, "inspect token: client", tc)
	if err != nil {
		return "", err
	}
	if nudge != "" {
		return nudge, nil
	}
	allowed, err := b.channelAllowed(ctx, tc)
	if err != nil {
		return b.fail("inspect token: channel scope", err)
	}
	resources, err := b.channelResources(ctx, c, allowed)
	if err != nil {
		return b.fail("inspect token: resources", err)
	}
	entries, err := b.channelPolicy(ctx, tc)
	if err != nil {
		return b.fail("inspect token: aliases", err)
	}
	resolved, msg := resolveInspectableResource(token, entries, resources)
	if msg != "" {
		return msg, nil
	}

	out, err := c.Create(ctx, client.CreateInput{
		ResourceID:      resolved.ResourceID,
		Label:           agentInspectLinkLabel,
		ExpiresIn:       resourceLinkExpiry,
		OneTimeUse:      true,
		MaxSessions:     resourceMaxSessions,
		SessionDuration: agentInspectSessionDuration,
		Reason:          "Slack agent summary lookup for $" + token,
	})
	if err != nil {
		b.log.Error("agent inspect mint failed", "token", token, "resource_id", resolved.ResourceID, "error", err)
		return fmt.Sprintf("`$%s` resolves in this channel, but I couldn't open it for a summary right now.", token), nil
	}

	snapshot, err := b.fetchInspectedSnapshot(ctx, out.QURLLink, out.QURLSite)
	if err != nil {
		var unsupported *protectedInspectableContentError
		if errors.As(err, &unsupported) {
			return formatProtectedInspectableResource(token, resolved, unsupported), nil
		}
		b.log.Error("agent inspect fetch failed", "token", token, "resource_id", resolved.ResourceID, "error", err)
		return fmt.Sprintf("`$%s` resolves in this channel, but I couldn't read its page content right now.", token), nil
	}
	return formatInspectedSnapshot(token, resolved, snapshot), nil
}

func resolveInspectableResource(token string, entries []slackdata.PolicyEntry, resources []client.Resource) (resolved *inspectedResource, userMsg string) {
	byID := make(map[string]*client.Resource, len(resources))
	for i := range resources {
		byID[resources[i].ResourceID] = &resources[i]
	}
	for i := range entries {
		if entries[i].Alias != token {
			continue
		}
		if isLegacyDirectURLBinding(entries[i].ResourceID) {
			return nil, fmt.Sprintf("`$%s` is bound in this channel, but it still points at a legacy direct URL binding that can't be summarized automatically. Ask your Slack admin to rebind it to a protected resource.", token)
		}
		return &inspectedResource{ResourceID: entries[i].ResourceID, Resource: byID[entries[i].ResourceID], Via: "channel alias"}, ""
	}

	for i := range resources {
		if resources[i].Slug == token {
			return &inspectedResource{ResourceID: resources[i].ResourceID, Resource: &resources[i], Via: "connector slug"}, ""
		}
	}

	aliasMatches := make([]*client.Resource, 0, 1)
	for i := range resources {
		if resources[i].Alias == token {
			aliasMatches = append(aliasMatches, &resources[i])
		}
	}
	switch len(aliasMatches) {
	case 0:
		return nil, fmt.Sprintf("`$%s` doesn't resolve to anything reachable in this channel.", token)
	case 1:
		return &inspectedResource{ResourceID: aliasMatches[0].ResourceID, Resource: aliasMatches[0], Via: "resource alias"}, ""
	default:
		return nil, fmt.Sprintf("`$%s` matches multiple resources reachable in this channel, so I can't tell which page to summarize.", token)
	}
}

func (b *agentBackend) fetchInspectedSnapshot(ctx context.Context, rawURL, qurlSite string) (*inspectedSnapshot, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse qURL link: %w", err)
	}
	if !isInspectableFetchScheme(parsed.Scheme) {
		return nil, fmt.Errorf("unsupported qURL link scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("qURL link missing host")
	}
	parsedSite, err := url.Parse(strings.TrimSpace(qurlSite))
	if err != nil {
		return nil, fmt.Errorf("parse qURL site: %w", err)
	}
	if !isInspectableFetchScheme(parsedSite.Scheme) {
		return nil, fmt.Errorf("unsupported qURL site scheme %q", parsedSite.Scheme)
	}
	if parsedSite.Host == "" {
		return nil, errors.New("qURL site missing host")
	}
	if err := inspectAllowedEntryHost(parsed, parsedSite, b.allowInspectableLoopbackHosts); err != nil {
		return nil, err
	}
	allowedHosts := inspectAllowedRedirectHosts(parsed, parsedSite)
	fetchCtx, cancel := context.WithTimeout(ctx, agentInspectFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, parsed.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build qURL fetch request: %w", err)
	}
	req.Header.Set("User-Agent", "qurl-slack-agent/inspect")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain;q=0.9,*/*;q=0.1")

	resp, err := b.inspectedFetchClient(allowedHosts).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch qURL link: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, agentInspectBodyMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read qURL response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected qURL fetch status %s", resp.Status)
	}

	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	contentDisposition := strings.TrimSpace(resp.Header.Get("Content-Disposition"))
	if isProtectedInspectableContent(contentType, contentDisposition) {
		return nil, &protectedInspectableContentError{
			ContentType:        contentType,
			ContentDisposition: contentDisposition,
		}
	}
	if !inspectableContentType(contentType, string(body)) {
		return nil, fmt.Errorf("qURL fetch returned unsupported content type %q", contentType)
	}
	if looksLikeInteractiveAuthPage(contentType, string(body)) {
		return nil, errors.New("qURL fetch landed on an interactive authentication page")
	}
	title, metaDesc, headings := extractInspectedContent(contentType, string(body))
	return &inspectedSnapshot{
		Status:          resp.Status,
		ContentType:     contentType,
		Title:           title,
		MetaDescription: metaDesc,
		Headings:        headings,
	}, nil
}

func (b *agentBackend) inspectedFetchClient(allowedHosts map[string]struct{}) *http.Client {
	if b.fetchClient != nil {
		clone := *b.fetchClient
		if clone.Timeout == 0 {
			clone.Timeout = agentInspectFetchTimeout
		}
		clone.CheckRedirect = inspectRedirectPolicy(allowedHosts)
		return &clone
	}
	return &http.Client{Timeout: agentInspectFetchTimeout, CheckRedirect: inspectRedirectPolicy(allowedHosts)}
}

func inspectAllowedEntryHost(qurlLink, qurlSite *url.URL, allowLoopback bool) error {
	linkHost := strings.ToLower(strings.TrimSpace(qurlLink.Hostname()))
	if linkHost == "" {
		return errors.New("qURL link missing host name")
	}
	siteHost := strings.ToLower(strings.TrimSpace(qurlSite.Hostname()))
	if siteHost == "" {
		return errors.New("qURL site missing host name")
	}
	if allowLoopback && isLoopbackHostname(linkHost) && isLoopbackHostname(siteHost) {
		return nil
	}
	if isInspectableQURLLinkHost(linkHost) {
		if isInspectableQURLSiteHost(siteHost) {
			return nil
		}
		return fmt.Errorf("inspect qURL site host %q is outside the expected qURL hosts", qurlSite.Host)
	}
	return fmt.Errorf("inspect qURL link host %q is outside the expected qURL entry hosts", qurlLink.Host)
}

func inspectAllowedRedirectHosts(qurlLink, qurlSite *url.URL) map[string]struct{} {
	allowed := map[string]struct{}{}
	if qurlLink != nil && qurlLink.Host != "" {
		allowed[strings.ToLower(qurlLink.Host)] = struct{}{}
	}
	if qurlSite != nil && qurlSite.Host != "" {
		allowed[strings.ToLower(qurlSite.Host)] = struct{}{}
	}
	return allowed
}

func isInspectableQURLLinkHost(hostname string) bool {
	return hostname == "qurl.link"
}

func isInspectableFetchScheme(scheme string) bool {
	return scheme == resourceExposeSchemeHTTP || scheme == resourceExposeSchemeHTTPS
}

func isInspectableQURLSiteHost(hostname string) bool {
	return hostname == "qurl.site" || strings.HasSuffix(hostname, ".qurl.site")
}

func isLoopbackHostname(hostname string) bool {
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func isProtectedInspectableContent(contentType, contentDisposition string) bool {
	return isDownloadDisposition(contentDisposition) || isProtectedInspectableContentType(contentType)
}

func isProtectedInspectableContentType(contentType string) bool {
	lower := strings.ToLower(strings.TrimSpace(contentType))
	switch lower {
	case "application/pdf",
		"application/octet-stream",
		"application/zip",
		"application/x-zip-compressed",
		"application/gzip",
		"application/x-gzip",
		"application/x-tar",
		"application/x-7z-compressed",
		"application/x-rar-compressed",
		"application/vnd.rar",
		"application/msword",
		"application/rtf",
		"text/rtf",
		"application/epub+zip":
		return true
	}
	return strings.HasPrefix(lower, "application/vnd.openxmlformats-officedocument.") ||
		strings.HasPrefix(lower, "application/vnd.ms-") ||
		strings.HasPrefix(lower, "application/vnd.oasis.opendocument.")
}

func inspectRedirectPolicy(allowedHosts map[string]struct{}) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if req.URL == nil {
			return errors.New("inspect redirect missing target url")
		}
		if !isInspectableFetchScheme(req.URL.Scheme) {
			return fmt.Errorf("inspect redirect has unsupported scheme %q", req.URL.Scheme)
		}
		host := strings.ToLower(req.URL.Host)
		if _, ok := allowedHosts[host]; !ok {
			origin := ""
			if via[0].URL != nil {
				origin = via[0].URL.Host
			}
			return fmt.Errorf("inspect redirect crossed hosts from %q to %q", origin, req.URL.Host)
		}
		return nil
	}
}

func extractInspectedContent(contentType, raw string) (title, metaDesc string, headings []string) {
	trimmed := strings.TrimSpace(raw)
	if looksLikeHTML(contentType, trimmed) {
		sanitized := stripInspectableHTMLNoise(trimmed)
		title = truncateRunes(normalizeInspectedText(extractHTMLCapture(agentInspectTitlePattern, sanitized)), agentInspectTitleMaxRunes)
		metaDesc = truncateRunes(normalizeInspectedText(extractMetaDescription(sanitized)), agentInspectMetaMaxRunes)
		headings = extractHeadings(sanitized)
	}
	return title, metaDesc, headings
}

func stripInspectableHTMLNoise(raw string) string {
	return agentInspectNoisePattern.ReplaceAllString(raw, " ")
}

func looksLikeHTML(contentType, raw string) bool {
	if strings.Contains(contentType, "html") {
		return true
	}
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<body") || strings.Contains(lower, "<title") || strings.HasPrefix(lower, "<!doctype html")
}

func inspectableContentType(contentType, raw string) bool {
	return looksLikeHTML(contentType, raw)
}

func isDownloadDisposition(contentDisposition string) bool {
	lower := strings.ToLower(strings.TrimSpace(contentDisposition))
	return strings.HasPrefix(lower, "attachment") || strings.Contains(lower, "attachment;")
}

func extractHTMLCapture(pattern *regexp.Regexp, raw string) string {
	m := pattern.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractMetaDescription(raw string) string {
	tags := agentInspectMetaTagPattern.FindAllString(raw, -1)
	for i := range tags {
		attrs := parseHTMLAttrs(tags[i])
		name := strings.ToLower(attrs["name"])
		property := strings.ToLower(attrs["property"])
		if name == "description" || property == "og:description" {
			return attrs["content"]
		}
	}
	return ""
}

func parseHTMLAttrs(tag string) map[string]string {
	out := map[string]string{}
	matches := agentInspectAttrPattern.FindAllStringSubmatch(tag, -1)
	for i := range matches {
		val := matches[i][2]
		if val == "" {
			val = matches[i][3]
		}
		if val == "" {
			val = matches[i][4]
		}
		out[strings.ToLower(matches[i][1])] = val
	}
	return out
}

func normalizeInspectedText(s string) string {
	if s == "" {
		return ""
	}
	s = html.UnescapeString(strings.ReplaceAll(s, "\u00a0", " "))
	return strings.TrimSpace(agentInspectSpacePattern.ReplaceAllString(s, " "))
}

func extractHeadings(raw string) []string {
	matches := agentInspectHeadingPattern.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, agentInspectHeadingMaxCount)
	seen := map[string]struct{}{}
	for i := range matches {
		heading := normalizeInspectedText(agentInspectTagPattern.ReplaceAllString(matches[i][1], " "))
		if heading == "" {
			continue
		}
		heading = truncateRunes(heading, agentInspectHeadingMaxRunes)
		if _, ok := seen[heading]; ok {
			continue
		}
		seen[heading] = struct{}{}
		out = append(out, heading)
		if len(out) >= agentInspectHeadingMaxCount {
			break
		}
	}
	return out
}

func looksLikeInteractiveAuthPage(contentType, raw string) bool {
	if !looksLikeHTML(contentType, raw) {
		return false
	}
	lower := strings.ToLower(raw)
	if containsAnyString(lower,
		`type="password"`, `type='password'`,
		`autocomplete="current-password"`, `autocomplete='current-password'`,
	) {
		return true
	}
	return strings.Contains(lower, "<form") &&
		containsAnyString(lower, "sign in", "log in", "login", "single sign-on", "sso") &&
		containsAnyString(lower, "password", "username", "email")
}

func containsAnyString(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func formatInspectedSnapshot(token string, resolved *inspectedResource, snapshot *inspectedSnapshot) string {
	var b strings.Builder
	b.WriteString("Inspectable content for `$")
	b.WriteString(token)
	b.WriteString("`:\n")
	writeResolvedInspectableResourceLines(&b, resolved)
	if snapshot.Status != "" {
		b.WriteString("- Fetch status: ")
		b.WriteString(snapshot.Status)
		b.WriteString("\n")
	}
	if snapshot.ContentType != "" {
		b.WriteString("- Content type: ")
		b.WriteString(snapshot.ContentType)
		b.WriteString("\n")
	}
	if snapshot.Title != "" {
		b.WriteString("- Page title: ")
		b.WriteString(snapshot.Title)
		b.WriteString("\n")
	}
	if snapshot.MetaDescription != "" {
		b.WriteString("- Meta description: ")
		b.WriteString(snapshot.MetaDescription)
		b.WriteString("\n")
	}
	if len(snapshot.Headings) > 0 {
		b.WriteString("- Page sections for summarization only: ")
		b.WriteString(strings.Join(snapshot.Headings, " | "))
		return b.String()
	}
	b.WriteString("- Page sections for summarization only: No section headings were extracted from the page.")
	return b.String()
}

func formatProtectedInspectableResource(token string, resolved *inspectedResource, unsupported *protectedInspectableContentError) string {
	var b strings.Builder
	b.WriteString("Protected resource for `$")
	b.WriteString(token)
	b.WriteString("`:\n")
	writeResolvedInspectableResourceLines(&b, resolved)
	if unsupported != nil && unsupported.ContentType != "" {
		b.WriteString("- Content type: ")
		b.WriteString(unsupported.ContentType)
		b.WriteString("\n")
	}
	b.WriteString("- Summary availability: This resolves here as a protected resource, but it serves a document or download rather than a web page, so no website summary is available.")
	return b.String()
}

func writeResolvedInspectableResourceLines(b *strings.Builder, resolved *inspectedResource) {
	b.WriteString("- Resolved via ")
	b.WriteString(resolved.Via)
	b.WriteString(" in this channel.\n")
	if resolved.Resource != nil {
		if resolved.Resource.Description != "" {
			b.WriteString("- Resource description: ")
			b.WriteString(resolved.Resource.Description)
			b.WriteString("\n")
		}
		if resolved.Resource.Type != "" {
			b.WriteString("- Resource type: ")
			b.WriteString(resolved.Resource.Type)
			b.WriteString("\n")
		}
	}
}
