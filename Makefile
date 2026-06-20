# processkit-go — developer tasks. `make help` lists targets.
# Mirrors the CI checks so you can reproduce them locally before pushing.

GO ?= go

.DEFAULT_GOAL := help

.PHONY: help build test race lint fmt vet vuln cover tidy clean ci

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Compile all packages
	$(GO) build ./...

test: ## Run the test suite
	$(GO) test ./...

race: ## Run the test suite under the race detector (needs cgo)
	$(GO) test -race ./...

lint: ## Run golangci-lint (install: golangci-lint.run)
	golangci-lint run

fmt: ## Format with gofmt (canonical)
	gofmt -w .

vet: ## Run go vet
	$(GO) vet ./...

vuln: ## Scan dependencies with govulncheck
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

cover: ## Run tests with a coverage profile (coverage.out)
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

clean: ## Remove build/coverage artifacts
	$(GO) clean ./...
	rm -f coverage.out

ci: fmt vet lint test ## Run the local equivalent of the CI gate
