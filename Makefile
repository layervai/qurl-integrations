.PHONY: all fmt lint vet test test-race coverage build-slack build-cli docs man vendor release-snapshot security check clean

VERSION ?= dev

all: check build-slack build-cli

## Formatting

fmt:
	gofmt -w .
	goimports -w -local github.com/layervai/qurl-integrations .

## Linting

lint:
	golangci-lint run --timeout=5m ./...

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
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o release/slack/bootstrap ./apps/slack/cmd/

build-cli: # Builds for host OS/arch (developer machine). Cross-compile manually if needed.
	CGO_ENABLED=0 go build -ldflags="-w -s -X main.version=$(VERSION)" -o release/cli/qurl ./apps/cli/cmd/

## Documentation

docs: # Generate markdown docs for the CLI
	go run ./apps/cli/tools/gendocs markdown ./docs/cli

man: # Generate man pages for the CLI
	go run ./apps/cli/tools/gendocs man ./man

## Vendoring (for reproducible builds / Homebrew core)

vendor:
	go mod vendor
	go mod tidy

## Release (requires goreleaser)

release-snapshot: # Build release artifacts without publishing
	goreleaser release --snapshot --clean

## Security

security:
	govulncheck ./...

## Pre-commit

pre-commit-install:
	pip install pre-commit
	pre-commit install

pre-commit-run:
	pre-commit run --all-files

## Full check (CI parity)

check: fmt vet lint test-race

## Cleanup

clean:
	rm -rf release/ coverage.out docs/cli/ man/
