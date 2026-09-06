BINARY := bin/bwrap-agent
GO_PACKAGE := ./cmd/bwrap-agent
GO_BUILD_FLAGS := -buildvcs=false -trimpath
PREFIX ?= /usr/local

.PHONY: all build test test-race vet integration dist install clean

all: build

build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o $(BINARY) $(GO_PACKAGE)

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

integration: build
	./tests/integration.sh ./$(BINARY)

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o dist/bwrap-agent-linux-amd64 $(GO_PACKAGE)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GO_BUILD_FLAGS) -o dist/bwrap-agent-linux-arm64 $(GO_PACKAGE)
	cp LICENSE THIRD_PARTY_NOTICES.md dist/
	cd dist && sha256sum bwrap-agent-linux-amd64 bwrap-agent-linux-arm64 > SHA256SUMS

install: build
	install -Dm755 $(BINARY) "$(DESTDIR)$(PREFIX)/bin/bwrap-agent"

clean:
	rm -f $(BINARY)
	rm -rf dist
