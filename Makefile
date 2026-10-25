BINARY    := grokbot-cli
PKG       := github.com/exa-pub/grokbot-cli
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X $(PKG)/internal/cli.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build test test-race lint proto proto-check dist clean

all: lint test build

# --- Build ---
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/grokbot-cli

# Release archives dist/grokbot-cli-<os>-<arch>.tar.gz and dist/checksums.txt.
dist:
	rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; name=$(BINARY)-$$os-$$arch; \
		echo "build $$name $(VERSION)"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$name ./cmd/grokbot-cli || exit 1; \
		tar -C dist -czf dist/$$name.tar.gz $$name || exit 1; \
		rm dist/$$name; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt

# --- Checks ---
test:
	go test ./...

test-race:
	go test -race ./...

lint:
	$(GOLANGCI_LINT) run ./...

# --- Proto ---
proto:
	./scripts/gen-proto.sh

# Fails if the committed generated code is out of date.
proto-check: proto
	git diff --exit-code -- internal/gen

clean:
	rm -rf bin dist
