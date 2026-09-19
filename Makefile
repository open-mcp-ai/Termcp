.PHONY: build build-debug test clean dist sync-assets

# Plain `make` must keep meaning "build the release binary" (GNU make otherwise
# picks the first target in the file as the default goal).
.DEFAULT_GOAL := build

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN := termcp-$(GOOS)-$(GOARCH)
ifeq ($(GOOS),windows)
BIN := $(BIN).exe
endif

# release 模式：-s 去掉符号表，-w 去掉 DWARF 调试信息，-trimpath 去掉本机路径等个人信息
LDFLAGS_RELEASE := -s -w

# 版本元数据：优先取 git tag（describe --tags --always 回退到短哈希），
# 编译时注入 main.version / main.commit / main.date，使 `termcp -version`
# 自动跟随 tag，无需手工改代码。无 .git 时（源码打包分发）自动降级为空。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)

LDFLAGS_VERSION := $(if $(VERSION),-X main.version=$(VERSION)) \
                   $(if $(COMMIT),-X main.commit=$(COMMIT)) \
                   $(if $(DATE),-X main.date=$(DATE))

# 构建目标（默认 release）
build:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS_RELEASE) $(LDFLAGS_VERSION)" -o dist/$(BIN) .

# 调试模式构建（保留符号信息，便于 delve 调试）
build-debug:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -gcflags "all=-N -l" -o dist/$(BIN) .

# 运行全部单元测试（-count=1 跳过测试结果缓存，保证每次都真实执行）
test:
	go test -count=1 ./...

# 清理构建文件
clean:
	rm -rf dist

# 构建并清理旧文件
dist: clean build

# 同步对外服务的文档副本：assets/api.md 通过 HTTP（/api.md）与 MCP resource 对外
# 发布，TestSyncedDocsMatchSource 会拦住漂移；MCP 工具参数由 tools/list schema
# 自描述，因此不再同步 docs/mcp-tools.md。
sync-assets:
	cp docs/api.md internal/webui/assets/api.md
