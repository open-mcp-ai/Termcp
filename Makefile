.PHONY: build build-api build-debug test test-stress clean dist sync-assets prepare-release check-release

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

# Run the unit tests. Flags are byte-for-byte what .github/workflows/test.yml's
# Test step runs (change the two together); -shuffle=on reorders every run and
# prints its seed, so a failure replays with: go test -shuffle=<seed>.
# Why these flags and what the stress target is for: docs/agents/testing.md.
#
# -race needs cgo and a C toolchain (all three CI runners ship gcc; some Windows
# dev boxes do not). RACE=auto (default) probes the toolchain once per target
# and runs without it when missing; RACE=0 forces it off, RACE=1 forces it on
# (e.g. after `scoop install mingw`). The probe is a recursive variable, so
# targets that never expand it (build, dist, ...) pay nothing.
RACE ?= auto
race_probe = CGO_ENABLED=1 go test -race -count=1 -run '^$$' ./internal/clock/ >/dev/null 2>&1
race_flag = $(if $(filter 1,$(RACE)),-race,$(if $(filter 0,$(RACE)),,$(shell $(race_probe) && echo -race)))

# Shared degraded-mode notice for both test targets; $(1) is the target name.
define race_notice
@if [ "$(RACE)" = "auto" ] && [ -z "$(race_flag)" ]; then \
	echo "$(1): -race unavailable (needs cgo + a C compiler); running without it - CI still enables it"; \
fi
endef

test:
	$(call race_notice,make test)
	go test ./... -count=1 -shuffle=on $(race_flag) -timeout 240s

# Scheduling perturbation for the races -race cannot see (filesystem/lifecycle:
# -cpu=1 interleaves teardown with background writers the way a loaded CI runner
# does). Defaults to ~6 rounds of the whole tree; scope a single package with
#   make test-stress STRESS_PKGS=./internal/session/
STRESS_PKGS ?= ./...
STRESS_COUNT ?= 2
STRESS_CPU ?= 1,2,4

test-stress:
	$(call race_notice,make test-stress)
	go test $(STRESS_PKGS) -count=$(STRESS_COUNT) -cpu=$(STRESS_CPU) -shuffle=on $(race_flag) -timeout 900s

# 发布 PR：将 Unreleased 归档到指定版本，并同步 server.json。
prepare-release:
	python3 scripts/release_metadata.py prepare "$(RELEASE_VERSION)"

check-release:
	python3 scripts/release_metadata.py check $(if $(RELEASE_VERSION),--expected "$(RELEASE_VERSION)")

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
