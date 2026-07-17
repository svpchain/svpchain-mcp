#!/usr/bin/make -f

VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Match the version strings the binary already imports transitively from
# cosmos-sdk (mirrors cmd/mcp-server/Dockerfile).
ldflags = -X github.com/cosmos/cosmos-sdk/version.Name=svpchain \
	-X github.com/cosmos/cosmos-sdk/version.AppName=svpchain-mcp \
	-X github.com/cosmos/cosmos-sdk/version.Version=$(VERSION) \
	-X github.com/cosmos/cosmos-sdk/version.Commit=$(COMMIT)

BUILD_FLAGS := -ldflags '$(ldflags)'

.PHONY: build install test vet vendor docker clean

# Build the mcp-server binary into ./build. Uses the go.mod replace directive
# pointing at the sibling protocol checkout (../svpchain-main/protocol).
build:
	go build -mod=readonly $(BUILD_FLAGS) -o build/mcp-server ./cmd/mcp-server
	go build -mod=readonly $(BUILD_FLAGS) -o build/devsign ./cmd/devsign

install:
	go install -mod=readonly $(BUILD_FLAGS) ./cmd/mcp-server

test:
	go test ./...

vet:
	go vet ./...

# Materialize dependencies into ./vendor so the Docker image can build without
# the sibling protocol checkout or GOPRIVATE credentials. Run before `docker`.
vendor:
	go mod vendor

# Build the deployable image. Vendors first so the build context is
# self-contained (the replace target is not inside the Docker context).
docker: vendor
	docker build --platform linux/amd64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t svpchain-mcp:$(VERSION) \
		-f cmd/mcp-server/Dockerfile .

clean:
	rm -rf build/ vendor/
