# Linux builds run inside manylinux2014 (glibc 2.17), the baseline CLIProxyAPI
# uses for its own release binaries. macOS and Windows libraries are built
# natively by CI (.github/workflows/build.yml).
GO_VERSION ?= 1.26.8
CPA_VERSION ?= 8.0.11
VERSION ?= $(shell cat VERSION)
PLUGIN_DIR := build/plugins/linux/amd64
PLUGIN_SO := $(PLUGIN_DIR)/cliproxyapi-ollama.so
DIST_DIR := dist
CACHE_DIR := .cache
LDFLAGS := -s -w -X main.pluginVersion=$(VERSION)

.PHONY: test vet build build-local package smoke verify clean

test:
	go test ./...

vet:
	go vet ./...

build:
	GO_VERSION=$(GO_VERSION) scripts/build-linux.sh amd64 $(VERSION) $(PLUGIN_SO)

# Host build; only safe when the host glibc is not newer than the target's.
build-local:
	mkdir -p $(PLUGIN_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cliproxyapi-ollama
	rm -f $(PLUGIN_DIR)/cliproxyapi-ollama.h

# Store release asset for linux/amd64: dist/cliproxyapi-ollama_<version>_linux_amd64.zip + checksums.txt
package: build
	rm -rf $(DIST_DIR)
	go run ./tools/releasetool package -lib $(PLUGIN_SO) -out $(DIST_DIR) -version $(VERSION) -goos linux -goarch amd64
	go run ./tools/releasetool checksums -dir $(DIST_DIR)
	go run ./tools/releasetool verify -dir $(DIST_DIR) -version $(VERSION) -platforms linux/amd64

# Loads the plugin into a real CLIProxyAPI release binary against a fake Ollama.
smoke: build
	go run ./tools/smoke -plugin $(PLUGIN_SO) -cpa-version $(CPA_VERSION) -expect-version $(VERSION)

# Verifies a complete store release (all five platforms) in $(DIST_DIR).
verify:
	go run ./tools/releasetool verify -dir $(DIST_DIR) -version $(VERSION)

clean:
	rm -rf build $(DIST_DIR) $(CACHE_DIR)
