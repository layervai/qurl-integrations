# Contractor Onboarding

Welcome to qurl-integrations. This doc covers everything you need to start shipping code.

## 1. Repo Setup

```bash
git clone git@github.com:layervai/qurl-integrations.git
cd qurl-integrations

# Install pre-commit hooks (mandatory — blocks secrets and lint errors before push)
pip install pre-commit && pre-commit install

# Verify your setup
make check
```

### Required tools

- Go 1.26+ (`go version`)
- golangci-lint v2.10+ (`golangci-lint --version`)
- pre-commit (`pre-commit --version`)
- gh CLI (`gh --version`) — for opening PRs

## 2. Where Your Code Goes

```
apps/
  slack/              # Slack integration (reference implementation)
    cmd/main.go       # Lambda entry point
    internal/         # Your handler logic, models, helpers
    README.md         # App-specific docs
  teams/              # <-- your app here
  discord/            # <-- your app here
shared/               # Shared libraries (platform team owns these)
  client/             # QURL API client
  auth/               # API key provider
  formatting/         # Chat message templates
  observability/      # Structured logging
```

Your work goes in `apps/{your-app}/`. Follow the Slack app as a reference.

### Directory structure for a new app

```
apps/your-app/
  cmd/main.go              # Lambda entrypoint
  internal/
    handler.go             # Request routing + business logic
    handler_test.go        # Tests (required — CI enforces coverage)
  README.md                # What it does, env vars, how to test
  CHANGELOG.md             # Release Please manages this — start with "# Changelog\n"
```

### Entrypoint pattern

Every app is an AWS Lambda behind API Gateway. Use this skeleton:

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/aws/aws-lambda-go/events"
    "github.com/aws/aws-lambda-go/lambda"

    "github.com/layervai/qurl-integrations/apps/yourapp/internal"
    "github.com/layervai/qurl-integrations/shared/auth"
    "github.com/layervai/qurl-integrations/shared/client"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
    slog.SetDefault(logger)

    endpoint := os.Getenv("QURL_ENDPOINT")
    if endpoint == "" {
        endpoint = "https://api.layerv.xyz"
    }

    authProvider := auth.EnvProvider{EnvVar: "QURL_API_KEY"}

    handler := internal.NewHandler(internal.Config{
        QURLEndpoint: endpoint,
        AuthProvider: &authProvider,
        NewClient: func(apiKey string) *client.Client {
            return client.New(endpoint, apiKey)
        },
    })

    lambda.Start(func(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
        return handler.Handle(ctx, &req)
    })
}
```

### Using the QURL API client

```go
import "github.com/layervai/qurl-integrations/shared/client"

c := client.New("https://api.layerv.xyz", apiKey)

// Create a QURL
qurl, err := c.Create(ctx, client.CreateInput{TargetURL: "https://example.com"})

// List QURLs
result, err := c.List(ctx, client.ListInput{Limit: 10})
```

If you need something not in `shared/`, put it in your app's `internal/` first. Open an issue if multiple apps need it.

## 3. Development Workflow

```bash
# Start from latest main
git checkout main && git pull

# Create a branch
git checkout -b feat/teams-slash-commands

# Write code + tests, then verify locally
make check          # fmt + vet + lint + test (matches CI exactly)
make coverage       # check you're above 40% threshold
make build-slack    # or: go build ./apps/yourapp/cmd/

# Push and open a PR
git push -u origin feat/teams-slash-commands
gh pr create --title "feat(teams): add slash command handler"
```

### What CI checks on every PR

| Check | What it does |
|-------|-------------|
| **Lint** | golangci-lint with 28 linters |
| **Test** | `go test -race` + 40% coverage minimum |
| **Build** | Lambda binary compiles |
| **Vulnerability Scan** | govulncheck |
| **TruffleHog** | Scans for leaked secrets |
| **PR Title** | Must match `type(scope): description` |
| **Dependency Review** | Blocks high-severity or GPL deps |
| **Code Review** | Platform team must approve |

All checks must pass. PRs are squash-merged with the PR title as the commit message.

### Things that will block your PR

- Pushing to main (branch protection rejects it)
- Hardcoded secrets (TruffleHog + GitHub push protection catch these)
- Coverage below 40%
- `//nolint` without an explanation and specific linter name
- GPL-3.0 or AGPL-3.0 dependencies
- Modifying `terraform/`, `.github/`, or `shared/` without platform team review

## 4. AWS Access via IAM Identity Center

You have access to the **sandbox account only**. Production is CI/CD only — you cannot access it directly.

### First-time setup

Add this to `~/.aws/config`:

```ini
[profile layerv-integrations]
sso_session = layerv
sso_account_id = <sandbox-account-id>
sso_role_name = IntegrationsDeveloper
region = us-east-2

[sso-session layerv]
sso_start_url = https://layerv.awsapps.com/start
sso_region = us-east-2
sso_registration_scopes = sso:account:access
```

Replace `<sandbox-account-id>` with the account ID provided in your onboarding email.

### Daily usage

```bash
# Log in (opens browser, lasts 8 hours)
aws sso login --profile layerv-integrations

# Use it
AWS_PROFILE=layerv-integrations aws lambda list-functions
AWS_PROFILE=layerv-integrations aws logs tail /aws/lambda/integrations-slack-sandbox

# Or export it for your session
export AWS_PROFILE=layerv-integrations
aws ssm get-parameter --name /integrations/slack/signing-secret
```

### What you can access

| Service | Access |
|---------|--------|
| Lambda | Create, update, invoke, view logs |
| API Gateway | Full management |
| CloudWatch Logs | Read and write |
| SSM Parameter Store | Read/write under `/integrations/*` |
| Secrets Manager | Read/write under `integrations/*` |
| S3 | Deploy artifact buckets only |

### What you cannot access

EC2, VPC, DynamoDB, ECS, EKS, RDS, IAM (except scoped), Route53, or any production account resources.

### AWS Console access

Visit https://layerv.awsapps.com/start, sign in, and select the sandbox account with `IntegrationsDeveloper` role.

## 5. Secrets and Configuration

Never hardcode secrets. Store them in the sandbox account:

```bash
# Store a secret
AWS_PROFILE=layerv-integrations aws secretsmanager create-secret \
  --name integrations/yourapp/api-key \
  --secret-string "your-secret-value"

# Store a config parameter
AWS_PROFILE=layerv-integrations aws ssm put-parameter \
  --name /integrations/yourapp/endpoint \
  --type String \
  --value "https://api.layerv.xyz"
```

Your Lambda reads these at startup via environment variables or SDK calls.

## 6. Testing Your Lambda Locally

```bash
# Build the binary
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bootstrap ./apps/yourapp/cmd/

# Test with the Lambda RIE (Runtime Interface Emulator)
# See: https://docs.aws.amazon.com/lambda/latest/dg/go-image.html#go-image-clients
```

Or just write good unit tests — they're faster and CI enforces them.

## Questions?

- **Build/CI issues**: Open an issue tagged `ci`
- **Shared package questions**: Open an issue tagged `shared`
- **AWS access issues**: Contact the platform team directly
