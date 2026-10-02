.PHONY: build build-api build-debug test clean dist sync-assets

# Plain `make` must keep meaning "build the release binary" (GNU make otherwise
# picks the first target in the file as the default goal).
.DEFAULT_GOAL := build

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN := termcp-$(GOOS)-$(GOARCH)
ifeq ($(GOOS),windows)
BIN := $(BIN).exe
endif
HBIN := termcp-api-$(GOOS)-$(GOARCH)
ifeq ($(GOOS),windows)
HBIN := $(HBIN).exe
endif

# release 模式：-s 去掉符号表，-w 去掉 DWARF 调试信息，-trimpath 去掉本机路径等个人信息
LDFLAGS_RELEASE := -s -w

# 只有 HEAD 正好带版本 tag 时才使用该 tag；其他提交标为 dev-<commit>，
# 避免在重写/分叉的历史上把旧 tag 当成当前版本。无 .git 时回退到 dev。
VERSION ?= $(shell tag=$$(git describe --tags --exact-match --match 'v[0-9]*' --dirty 2>/dev/null); if [ -n "$$tag" ]; then printf '%s' "$$tag"; else commit=$$(git rev-parse --short HEAD 2>/dev/null); if [ -n "$$commit" ]; then printf 'dev-%s' "$$commit"; git diff-index --quiet HEAD -- 2>/dev/null || printf '%s' '-dirty'; else printf 'dev'; fi; fi)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)

LDFLAGS_VERSION := $(if $(VERSION),-X main.version=$(VERSION)) \
                   $(if $(COMMIT),-X main.commit=$(COMMIT)) \
                   $(if $(DATE),-X main.date=$(DATE))

# 构建目标（默认 release）
build:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS_RELEASE) $(LDFLAGS_VERSION)" -o dist/$(BIN) .

# 纯 API 构建：-tags no_webui 令二进制不嵌入 Web UI 静态资源、也不注册其路由
# （`/`、`/api.html`、`/static/*` 一律 404），只保留 REST/MCP/WS 与 /api.md、
# /skills.md 两份文档。产物带 api 后缀，可与完整版并存于 dist/。
build-api:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -tags no_webui -trimpath -ldflags "$(LDFLAGS_RELEASE) $(LDFLAGS_VERSION)" -o dist/$(HBIN) .

# 调试模式构建（保留符号信息，便于 delve 调试）
build-debug:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -gcflags "all=-N -l" -ldflags "$(LDFLAGS_VERSION)" -o dist/$(BIN) .

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
