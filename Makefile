PLUGIN_ID    ?= zcode
PLUGIN_NAME  ?= $(PLUGIN_ID)
VERSION      ?= $(shell sed -n 's/.*pluginVersion = "\(.*\)"/\1/p' core.go | head -1)
BUILD_DIR    ?= dist
GOOS         ?= linux
GOARCH       ?= amd64
CPA_SRC      ?= /cpa-src
GO_IMAGE     ?= golang:1.26.10-bookworm

EXT_linux   = so
EXT_freebsd = so
EXT_darwin  = dylib
EXT_windows = dll
PLUGIN_EXT   = $(or $(EXT_$(GOOS)),so)

PLUGIN_OUTPUT   ?= $(BUILD_DIR)/$(PLUGIN_ID).$(PLUGIN_EXT)
ARCHIVE_NAME    ?= $(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip
ARCHIVE_PATH    ?= $(BUILD_DIR)/$(ARCHIVE_NAME)
CHECKSUMS_PATH  ?= $(BUILD_DIR)/checksums.txt

# 插件依赖 CPA 的 SDK (go.mod 里 replace 到 /cpa-src), 因此必须挂载 CPA 源码构建。
# 本机没有 1.26 工具链时走容器: make docker-build CPA_SRC=/path/to/CLIProxyAPI
DOCKER_RUN = docker run --rm \
	-u $$(id -u):$$(id -g) -e HOME=/tmp \
	-e GOPROXY=$${GOPROXY:-https://proxy.golang.org,direct} \
	-e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
	-v $(abspath $(CPA_SRC)):/cpa-src:ro -v $(CURDIR):/src -w /src $(GO_IMAGE)

.PHONY: build docker-build test docker-test vet clean package checksums release-assets

# 打包时用哪个构建目标: 本机工具链满足 go.mod 就用 build, 否则 make package BUILD=docker-build
BUILD ?= build

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w" -o $(PLUGIN_OUTPUT) .
	rm -f $(basename $(PLUGIN_OUTPUT)).h

docker-build:
	mkdir -p $(BUILD_DIR)
	$(DOCKER_RUN) sh -c 'CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o /src/$(PLUGIN_OUTPUT) . && rm -f /src/$(basename $(PLUGIN_OUTPUT)).h'

test:
	CGO_ENABLED=1 go test -race -count=1 ./...

docker-test:
	$(DOCKER_RUN) sh -c 'CGO_ENABLED=1 go vet ./... && CGO_ENABLED=1 go test -race -count=1 ./...'

vet:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR)

# CPA 插件商店要求的 release 资产: <id>_<version>_<goos>_<goarch>.zip
# zip 内必须是 <id>.so (或 <id>-v<version>.so), 商店安装时按此查找。
package: $(BUILD)
	rm -rf $(BUILD_DIR)/stage $(ARCHIVE_PATH)
	mkdir -p $(BUILD_DIR)/stage
	cp $(PLUGIN_OUTPUT) $(BUILD_DIR)/stage/$(PLUGIN_ID).$(PLUGIN_EXT)
	cd $(BUILD_DIR)/stage && zip -q -r ../$(ARCHIVE_NAME) .
	@echo "打包完成: $(ARCHIVE_PATH)"

checksums: package
	cd $(BUILD_DIR) && sha256sum $(ARCHIVE_NAME) > $(notdir $(CHECKSUMS_PATH))

release-assets: checksums
	@echo "待上传到 GitHub Release 的资产:"
	@ls -la $(ARCHIVE_PATH) $(CHECKSUMS_PATH)
