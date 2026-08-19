.PHONY: all fmt lint vet test test-race coverage build-slack build-cli docs man vendor release-snapshot security check check-actions-pins test-actions-pins test-install-script check-release-please-sync test-release-please-sync test-cli-release-verifier check-extension-lockstep check-notification-payload test-validated-base check-cli check-discord test-discord check-chrome-extension check-edge-extension check-teams check-e2e check-node pre-commit-install pre-commit-run clean check-i18n-parity test-i18n-parity

VERSION ?= dev

all: check build-slack build-cli

## Formatting

fmt:
	gofmt -w .
	goimports -w -local github.com/layervai/qurl-integrations .

## Linting

# Pinned so local runs match CI exactly. Keep in sync with every pin site:
# .github/workflows/slack.yml (2), .github/workflows/shared-test.yml (2),
# .github/workflows/cli.yml (2), .github/workflows/workflow-contract.yml (1),
# and .pre-commit-config.yaml's golangci-lint rev. An unpinned PATH install
# drifts: newer golangci-lint versions flag issues the pinned config is clean
# on.
GOLANGCI_LINT_VERSION := v2.12.2

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

## Release (requires goreleaser + syft; .goreleaser.yml's sboms block shells
## out to syft). Signing is skipped: keyless cosign needs GitHub's OIDC
## broker, which only exists on Actions runs — see `signs:` in .goreleaser.yml.

release-snapshot: # Build release artifacts without publishing
	goreleaser release --snapshot --clean --skip=sign

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

check-i18n-parity:
	scripts/check-i18n-parity.sh

test-i18n-parity:
	scripts/test-check-i18n-parity.sh

check-notification-payload:
	scripts/check-main-ci-notification-payload.sh

test-validated-base:
	scripts/test-resolve-validated-base.sh

test-cli-release-verifier:
	scripts/test-verify-cli-release.sh

test-release-please-sync:
	scripts/test-check-release-please-sync.sh

## Pre-commit

pre-commit-install:
	pip install pre-commit
	pre-commit install

pre-commit-run:
	pre-commit run --all-files

## Node.js suites (Discord, Chrome/Edge extensions, Teams, e2e helpers)
##
## Opt-in, never prerequisites of `make check`: each installs from its own
## lockfile, and that does not belong on the target every Go contributor runs.
## CONTRIBUTING.md#nodejs-apps says which to run when. Each mirrors its app's
## CI job closely enough to predict it — output-only flags differ, and what a
## target omits is noted on the target.
##
## Side effect worth knowing: these create node_modules/ that Go's `./...` walk
## then sees (eslint's flatted ships a .go file). .golangci.yml already drops
## findings under node_modules/, so `lint` is covered; `vet` and `test-race`
## are not, so a dep shipping malformed Go would surface as a `make check`
## failure with no Go change behind it. Harmless today — measured at ~0.7s of
## extra package loading, and the one Go file in there is clean.

# $(1) is the app directory. Every app pins its own Node in .nvmrc and CI
# feeds that file to setup-node, so a mismatched local Node can pass here and
# fail there. Warn rather than fail: a newer Node usually works, and a hard
# failure would make the target unusable for anyone not running nvm.
define node_version_warning
@if command -v node >/dev/null 2>&1 && [ "$$(node --version)" != "v$$(cat $(1)/.nvmrc)" ]; then \
	echo "warning: node $$(node --version) differs from $(1)/.nvmrc v$$(cat $(1)/.nvmrc) (CI uses the pinned version)" >&2; \
fi
endef

# Every target below uses `npm ci`, not `npm install`: CI installs the lockfile
# exactly, and `npm install` can rewrite package-lock.json — which both dirties
# the tree and breaks the "this predicts CI" property. `--no-audit --no-fund`
# only mute npm output; they do not change the tree.

