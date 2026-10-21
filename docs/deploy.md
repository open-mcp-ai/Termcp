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

不需要 Web UI 时，`-tags no_webui` 产出的是只含 REST、MCP、WebSocket 与 agent 文档（`/api.md`）的二进制：它既不嵌入也不服务 Web UI（`/`、`/api.html`、`/static/*` 一律 404），可与完整版并存。

```bash
make build-api    # dist/termcp-api-<os>-<arch>
```

## 零信任远程实例（sidecar 注入）

适用场景：termcp 跑在一台**不可信**的远程主机/容器里供 Agent 调用，凭据不离开本地可信端（设计：`docs/design/credential-vault.md`）。远程实例不建 vault、不落凭据；本地 sidecar 经已认证的 REST 注入**内存临时 profile**，会话结束或 TTL 到期即自动清除。

远程实例（零持久秘密）：

```bash
termcp --no-internal --host 0.0.0.0 --port 18765 --auth-token <长期随机串>
```

本地 sidecar（持 vault）：

```bash
# 1. 解锁本地 vault（keychain 用户免密）
termcp vault unlock   # 或启动本地实例由 keychain 自动解锁

# 2. 经本地实例取出目标 profile 的明文 TOML（本地 vault 解锁状态下，
#    GET 单条配置返回解密后的原文），注入远程实例为临时 profile（TTL 10 分钟）
#    明文只存在于 TLS 通道与远程实例内存
curl -s 'http://127.0.0.1:18765/api/connections/prod' | \
  curl -X POST 'https://remote:18765/api/connections/batch?temporary=true&ttl_seconds=600' \
       -H 'Authorization: Bearer <远程 token>' --data-binary @-

# 3. 引用注入的名字开会话（首个会话绑定该 profile；会话 DEAD 即清除）
curl -X POST 'https://remote:18765/api/sessions' \
     -H 'Authorization: Bearer <远程 token>' \
     -H 'Content-Type: application/json' \
     -d '{"ssh_config":"prod"}'
```

两条清除路径互为冗余：会话 DEAD（经绑定）与 TTL（经懒清扫）。注入用的明文取自本地实例的单条配置 GET——它要求本地 vault 处于解锁态，且只出现在管道里。注入面建议用 scoped token 收窄：`termcp token create --tools profile.create --ttl 24h` 签出的 token 只能注入、不能读取或管理；人面 token 继续单独保管。

## 容器里的凭据 Vault

容器内没有 OS keychain，vault 自动落到密码路径（`identity.age` + scrypt）。以下每一步都按真实容器实测过：

```bash
# 建库（在运行中的容器里执行一次；密码经 stdin 读入，不进进程参数）
echo '<一个强密码>' | docker exec -i termcp termcp vault init

# 解锁（REST，等价于宿主机上的 termcp vault unlock）
curl -X POST http://localhost:18765/api/vault/unlock \
  -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' \
  -d '{"passphrase":"<同一个密码>"}'
```

三个运维事实，均与设计一致而非缺陷：

- **重启后 vault 必然回锁**——容器没有 keychain 可记忆，重启即回到锁定态，需重新 `unlock`。锁定期间消耗凭据的操作一律 `409`，profile 列表仍可读。
- **卷里只有密文**——`~/.termcp/vault/` 下 `identity.age`（scrypt 加密）与 `ssh_configs/*.age`（age 密文），凭据明文任何时刻不落卷。备份整个卷不会泄露凭据，但**丢了密码或 identity 文件就是丢了库**——两处都备份。
- 想完全关掉 keychain 探测（纯容器 fleet），加 `-e TERMCP_VAULT_NO_KEYCHAIN=1`，行为不变、日志更静。

## 多端同步（recipients）

vault 的多端同步是"密文 + 公钥表"模型：同步内容天生可以走任何不可信通道（rsync、git、网盘），因为里面没有可读的东西。每台设备有自己的 identity（私钥不出设备），密文按 recipients 表封送给全部已授权设备。

```bash
# 新设备 B：建库，拿到它的公钥（init 输出里）
termcp vault init --data-dir <B的数据目录>     # 输出 "this device's recipient: age1..."

# 既有设备 A：把 B 加进 recipients，重封存量密文
termcp vault recipients --data-dir <A的数据目录> add laptop age1...
termcp vault reseal --data-dir <A的数据目录>

# 同步（只带这两样；identity.age 永远不在其列）
rsync -a <A>/vault/recipients.toml <A>/vault/ssh_configs/ <B>/vault/
```

之后 B 用自己的密码/keychain 解锁即可解密全部 profile。**撤销**一台设备：`recipients remove <名字>` + `reseal`——此后新密文它解不开（它手里已有的旧密文副本无法追溯收回，这是向前撤销语义）。 relay 协议（自动同步 + 冲突合并）是独立后续项，手动路径即上文。
