BINARY := bin/bwrap-agent
INTEGRATION_SERVER := bin/bwrap-agent-integration-server
GO_PACKAGE := ./cmd/bwrap-agent
GO_BUILD_FLAGS := -buildvcs=false -trimpath
PREFIX ?= /usr/local
HOST_OS := $(shell uname -s)
DEV_CONTAINER := ./tests/dev-container.sh

.PHONY: all build test test-race vet check integration integration-testcontainers dist install clean dev-shell \
	build-native test-native test-race-native vet-native check-native integration-native integration-testcontainers-native dist-native

all: build

build-native:
	mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o $(BINARY) $(GO_PACKAGE)

test-native:
	go test ./...

test-race-native:
	CGO_ENABLED=1 go test -race ./...

vet-native:
	go vet ./...

check-native: test-native vet-native

integration-native: build-native
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o $(INTEGRATION_SERVER) ./tests/fixtures/port-server
	./tests/integration.sh ./$(BINARY) ./$(INTEGRATION_SERVER)

integration-testcontainers-native: build-native
	./tests/testcontainers.sh ./$(BINARY)

dist-native:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o dist/bwrap-agent-linux-amd64 $(GO_PACKAGE)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GO_BUILD_FLAGS) -o dist/bwrap-agent-linux-arm64 $(GO_PACKAGE)
	cp LICENSE THIRD_PARTY_NOTICES.md dist/
	cd dist && sha256sum bwrap-agent-linux-amd64 bwrap-agent-linux-arm64 > SHA256SUMS

ifeq ($(HOST_OS),Linux)
build: build-native
test: test-native
test-race: test-race-native
vet: vet-native
check: check-native
integration: integration-native
integration-testcontainers: integration-testcontainers-native
dist: dist-native
else ifeq ($(HOST_OS),Darwin)
build:
	$(DEV_CONTAINER) run make build-native
test:
	$(DEV_CONTAINER) run make test-native
test-race:
	$(DEV_CONTAINER) run make test-race-native
vet:
	$(DEV_CONTAINER) run make vet-native
check:
	$(DEV_CONTAINER) run make check-native
integration:
	$(DEV_CONTAINER) privileged make integration-native
integration-testcontainers:
	$(DEV_CONTAINER) privileged make integration-testcontainers-native
dist:
	$(DEV_CONTAINER) run make dist-native
else
$(error unsupported development host $(HOST_OS); use Linux or macOS with Docker)
endif

dev-shell:
	$(DEV_CONTAINER) shell

ifeq ($(HOST_OS),Linux)
install: build
	install -Dm755 $(BINARY) "$(DESTDIR)$(PREFIX)/bin/bwrap-agent"
else
install:
	@echo "bwrap-agent: install is only supported on Linux." >&2
	@exit 1
endif

clean:
	rm -f $(BINARY)
	rm -f $(INTEGRATION_SERVER)
	rm -rf dist
