.PHONY: all fmt lint vet test test-race coverage build-slack build-cli docs man vendor release-snapshot security check check-actions-pins test-actions-pins test-install-script check-release-please-sync check-discord test-discord pre-commit-install pre-commit-run clean

VERSION ?= dev

all: check build-slack build-cli

## Formatting

fmt:
	gofmt -w .
	goimports -w -local github.com/layervai/qurl-integrations .

## Linting

# Pinned so local runs match CI exactly. Keep in sync with every pin site:
# .github/workflows/slack.yml (2), .github/workflows/shared-test.yml (2),
# and .pre-commit-config.yaml's golangci-lint rev. An unpinned PATH install
# drifts: newer golangci-lint versions flag issues the pinned config is
# clean on.
GOLANGCI_LINT_VERSION := v2.10.1

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...

vet:
	go vet ./...

## Testing

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

coverage:
	@go test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	@COVERAGE=$$(go tool cover -func=coverage.out | grep ^total: | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if [ "$$(echo "$$COVERAGE < 40" | bc -l)" -eq 1 ]; then \
		echo "FAIL: Coverage $${COVERAGE}% is below 40% threshold"; \
		exit 1; \
	fi

## Building

build-slack:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-w -s -X main.version=$(VERSION)" -o release/slack/qurl-bot-slack ./apps/slack/cmd/

build-cli: # Builds for host OS/arch (developer machine). Cross-compile manually if needed.
	CGO_ENABLED=0 go build -ldflags="-w -s -X main.version=$(VERSION)" -o release/cli/qurl ./apps/cli/cmd/

## Documentation

docs: build-cli # Generate markdown docs for the CLI
	./release/cli/qurl docs markdown -d ./docs/cli

man: build-cli # Generate man pages for the CLI
	./release/cli/qurl docs man -d ./man

## Vendoring (for reproducible builds / Homebrew core)

vendor:
	go mod vendor
	go mod tidy

## Release (requires goreleaser)

release-snapshot: # Build release artifacts without publishing
	goreleaser release --snapshot --clean

## Security

security:
	go tool govulncheck ./...

check-actions-pins:
	scripts/validate-github-actions-pins.sh

test-actions-pins:
	scripts/test-validate-github-actions-pins.sh

## Scripts

test-install-script:
	scripts/test-install.sh

check-release-please-sync:
	scripts/check-release-please-sync.sh

## Pre-commit

pre-commit-install:
	pip install pre-commit
	pre-commit install

pre-commit-run:
	pre-commit run --all-files

## Discord bot (Node.js)

define discord_node_warning
@if command -v node >/dev/null 2>&1 && [ "$$(node --version)" != "v$$(cat apps/discord/.nvmrc)" ]; then \
	echo "warning: node $$(node --version) differs from apps/discord/.nvmrc v$$(cat apps/discord/.nvmrc) (CI uses the pinned version)" >&2; \
fi
endef

# Matches CI's jest flags minus --silent, kept verbose for local debugging
# (discord.yml runs `npm test -- --ci --silent`); --no-audit --no-fund are
# local-only conveniences that mute npm output without changing the tree.
test-discord:
	$(discord_node_warning)
	cd apps/discord && npm ci --no-audit --no-fund && npm test -- --ci

# CI's discord gate minus `npm audit` (network-dependent and can newly fail
# with no code change; CI owns that gate).
check-discord:
	$(discord_node_warning)
	cd apps/discord && npm ci --no-audit --no-fund && npm run lint && npm test -- --ci

## Full check (Go + repo checks, matching the Go CI path; app suites
## run via their own CI gates or make check-discord)

check: fmt vet check-actions-pins test-actions-pins test-install-script check-release-please-sync lint test-race

## Cleanup

clean:
	rm -rf release/ coverage.out docs/cli/ man/
