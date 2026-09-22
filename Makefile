GO                     ?= go
GOBIN                  ?= $(shell $(GO) env GOPATH)/bin
GOLANGCI_LINT_VERSION  ?= v2.13.2
GOVULNCHECK_VERSION    ?= v1.8.0
VERSION                ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAG                    ?= $(VERSION)
IMAGE_REPO             ?=
# Cloud Run runs amd64. Building on an arm64 machine without this produces an
# image the platform cannot start.
PLATFORM               ?= linux/amd64
# Optional override; the default lives in the worker Dockerfile so there is
# one place that decides which opencode the agent runs.
OPENCODE_VERSION       ?=
WORKER_BUILD_ARGS      := $(if $(OPENCODE_VERSION),--build-arg OPENCODE_VERSION=$(OPENCODE_VERSION),)
LDFLAGS                := -s -w -X main.version=$(VERSION)

.PHONY: all
all: fmt vet test build

.PHONY: build
build: bin/kibitz-server bin/kibitz-worker bin/kibitz-scaler

bin/kibitz-server: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/kibitz-server

bin/kibitz-worker: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/kibitz-worker

bin/kibitz-scaler: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/kibitz-scaler

.PHONY: test
test:
	$(GO) test -race -cover ./...

.PHONY: cover
cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: lint
lint: $(GOBIN)/golangci-lint
	$(GOBIN)/golangci-lint run

.PHONY: vuln
vuln: $(GOBIN)/govulncheck
	$(GOBIN)/govulncheck ./...

.PHONY: ci
ci: vet lint test vuln build

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: run-server
run-server:
	$(GO) run ./cmd/kibitz-server

.PHONY: run-worker
run-worker:
	$(GO) run ./cmd/kibitz-worker

.PHONY: up
up:
	docker compose up --build -d
	@echo "waiting for kibitz-server ..."
	@for i in $$(seq 1 30); do \
		if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then \
			echo "kibitz-server is healthy"; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "kibitz-server did not become healthy in time"; docker compose logs; exit 1

# Brings the stack up against the Pub/Sub emulator, creating the topic and the
# ordered subscription the worker expects.
.PHONY: up-pubsub
up-pubsub:
	docker compose --profile pubsub -f docker-compose.yml -f docker-compose.pubsub.yml up --build -d
	@echo "waiting for the pubsub emulator ..."
	@for i in $$(seq 1 30); do \
		if curl -fsS http://localhost:8085/v1/projects/kibitz-dev/topics >/dev/null 2>&1; then break; fi; \
		sleep 1; \
	done
	@$(MAKE) --no-print-directory pubsub-init

# Creates the emulator's topic and subscription. Safe to re-run.
.PHONY: pubsub-init
pubsub-init:
	@curl -fsS -X PUT http://localhost:8085/v1/projects/kibitz-dev/topics/kibitz-events >/dev/null \
		|| echo "topic already exists"
	@curl -fsS -X PUT http://localhost:8085/v1/projects/kibitz-dev/subscriptions/kibitz-worker \
		-H 'Content-Type: application/json' \
		-d '{"topic":"projects/kibitz-dev/topics/kibitz-events","enableMessageOrdering":true,"ackDeadlineSeconds":60}' >/dev/null \
		|| echo "subscription already exists"
	@echo "pubsub emulator is ready"

# Runs the tests that need the emulators (skipped by `make test`).
.PHONY: test-integration
test-integration:
	PUBSUB_EMULATOR_HOST=localhost:8085 $(GO) test -race -count=1 ./internal/queue/pubsub/...
	FIRESTORE_EMULATOR_HOST=localhost:8086 $(GO) test -race -count=1 ./internal/store/firestore/...

.PHONY: down
down:
	docker compose --profile pubsub -f docker-compose.yml -f docker-compose.pubsub.yml down -v

# Builds the images for the local machine's architecture. For images that
# will run on Cloud Run, use `make push`, which builds for linux/amd64.
.PHONY: docker-build
docker-build:
	docker build -f deploy/docker/Dockerfile.server --build-arg VERSION=$(VERSION) -t kibitz-server:$(VERSION) .
	docker build -f deploy/docker/Dockerfile.worker --build-arg VERSION=$(VERSION) $(WORKER_BUILD_ARGS) -t kibitz-worker:$(VERSION) .
	docker build -f deploy/docker/Dockerfile.scaler --build-arg VERSION=$(VERSION) -t kibitz-scaler:$(VERSION) .

# Builds the images for the deployment platform and pushes them. IMAGE_REPO
# is the Artifact Registry repository, which `terraform output
# image_repository` prints.
#
#   make push IMAGE_REPO=asia-northeast1-docker.pkg.dev/my-project/kibitz TAG=v0.1.0
#
# buildx builds and pushes in one step, which is also what makes the
# cross-architecture build work from an arm64 machine.
.PHONY: push
push:
	@test -n "$(IMAGE_REPO)" || { echo "IMAGE_REPO is required, e.g. make push IMAGE_REPO=REGION-docker.pkg.dev/PROJECT/kibitz"; exit 1; }
	docker buildx build --platform $(PLATFORM) -f deploy/docker/Dockerfile.server \
		--build-arg VERSION=$(TAG) -t $(IMAGE_REPO)/kibitz-server:$(TAG) --push .
	docker buildx build --platform $(PLATFORM) -f deploy/docker/Dockerfile.worker \
		--build-arg VERSION=$(TAG) $(WORKER_BUILD_ARGS) -t $(IMAGE_REPO)/kibitz-worker:$(TAG) --push .
	docker buildx build --platform $(PLATFORM) -f deploy/docker/Dockerfile.scaler \
		--build-arg VERSION=$(TAG) -t $(IMAGE_REPO)/kibitz-scaler:$(TAG) --push .
	@echo
	@echo "server_image = \"$(IMAGE_REPO)/kibitz-server:$(TAG)\""
	@echo "worker_image = \"$(IMAGE_REPO)/kibitz-worker:$(TAG)\""
	@echo "scaler_image = \"$(IMAGE_REPO)/kibitz-scaler:$(TAG)\""

.PHONY: clean
clean:
	rm -rf bin coverage.out

$(GOBIN)/golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOBIN)/govulncheck:
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
