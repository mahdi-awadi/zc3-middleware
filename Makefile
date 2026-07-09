BINARY      := middleware
PKG         := ./cmd/middleware
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: all build build-linux run test vet tidy clean docker

all: build

build: ## Build host binary -> bin/
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

build-linux: ## Static linux/amd64 binary -> dist/
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)

build-arm64: ## Static linux/arm64 (e.g. Raspberry Pi) -> dist/
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-arm64 $(PKG)

run: ## Run with .env
	go run $(PKG)

test: ## Run tests
	go test ./...

vet: ## go vet
	go vet ./...

tidy: ## Tidy modules
	go mod tidy

docker: ## Build the container image
	docker build -t $(BINARY):$(VERSION) .

clean:
	rm -rf bin dist
