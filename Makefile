# flintlock-runner build, test, lint and requirement-tracing targets.
# `make help` lists them.

GO                   ?= go
GOTOOLCHAIN          ?= auto
DUVET                ?= duvet
GOLANGCI_LINT_VERSION ?= v2.13.2
BIN                  ?= bin
# IDS is the space- or comma-separated list of requirement identifiers for
# `make coverage-gate`, for example IDS="SC-001 SC-002" or IDS=TD-001..010.
IDS                  ?=

export GOTOOLCHAIN

.PHONY: help build test vet lint tidy-check duvet duvet-ci duvet-open e2e coverage-gate clean

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | column -t -s ':'

## build: compile every package and the flintlock-runner binary into bin/
build:
	$(GO) build ./...
	$(GO) build -o $(BIN)/flintlock-runner ./cmd/flintlock-runner

## test: run every Go test with the race detector
test:
	$(GO) test -race ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint at the pinned version via go run (no install needed)
lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

## tidy-check: fail if go.mod or go.sum would change under go mod tidy
tidy-check:
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

## duvet: extract requirements, build the HTML/JSON report and refresh the snapshot
duvet:
	rm -rf .duvet/requirements
	$(DUVET) report

## duvet-ci: same as duvet but fail if .duvet/snapshot.txt would change (milestones only)
duvet-ci:
	rm -rf .duvet/requirements
	$(DUVET) report --ci

## duvet-open: build the report and open it in a browser
duvet-open: duvet
	xdg-open .duvet/reports/report.html 2>/dev/null || open .duvet/reports/report.html

## coverage-gate: fail unless every ID in IDS has an implementation and a test citation
coverage-gate:
	@if [ -z "$(IDS)" ]; then echo 'usage: make coverage-gate IDS="SC-001 SC-002"' >&2; exit 2; fi
	DUVET=$(DUVET) hack/duvet-coverage.sh $(IDS)

## e2e: run the end-to-end harness (TD-050); not implemented yet
e2e:
	@echo "make e2e: the end-to-end harness (TD-050) is not implemented yet." >&2
	@echo "It lands with the 'fakes' and 'executor' work packages (docs/PLAN.md, milestone M1)." >&2
	@exit 2

## clean: remove build outputs and generated duvet files
clean:
	rm -rf $(BIN) .duvet/requirements .duvet/reports
