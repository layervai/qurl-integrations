.PHONY: all fmt lint vet test test-race coverage build-slack build-cli security check clean

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

build-cli:
	CGO_ENABLED=0 go build -ldflags="-w -s -X main.version=$(VERSION)" -o release/cli/qurl ./apps/cli/cmd/

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
	rm -rf release/ coverage.out