# cli.yml's quality gates for the host OS, so a contributor can run them
# before pushing. Adding or removing a gate there means updating this target
# too. What it deliberately cannot mirror: the three-OS matrix (only the host
# OS runs here; the workflow's per-OS keyring setup is dormant until apps/cli
# references a keyring, so omitting it matches CI today) and the sandbox
# gate's vars.CLI_SANDBOX_E2E arming plus repo-wide serialization — the tagged
# test command itself still runs. `goreleaser check` needs goreleaser on PATH,
# like release-snapshot above. The 40 floor mirrors cli.yml's coverage gate;
# raise both together once the v2 code lands.
check-cli:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./apps/cli/...
	@go test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./apps/cli/...
	@COVERAGE=$$(go tool cover -func=coverage.out | grep ^total: | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if ! awk -v c="$$COVERAGE" 'BEGIN { exit !(c+0 >= 40) }' </dev/null; then \
		echo "FAIL: Coverage $${COVERAGE}% is below 40% threshold"; \
		exit 1; \
	fi
	go vet ./apps/cli/...
	go tool govulncheck ./apps/cli/...
	QURL_TEST_HARNESS=1 go test -count=1 ./apps/cli/...
	go test -tags=clisandbox -count=1 ./apps/cli/...
	goreleaser check

# Kept verbose for local debugging — discord.yml adds --silent.
test-discord:
	$(call node_version_warning,apps/discord)
	cd apps/discord && npm ci --no-audit --no-fund
	cd apps/discord && npm test -- --ci

# discord.yml's build-and-test steps minus `npm audit`, which is network
# dependent and can newly fail with no code change. Its sibling docker-check
# job is a separate gate and is not mirrored here.
check-discord:
	$(call node_version_warning,apps/discord)
	cd apps/discord && npm ci --no-audit --no-fund
	cd apps/discord && npm run lint
	cd apps/discord && npm test -- --ci

# $(1) is the extension app directory. The recipe is shared rather than written
# once per extension on purpose: this Makefile is outside the file list in
# scripts/check-extension-lockstep.sh, so a hand-copied second copy could drift
# from the first with nothing in CI to catch it.
#
# Mirrors the extensions' build-and-test steps minus `npm run package:release`,
# which writes release/ and dist/ and shells out to zip. That omission is the
# one place these targets are a weaker signal than CI: `npm test` covers
# build-release.js and package-release.js through their own suites, but not the
# real zip-writing path — run it by hand before a store submission.
#
# The syntax check is CI's command verbatim, run through `bash -o pipefail`
# because that is the shell GitHub gives a `run:` step. Without it a Make
# recipe line runs under /bin/sh, the pipeline reports only xargs's status,
# and a `find` that failed — a globbed root renamed in some later refactor —
# would pass here while failing CI. That is the one asymmetry this file's
# "predicts CI" claim cannot afford. Globbed so a source file added later
# cannot slip past the check, and piped through xargs rather than `find
# -exec`, which reports success even when the command it ran failed.
define check_extension
$(call node_version_warning,$(1))
cd $(1) && npm ci --no-audit --no-fund
cd $(1) && npm run lint
cd $(1) && bash -o pipefail -c "find background.js popup content lib scripts -name '*.js' -print0 | xargs -0 -r -n1 node --check"
cd $(1) && npm test
endef

check-chrome-extension:
	$(call check_extension,apps/chrome-extension)

check-edge-extension:
	$(call check_extension,apps/edge-extension)

# teams.yml's build-and-test in full — every step is offline. `npm run build`
# writes only dist/, which apps/teams/.gitignore already covers.
check-teams:
	$(call node_version_warning,apps/teams)
	cd apps/teams && npm ci --no-audit --no-fund
	cd apps/teams && npm run typecheck
	cd apps/teams && npm run lint
	cd apps/teams && npm test
	cd apps/teams && npm run build

# e2e.yml's build-and-test in full. Offline subset only: `npm test` there also
# runs the live suite, which mints real qURL resources and posts real Discord
# messages against credentials in e2e/.env (see e2e/README.md). CI omits that
# script for the same reason and has no such credentials, so the two stay
# step-for-step identical rather than this being the weaker signal.
#
# e2e/.nvmrc pins 24.x where the apps pin 22.21.0: e2e/package.json asks for
# node >=24, and npm does not enforce engines by default, so that file is the
# only thing making the two agree. It landed with the CI workflow that reads
# it — before that there was nothing to verify a pin.
#
# -p is explicit rather than relying on tsc's upward search, and names the same
# config ts-jest compiles with (neither jest config overrides it). Note the
# typecheck is deliberately the wider net: tsconfig.json includes **/*.ts, so
# it covers helpers/ and the live tests/ too, while test:unit runs only
# unit/**. For the live suite that typecheck is the only gate there is.
check-e2e:
	$(call node_version_warning,e2e)
	cd e2e && npm ci --no-audit --no-fund
	cd e2e && npx tsc -p tsconfig.json --noEmit
	cd e2e && npm run test:unit

# Every Node.js suite at once, for changes that cross app boundaries; prefer a
# single target when only one app moved. They write into separate directories
# and npm's cache takes its own locks, so `make -j5 check-node` is safe — it
# finishes with the slowest app rather than the sum of all five.
check-node: check-chrome-extension check-edge-extension check-discord check-teams check-e2e

## Full check (Go + repo-wide checks, matching the Go CI path; the Node.js
## suites are opt-in above — `make check-node` or a single `check-<app>`)

check: fmt vet check-actions-pins test-actions-pins test-install-script check-release-please-sync test-release-please-sync test-cli-release-verifier check-extension-lockstep check-i18n-parity test-i18n-parity check-notification-payload test-validated-base lint test-race

## Cleanup

clean:
	rm -rf release/ coverage.out docs/cli/ man/
