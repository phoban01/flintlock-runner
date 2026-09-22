# flintlock-runner build, test, lint and requirement-tracing targets.
# `make help` lists them.

GO                   ?= go
GOTOOLCHAIN          ?= auto
DUVET                ?= duvet
GOLANGCI_LINT_VERSION ?= v2.13.2
BIN                  ?= bin
# SETUP_ENVTEST_VERSION pins controller-runtime's setup-envtest (release-0.24,
# which goes with the k8s.io v0.36 modules in go.mod) and ENVTEST_K8S_VERSION
# the kube-apiserver and etcd it downloads for `make envtest`.
SETUP_ENVTEST_VERSION ?= v0.24.2-0.20260713111223-0f529e22d5c0
ENVTEST_K8S_VERSION  ?= 1.36.x
# IDS is the space- or comma-separated list of requirement identifiers for
# `make coverage-gate`, for example IDS="SC-001 SC-002" or IDS=TD-001..010.
IDS                  ?=
# DEMO_ARGS are passed to flintlock-devstack by `make demo`, for example
# DEMO_ARGS='-jobs 3', DEMO_ARGS="-script 'exit 3'" or DEMO_ARGS=-keep.
DEMO_ARGS            ?=
# E2E_FLAGS are extra `go test` flags for `make e2e`, for example -v or
# -run 'TestFakeTier/successful'.
E2E_FLAGS            ?=

export GOTOOLCHAIN

.PHONY: help build test envtest vet lint tidy-check duvet duvet-ci duvet-open e2e demo coverage-gate clean

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | column -t -s ':'

## build: compile every package and the binaries into bin/
build:
	$(GO) build ./...
	$(GO) build -o $(BIN)/flr ./cmd/flr
	$(GO) build -o $(BIN)/fake-poolmgr ./cmd/fake-poolmgr
	$(GO) build -o $(BIN)/flintlock-devstack ./cmd/flintlock-devstack

## test: run every Go test with the race detector
# The Kubernetes pool backend's tests need a kube-apiserver and etcd (KF-121).
# `go test ./...` on its own finds the ones `make envtest` has downloaded and
# skips those tests when there are none; here they are downloaded first and
# required, so this target never skips them.
test:
	KUBEBUILDER_ASSETS="$$($(MAKE) -s envtest)" FLINTLOCK_RUNNER_REQUIRE_ENVTEST=1 $(GO) test -race ./...

## envtest: download the kube-apiserver and etcd of the API server test environment and print their directory
envtest:
	@$(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path

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
# --ci false is explicit: duvet turns the snapshot check on by itself when CI is
# set in the environment, which would make this target reject every PR that adds
# a citation. The snapshot is checked only by duvet-ci, at milestones.
duvet:
	rm -rf .duvet/requirements
	$(DUVET) report --ci false

## duvet-ci: same as duvet but fail if .duvet/snapshot.txt would change (milestones only)
duvet-ci:
	rm -rf .duvet/requirements
	$(DUVET) report --ci true

## duvet-open: build the report and open it in a browser
duvet-open: duvet
	xdg-open .duvet/reports/report.html 2>/dev/null || open .duvet/reports/report.html

## coverage-gate: fail unless every ID in IDS has an implementation and a test citation
coverage-gate:
	@if [ -z "$(IDS)" ]; then echo 'usage: make coverage-gate IDS="SC-001 SC-002"' >&2; exit 2; fi
	DUVET=$(DUVET) hack/duvet-coverage.sh $(IDS)

## e2e: run the end-to-end scenarios against the real binary and the fakes (TD-050; build tag e2e)
# The scenarios are behind the e2e build tag so that `go test ./...` stays
# fast. FLINTLOCK_RUNNER_E2E_INVENTORY selects the hardware tier (TD-052) and
# FLINTLOCK_RUNNER_E2E_POOL_MANAGER a real Pool Manager (TD-053).
e2e:
	$(GO) build -o $(BIN)/flr ./cmd/flr
	FLINTLOCK_RUNNER_E2E_BINARY=$(abspath $(BIN))/flr \
		$(GO) test -race -tags e2e -count=1 -timeout 15m $(E2E_FLAGS) ./internal/testing/harness/...

## demo: run jobs through the real runner on a local fake stack, no KVM needed (DEMO_ARGS='-keep', '-jobs 3', "-script 'exit 3'")
demo:
	$(GO) build -o $(BIN)/flr ./cmd/flr
	$(GO) build -o $(BIN)/flintlock-devstack ./cmd/flintlock-devstack
	$(BIN)/flintlock-devstack -runner-bin $(BIN)/flr $(DEMO_ARGS)

## clean: remove build outputs and generated duvet files
clean:
	rm -rf $(BIN) .duvet/requirements .duvet/reports
