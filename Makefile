.DEFAULT_GOAL := help

.PHONY: build coverage help lint race security test verify

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z_-]+:.*## / {printf "%-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the Gofer binary.
	go build -trimpath -o bin/gofer .

test: ## Run unit tests.
	go test ./...

coverage: ## Enforce the unit-test coverage threshold.
	scripts/coverage.sh

lint: ## Run formatting, documentation, static analysis, and complexity checks.
	scripts/quality.sh

race: ## Run tests with the race detector.
	scripts/race.sh

security: ## Scan reachable dependencies for known vulnerabilities.
	scripts/security.sh

verify: ## Run the local delivery gate.
	scripts/smoke.sh
