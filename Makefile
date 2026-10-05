# Builds the CLIProxyAPI native plugin. The target image (eceasy/cli-proxy-api:v8.0.11)
# runs on debian:bookworm (glibc 2.36), so the default build uses the same
# golang:1.26-bookworm toolchain image CLIProxyAPI itself is built with.
GO_IMAGE ?= golang:1.26-bookworm
VERSION ?= 0.1.0
PLUGIN_DIR := build/plugins/linux/amd64
PLUGIN_SO := $(PLUGIN_DIR)/cliproxyapi-ollama.so
CACHE_DIR := .cache
LDFLAGS := -s -w -X main.pluginVersion=$(VERSION)

.PHONY: test vet build build-local checksum clean

test:
	go test ./...

vet:
	go vet ./...

build:
	mkdir -p $(PLUGIN_DIR) $(CACHE_DIR)/go-build $(CACHE_DIR)/go-mod $(CACHE_DIR)/home
	docker run --rm \
		--user "$$(id -u):$$(id -g)" \
		-e HOME=/src/$(CACHE_DIR)/home \
		-e GOCACHE=/src/$(CACHE_DIR)/go-build \
		-e GOMODCACHE=/src/$(CACHE_DIR)/go-mod \
		-e GOTOOLCHAIN=local \
		-v "$(CURDIR):/src" \
		-w /src \
		$(GO_IMAGE) \
		sh -ec 'CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cliproxyapi-ollama'
	rm -f $(PLUGIN_DIR)/cliproxyapi-ollama.h
	@$(MAKE) --no-print-directory checksum

# Host build; only safe when the host glibc is not newer than the container's.
build-local:
	mkdir -p $(PLUGIN_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cliproxyapi-ollama
	rm -f $(PLUGIN_DIR)/cliproxyapi-ollama.h

checksum:
	sha256sum $(PLUGIN_SO)

clean:
	rm -rf build $(CACHE_DIR)
