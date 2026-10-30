COMMIT := $(or $(shell git rev-parse HEAD 2>/dev/null | cut -c1-7),unknown)
COMMIT_DATE := $(shell TZ=UTC git log -1 --date=format-local:%Y.%m.%d --format=%cd 2>/dev/null)
BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
VERSION ?= $(if $(COMMIT_DATE),$(if $(filter main,$(BRANCH)),v,vdev)$(COMMIT_DATE).$(COMMIT)$(shell git diff --quiet HEAD 2>/dev/null || echo -dirty),dev)
ifndef BUILD_DATE
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
endif

LDFLAGS := -s -w \
           -X github.com/nicolaeser/dockpaum/internal/buildinfo.Version=$(VERSION) \
           -X github.com/nicolaeser/dockpaum/internal/buildinfo.Commit=$(COMMIT) \
           -X github.com/nicolaeser/dockpaum/internal/buildinfo.Date=$(BUILD_DATE)

GO  ?= go
BIN ?= bin/dockpaum
PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6
STATICCHECK_VERSION   ?= v0.8.2
GOLANGCI_LINT_VERSION ?= v2.15.0
DOCKER_ARGS := --build-arg VERSION=$(VERSION) \
               --build-arg COMMIT=$(COMMIT) \
               --build-arg BUILD_DATE=$(BUILD_DATE)

.PHONY: all build test cover vet fmt fmt-check tidy lint vuln check schema docker docker-multiarch clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/dockpaum

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l cmd internal)" || { echo 'gofmt needed on:'; gofmt -l cmd internal; exit 1; }

tidy:
	$(GO) mod tidy

lint:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln:
	$(GO) tool govulncheck ./...

check: fmt-check vet lint test vuln
	$(GO) mod tidy -diff

schema:
	$(GO) test ./internal/config -run TestSchema -update

docker:
	docker build $(DOCKER_ARGS) -t ghcr.io/nicolaeser/dockpaum:latest .

docker-multiarch:
	docker buildx build --platform $(PLATFORMS) $(DOCKER_ARGS) -t ghcr.io/nicolaeser/dockpaum:latest .

clean:
	rm -rf bin coverage.out
