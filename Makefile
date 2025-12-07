
WORKSPACE_RESULTS_PATH ?= /tmp/image
export KO_DOCKER_REPO=registry.arthurvardevanyan.com/homelab/bmc-shim
export KO_DEFAULTBASEIMAGE=cgr.dev/chainguard/static:latest
TAG ?= $(shell date --utc '+%Y%m%d-%H%M')
EXPIRE ?= 180d

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: build
build:
	go build -C cmd/bmc-shim -o /tmp/bmc-shim

.PHONY: run
run: build
	/tmp/bmc-shim

.PHONY: ko-build
ko-build:
	ko build ./cmd/bmc-shim --platform=linux/amd64,linux/arm64 --bare --sbom none --image-label quay.expires-after="${EXPIRE}" --tags "${TAG}"

.PHONY: ko-build-pipeline
ko-build-pipeline:
	ko build ./cmd/bmc-shim --platform=linux/amd64,linux/arm64 --bare --sbom none --image-label quay.expires-after="${EXPIRE}" --tags "${TAG}"
	echo "${KO_DOCKER_REPO}:${TAG}" > "${WORKSPACE_RESULTS_PATH}"
