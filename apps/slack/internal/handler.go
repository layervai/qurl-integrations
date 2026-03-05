// Package internal contains Slack-specific handler logic.
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const methodPost = "POST"

// Config holds the Slack handler configuration.
type Config struct {
	QURLEndpoint       string
	AuthProvider       auth.Provider
	SlackSigningSecret string
	NewClient          func(apiKey string) *client.Client
}

// Handler processes Slack events and commands.
type Handler struct {
	cfg Config
}

// NewHandler creates a new Slack handler.
func NewHandler(cfg Config) *Handler {
	return &Handler{cfg: cfg}
}

// Handle routes incoming API Gateway requests to the appropriate handler.
func (h *Handler) Handle(ctx context.Context, req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	slog.Info("received request", "path", req.Path, "method", req.HTTPMethod)

	switch {
	case req.Path == "/slack/commands" && req.HTTPMethod == methodPost:
		return h.handleSlashCommand(ctx, req)
	case req.Path == "/slack/events" && req.HTTPMethod == methodPost:
		return h.handleEvent(req)
	case req.Path == "/slack/interactions" && req.HTTPMethod == methodPost:
		return h.handleInteraction(req)
	case req.Path == "/health":
		return respond(http.StatusOK, map[string]string{"status": "ok"})
	default:
		return respond(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) handleSlashCommand(ctx context.Context, req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	values, err := url.ParseQuery(req.Body)
	if err != nil {
		return respond(http.StatusBadRequest, map[string]string{"error": "invalid form body"})
	}

	command := values.Get("command")
	text := strings.TrimSpace(values.Get("text"))

	slog.Info("slash command", "command", command, "text", text)

	switch {
	case text == "" || text == "help":
		return respondSlack(helpMessage())
	case strings.HasPrefix(text, "create "):
		return h.handleCreate(ctx, values)
	case strings.HasPrefix(text, "list"):
		return h.handleList(ctx, values)
	default:
		return respondSlack(fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
	}
}

func (h *Handler) handleCreate(ctx context.Context, values url.Values) (events.APIGatewayProxyResponse, error) {
	text := strings.TrimSpace(values.Get("text"))
	targetURL := strings.TrimPrefix(text, "create ")
	targetURL = strings.TrimSpace(targetURL)

	if targetURL == "" {
		return respondSlack("Usage: `/qurl create <url>`")
	}

	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		return respondSlack("Failed to authenticate. Please check your QURL API key configuration.")
	}

	c := h.cfg.NewClient(apiKey)
	qurl, err := c.Create(ctx, client.CreateInput{TargetURL: targetURL})
	if err != nil {
		slog.Error("failed to create QURL", "error", err, "target_url", targetURL)
		return respondSlack("Failed to create QURL: " + err.Error())
	}

	return respondSlack(fmt.Sprintf("QURL created!\n*Link:* %s\n*Target:* %s", qurl.LinkURL, qurl.TargetURL))
}

func (h *Handler) handleList(ctx context.Context, values url.Values) (events.APIGatewayProxyResponse, error) {
	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		return respondSlack("Failed to authenticate. Please check your QURL API key configuration.")
	}

	c := h.cfg.NewClient(apiKey)
	result, err := c.List(ctx, client.ListInput{Limit: 5})
	if err != nil {
		slog.Error("failed to list QURLs", "error", err)
		return respondSlack("Failed to list QURLs: " + err.Error())
	}

	if len(result.QURLs) == 0 {
		return respondSlack("No QURLs found.")
	}

	lines := make([]string, 0, len(result.QURLs))
	for i := range result.QURLs {
		q := &result.QURLs[i]
		line := fmt.Sprintf("• %s → %s (%d clicks)", q.LinkURL, q.TargetURL, q.ClickCount)
		if q.Title != "" {
			line = fmt.Sprintf("• *%s* — %s → %s (%d clicks)", q.Title, q.LinkURL, q.TargetURL, q.ClickCount)
		}
		lines = append(lines, line)
	}

	return respondSlack("*Recent QURLs:*\n" + strings.Join(lines, "\n"))
}

func (h *Handler) handleEvent(req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Handle Slack URL verification challenge.
	var body struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(req.Body), &body); err == nil && body.Type == "url_verification" {
		return respond(http.StatusOK, map[string]string{"challenge": body.Challenge})
	}

	// TODO: Handle link_shared events for unfurling.
	slog.Info("event received", "body_length", len(req.Body))
	return respond(http.StatusOK, map[string]string{"ok": "true"})
}

func (h *Handler) handleInteraction(req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// TODO: Handle interactive components (buttons, modals).
	slog.Info("interaction received", "body_length", len(req.Body))
	return respond(http.StatusOK, map[string]string{"ok": "true"})
}

func helpMessage() string {
	return `*/qurl* — Create and manage QURLs from Slack

*Commands:*
• ` + "`/qurl create <url>`" + ` — Create a QURL for the given URL
• ` + "`/qurl list`" + ` — Show your 5 most recent QURLs
• ` + "`/qurl help`" + ` — Show this help message`
}

func respond(status int, body any) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(body)
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}, nil
}

func respondSlack(text string) (events.APIGatewayProxyResponse, error) {
	return respond(http.StatusOK, map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
}
