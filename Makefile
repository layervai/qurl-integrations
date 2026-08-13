.PHONY: all fmt lint vet test test-race coverage build-slack build-cli docs man vendor release-snapshot security check check-actions-pins test-actions-pins test-install-script check-release-please-sync check-extension-lockstep check-notification-payload test-validated-base check-discord test-discord check-chrome-extension check-edge-extension check-teams check-node pre-commit-install pre-commit-run clean

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

check-extension-lockstep:
	scripts/check-extension-lockstep.sh

check-notification-payload:
	scripts/check-main-ci-notification-payload.sh

test-validated-base:
	scripts/test-resolve-validated-base.sh

## Pre-commit

pre-commit-install:
	pip install pre-commit
	pre-commit install

pre-commit-run:
	pre-commit run --all-files

## Node.js apps (Discord bot, Chrome/Edge extensions, Teams)
##
## Deliberately NOT wired into `make check`: each app installs from its own
## lockfile, so folding them in would put four `npm ci` runs on the target
## every Go contributor runs. Run the one matching your change instead —
## `make check-node` runs all four. Each mirrors its app's CI gate, so a
## green run here predicts the `<app> / required` aggregate.
##
## e2e/ has no target: its suite drives live qURL and Discord systems and
## needs credentials from e2e/.env (see e2e/README.md). It has no CI gate
## either — running it is a deliberate act, not a pre-push check.

# $(1) is the app directory. Every app pins its own Node in .nvmrc and CI
# feeds that file to setup-node, so a mismatched local Node can pass here and
# fail there. Warn rather than fail: a newer Node usually works, and a hard
# failure would make the target unusable for anyone not running nvm.
define node_version_warning
@if command -v node >/dev/null 2>&1 && [ "$$(node --version)" != "v$$(cat $(1)/.nvmrc)" ]; then \
	echo "warning: node $$(node --version) differs from $(1)/.nvmrc v$$(cat $(1)/.nvmrc) (CI uses the pinned version)" >&2; \
fi
endef

# Matches CI's jest flags minus --silent, kept verbose for local debugging
# (discord.yml runs `npm test -- --ci --silent`); --no-audit --no-fund are
# local-only conveniences that mute npm output without changing the tree.
test-discord:
	$(call node_version_warning,apps/discord)
	cd apps/discord && npm ci --no-audit --no-fund && npm test -- --ci

# CI's discord gate minus `npm audit` (network-dependent and can newly fail
# with no code change; CI owns that gate) and the Docker build (needs a
# running daemon).
check-discord:
	$(call node_version_warning,apps/discord)
	cd apps/discord && npm ci --no-audit --no-fund && npm run lint && npm test -- --ci

# The extensions' CI gate minus `npm run package:release`, which writes
# release/ and dist/ into the app dir and shells out to `zip`. Nothing is
# lost: build-release.js and package-release.js each have a dedicated suite
# under test/, which `npm test` runs. The syntax check is CI's verbatim —
# globbed so a source file added later cannot slip past it, and piped through
# xargs because `find -exec` reports success even when the command it ran
# failed. Chrome↔Edge lockstep is not repeated here; `make check` already
# runs check-extension-lockstep for every PR.
check-chrome-extension:
	$(call node_version_warning,apps/chrome-extension)
	cd apps/chrome-extension && npm ci --no-audit --no-fund
	cd apps/chrome-extension && npm run lint
	cd apps/chrome-extension && find background.js popup content lib scripts -name '*.js' -print0 | xargs -0 -r -n1 node --check
	cd apps/chrome-extension && npm test

check-edge-extension:
	$(call node_version_warning,apps/edge-extension)
	cd apps/edge-extension && npm ci --no-audit --no-fund
	cd apps/edge-extension && npm run lint
	cd apps/edge-extension && find background.js popup content lib scripts -name '*.js' -print0 | xargs -0 -r -n1 node --check
	cd apps/edge-extension && npm test

# teams.yml's gate in full — every step is offline and fast. `npm run build`
# only writes the gitignored dist/.
check-teams:
	$(call node_version_warning,apps/teams)
	cd apps/teams && npm ci --no-audit --no-fund
	cd apps/teams && npm run typecheck
	cd apps/teams && npm run lint
	cd apps/teams && npm test
	cd apps/teams && npm run build

# Every Node.js app at once, for changes that cross app boundaries (a shared
# lint convention, a repo-wide rename). Minutes, not seconds — prefer the
# single check-<app> when only one app moved.
check-node: check-chrome-extension check-edge-extension check-discord check-teams

## Full check (Go + repo-wide checks, matching the Go CI path; the Node.js
## app suites are opt-in above — `make check-node` or a single `check-<app>`)

check: fmt vet check-actions-pins test-actions-pins test-install-script check-release-please-sync check-extension-lockstep check-notification-payload test-validated-base lint test-race

## Cleanup

clean:
	rm -rf release/ coverage.out docs/cli/ man/
