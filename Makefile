GO                     ?= go
GOBIN                  ?= $(shell $(GO) env GOPATH)/bin
GOLANGCI_LINT_VERSION  ?= v2.5.0
VERSION                ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS                := -s -w -X main.version=$(VERSION)

.PHONY: all
all: fmt vet test build

.PHONY: build
build: bin/kibitz-server bin/kibitz-worker

bin/kibitz-server: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/kibitz-server

bin/kibitz-worker: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/kibitz-worker

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

.PHONY: docker-build
docker-build:
	docker build -f deploy/docker/Dockerfile.server -t kibitz-server:$(VERSION) .
	docker build -f deploy/docker/Dockerfile.worker -t kibitz-worker:$(VERSION) .

.PHONY: clean
clean:
	rm -rf bin coverage.out

$(GOBIN)/golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOBIN)/govulncheck:
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
