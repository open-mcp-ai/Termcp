# 部署

Termcp 是纯 Go、无 CGO 的单二进制，可交叉编译到任意平台（Windows 用 ConPTY，macOS / Linux 用 POSIX PTY，行为一致）。这里收录容器化与自建部署方式；安装方式见 README 的「30 秒上手」，全部 flag 见 [`cli.md`](./cli.md)。

## 运行官方镜像

registry 镜像以非 root 用户 `termcp`（uid/gid 1000）运行，并把 `/home/termcp` 声明为 `VOLUME`——所有状态（会话、SSH 配置、转录）默认落在 `~/.termcp`。镜像里只有二进制：没有内置 entrypoint、没有 EXPOSE，绑定地址由运行命令决定。

```bash
docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765
```

> Shell 示例刻意写成单行：`\` 续行在 bash 里合法，在 PowerShell 里是语法错误，所以每条命令都能原样粘进 bash、zsh 和 PowerShell。

`--host 0.0.0.0` 可从容器外到达，因此必须带认证 token。MCP 端点：`http://localhost:18765/stream`。改用 bind mount 而不是具名卷时，先把宿主机目录 chown 掉：`chown -R 1000:1000 /path/on/host`。

### 不启用 Token 的 Docker 运行方式（仅限本机）

一次性演示、录屏或单用户工作站上，token 是纯粹的摩擦。把端口只发布到**宿主机 loopback**，并明确告诉 Termcp 缺凭据是刻意的：

```bash
docker run -d --name termcp -p 127.0.0.1:18765:18765 -v termcp-data:/home/termcp ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765 --disable-auth
```

两个细节让它安全而不只是方便。`-p 127.0.0.1:18765:18765` 把发布的端口绑到宿主机 loopback，容器对本机可达而对 LAN 不可见——容器自己仍然监听 `0.0.0.0`，因为那是它所在网络命名空间之外唯一可路由的地址。而 `--disable-auth` 之所以必需，正是因为 Termcp 拒绝在非 loopback 绑定上无认证启动：这个 flag 是运维在承担责任，所以它也把启动日志降级为警告。等价的环境变量形式是用 `-e TERMCP_DISABLE_AUTH_TOKEN=1` 代替该 flag。

## 多阶段构建：添加到任意容器

把这份 `Dockerfile` 放进你的应用项目：构建阶段用 `go install` 装好 Termcp，再由 `COPY --from` 把二进制拷进目标镜像——那里不需要 Go 运行时。

```dockerfile
# syntax=docker/dockerfile:1

ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS termcp-build

# 模块代理；海外环境可换 https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} GOBIN=/out CGO_ENABLED=0

# 生产环境建议固定版本，如 @vX.Y.Z
RUN go install github.com/open-mcp-ai/termcp@latest

# 任意目标基础镜像
FROM alpine
COPY --from=termcp-build /out/termcp /usr/local/bin/termcp
```

需要另一个模块代理或基础镜像镜像源时，用 `--build-arg` 换掉 `GOPROXY` 或 `GO_IMAGE`。

## 启动命令示例

容器必须绑定 `0.0.0.0`，而非 loopback 绑定**要求认证**（`TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`），否则启动失败。

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t my-app-with-termcp .
docker run -d --name my-app-termcp -p 18765:18765 -v termcp-data:/data -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret --entrypoint /usr/local/bin/termcp my-app-with-termcp --host 0.0.0.0 --port 18765 --data-dir /data
docker logs -f my-app-termcp
```

追加以 `--mcp-manage-ssh-configs` 打开 Agent 的 SSH 配置写工具。

如果 Termcp 必须与另一个主进程共处一个容器，就从现有 entrypoint 或进程管理器启动它；否则把它作为独立服务运行，并在 `http://termcp:18765/stream` 访问。

## Docker Compose 启动

```yaml
services:
  termcp:
    build: .
    entrypoint: ["/usr/local/bin/termcp"]
    command: ["--host", "0.0.0.0", "--port", "18765", "--data-dir", "/data"]
    environment:
      - TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret
    ports:
      - "18765:18765"
    volumes:
      - termcp-data:/data

volumes:
  termcp-data:
```

```bash
docker compose up -d --build
```

## 纯 API 构建

不需要 Web UI 时，`-tags no_webui` 产出的是只含 REST、MCP、WebSocket 与两份 agent 文档（`/api.md`、`/skills.md`）的二进制：它既不嵌入也不服务 Web UI（`/`、`/api.html`、`/static/*` 一律 404），可与完整版并存。

```bash
make build-api    # dist/termcp-api-<os>-<arch>
```
