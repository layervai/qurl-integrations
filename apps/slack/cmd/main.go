package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	qurlEndpoint := os.Getenv("QURL_ENDPOINT")
	if qurlEndpoint == "" {
		qurlEndpoint = "https://api.layerv.xyz"
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		slog.Error("SLACK_SIGNING_SECRET is required")
		os.Exit(1)
	}

	authProvider := auth.EnvProvider{EnvVar: "QURL_API_KEY"}

	handler := internal.NewHandler(internal.Config{
		QURLEndpoint:       qurlEndpoint,
		AuthProvider:       &authProvider,
		SlackSigningSecret: slackSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlEndpoint, apiKey)
		},
	})

	lambda.Start(func(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return handler.Handle(ctx, req)
	})
}
