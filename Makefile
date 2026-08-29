VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64

.PHONY: test
test:
	go vet ./...
	go test ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

.PHONY: check
check: fmt-check test

.PHONY: build
build:
	go build -ldflags '$(LDFLAGS)' -o dist/kloudy-agent ./cmd/kloudy-agent

# Release binaries are static and CGO-free so they run on any glibc or musl
# distribution without a runtime dependency on the customer's machine.
.PHONY: dist
dist: clean
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/kloudy-agent-$$os-$$arch ./cmd/kloudy-agent || exit 1; \
	done
	@$(MAKE) --no-print-directory checksums

# The install script verifies these before executing anything it downloaded.
# Publishing a release without them turns the agent into an unauthenticated code
# path onto every server running it.
.PHONY: checksums
checksums:
	@cd dist && shasum -a 256 kloudy-agent-* > SHA256SUMS && cat SHA256SUMS

.PHONY: clean
clean:
	@rm -rf dist
