# termcp HTTP API

Base URL: `http://localhost:18765`

termcp 是一个终端会话平台，同一端口向**三种使用者**开放同一批会话：

| 入口 | 路径 | 使用者 |
|------|------|--------|
| Web UI | `/` + `/api.html` | 人（浏览器） |
| MCP | `/sse` + `/message` 或 `/stream` | AI Agent |
| REST + WebSocket | `/api/*` + `/api/ui/ws` | 脚本 / 程序 |

本文档描述 REST 面（脚本编程）。实时终端 I/O 走 WebSocket (`/api/ui/ws`)，其余操作走 REST。

## 0. MCP 传输（AI 对接）

与 REST 共用同一 HTTP 端口、同一会话内核；MCP 是平台面向 AI Agent 的接入面。按客户端能力二选一：

| 传输 | 路径 | 说明 |
|------|------|------|
| SSE | `GET /sse` + `POST /message` | 客户端配置 `/sse`；JSON-RPC 走 `/message` |
| Streamable HTTP | `/stream` | 单端点；Open WebUI 等使用 |

浏览器：`/api.html`（Web UI **API / MCP**）提供按 origin 复制的 MCP 配置和 HTTP API 速查表。工具参数见 [`mcp-tools.md`](./mcp-tools.md)。

## Authentication（可选，默认关闭）

启用 `--auth-token` / `--auth-hash`（或 `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`）后，**全部** HTTP 面都要求凭据：Web UI、REST、MCP SSE、`/stream`、WebSocket，由共享 mux 外层的 `internal/auth` 中间件统一校验。缺少或错误的凭据返回 `401 Unauthorized`，并携带 `WWW-Authenticate: Basic`（浏览器据此弹出原生登录框）。

| 客户端类型 | 凭据方式 |
|-----------|---------|
| REST / MCP / curl | `Authorization: Bearer <token>` |
| 浏览器（Web UI） | 原生 Basic 弹框——用户名被忽略、**密码填 token** |
| 浏览器 WebSocket (`/api/ui/ws`) | 同源自动携带认证成功后下发的 `termcp_token` cookie |

```bash
# 生成 salted SHA-256 哈希（终端下无回显，不进 shell 历史）
termcp --gen-auth-hash

# REST 请求两种等价写法
curl -u :<token> http://127.0.0.1:18765/api/sessions
curl -H "Authorization: Bearer <token>" http://127.0.0.1:18765/api/sessions
```

约束：

- `--auth-token` 与 `--auth-hash` 互斥；同一配置项 flag 优先于环境变量。
- 绑定非 loopback 地址（`0.0.0.0`、局域网 IP 等）时未配置认证会**拒绝启动**。
- Token 不会写入日志，也不应放入 URL（query string）——请放在请求头。

---

## 1. 连接配置 (Connection Profiles)

### `GET /api/connection-templates`

返回新建连接时的 TOML 模板。

```
Response 200:
{
  "remote":   "# Remote SSH connection\n...",
  "internal": "# Internal loopback\n..."
}
```

### `GET /api/connections`

列出所有连接配置的摘要。

```
Response 200:
{
  "connections": [
    { "name": "pi", "kind": "remote", "host": "192.168.1.100", "user": "pi", "port": 22 }
  ]
}
```

### `GET /api/connections/{name}`

获取单个连接配置的原始 TOML。

### `PUT /api/connections/{name}`

创建或更新连接配置。Body 为 TOML。

```
Response: 204 No Content
```

### `DELETE /api/connections/{name}`

删除连接配置。

```
Response: 204 No Content
```

### `POST /api/connections/test`

Web UI 新建连接对话框的「测试连接」按钮后端：验证连接配置的连通性（完整拨号链路：SOCKS5 代理 → bastion 跳板 → 目标主机，并验证可开 exec 通道），**测试成功后不保留连接**。Body 为 TOML（与 `PUT` 相同）。`internal` 类 profile 不做拨号，直接返回 `{ "ok": true }`。

```
Request: TOML body

Response 200:
{ "ok": true, "duration_ms": 123 }
{ "ok": false, "duration_ms": 123, "error": "<失败原因 + 诊断提示>" }
```

---

## 2. Session

**概念：** Session = SSH 连接容器，包含 0~N 个 Shell + 0~N 个 Forward。Shell ID 与 Session ID 分离，第一个 Shell 也有独立 ID。

### `GET /api/sessions`

列出所有活跃 session。

```
Response 200:
{
  "sessions": [
    { "id": "abc123", "name": "pi", "mode": "pty", "status": "running",
      "pid": 12345, "rows": 24, "cols": 80, "ssh_endpoint": "remote", "created_at": "..." }
  ]
}
```

### `POST /api/sessions`

创建新 SSH 连接和 Session（含第一个 Shell）。

```
Request:
{
  "ssh_config": "pi",    // 连接配置名，默认 "internal"（本机 loopback）；MCP 的 session_start 则必填
  "command": "",         // 命令，空 = 登录 shell
  "args": [],
  "mode": "pty",         // "pty" | "pipe"
  "name": "my-session",  // 显示名称，默认 = ssh_config
  "rows": 24,
  "cols": 80
}

Response 200:
{ "session_id": "abc123", "shell_id": "def456", "pid": 12345, "ssh_config": "pi" }
```

### `GET /api/sessions/{id}`

获取单个 session 详情。

```
Response 200: Session 对象（同列表中的元素）
```

### `DELETE /api/sessions/{id}`

**永久删除**：断开 Session（关闭所有 Shell → Forward → SSH 连接），并同时移除其归档记录与落盘消息历史。此操作不可恢复。

```
Response: 204 No Content
```

> 注意：这与 `POST /api/sessions/{id}/terminate`（仅停止并归档、保留历史）不同。

### `PATCH /api/sessions/{id}`

重命名 session（活跃或已归档均可）。

```
Request:
{ "name": "my-ctf-box" }

Response 200: Session 对象
```

---

## 2.5 归档会话历史（Dead Sessions）

会话正常退出、意外断线或服务器关闭后，会保留为**归档会话**，元数据写入 `history.json`、消息保留在 `data/messages/{id}/`，跨 termcp 重启仍然可见。只有 `DELETE`（主动删除）才会真正清空历史。

### `GET /api/history`

列出所有归档（dead）会话。

```
Response 200:
{
  "sessions": [
    { "id": "abc123", "name": "CTF-1", "status": "archived", "reason": "crash",
      "notes": "…", "tags": ["web"], "created_at": "..." }
  ]
}
```

### `GET /api/history/{id}`

获取单个归档会话详情。

```
Response 200: ArchivedSession 对象
```

### `PATCH /api/history/{id}`

更新归档会话的备注/标签/名称。字段缺省则保持不变；传空字符串/空数组清空。

```
Request:
{ "name": "new-name", "notes": "解法要点", "tags": ["web","flag"] }

Response 200: ArchivedSession 对象
```

### `DELETE /api/history/{id}`

**永久删除**归档会话，并清空其 `data/messages/{id}/` 消息目录。不可恢复。

```
Response: 204 No Content
```

### `GET /api/history/search?q=...&limit=...`

在全部归档会话的消息中进行全文搜索（子串、不区分大小写）。

```
Response 200:
{
  "hits": [
    { "session_id": "abc123", "name": "CTF-1", "type": "output", "snippet": "…keyword…" }
  ]
}
```

### `GET /api/history/{id}/transcript?format=text|markdown|html`

导出交错 input/output 时间线（去掉 ANSI）。默认 `text`。

```
Response 200:
$ ls -la
file.txt
...
```

### `GET /api/history/{id}/screenshot?start=0&lines=40&cols=80&theme=dark`

渲染指定行范围的会话截图，返回 PNG。`start` 起始显示行（0 基）、`lines` 渲染行数（0=全部）、`cols` 终端列宽（默认 80）、`theme` 为 `dark`(默认)/`light`。

```
Response 200: image/png（Content-Disposition 附加下载）
```

---

## 3. Shell

Shell 是 Session 下的子资源，ID 全局唯一。

### `GET /api/sessions/{id}/shells`

列出 Session 的所有 Shell。

- `running` Session 只列出**现存**的 channel：活跃 shell + 自然退出但保留的 shell（status `exited`，供读取末尾输出）。**手动关闭的 shell 已删除，不会出现在列表中**，也不会以死态 tab 形式复活。
- `exited`（DEAD）Session 返回保留的 shell 快照（仅自然退出/异常断线的 shell）。

```
Response 200:
{
  "shells": [
    { "id": "abc123", "name": "pi", "status": "running", ... },
    { "id": "def456", "name": "shell-2", "status": "running", ... }
  ]
}
```

### `POST /api/sessions/{id}/shells`

在已有 Session 上创建新 Shell channel（复用 SSH 连接）。

```
Request:
{ "command": "", "name": "shell-2", "mode": "pty", "rows": 24, "cols": 80 }

Response 200:
{ "shell_id": "def456", "session_id": "abc123", "name": "shell-2" }
```

### `DELETE /api/shells/{id}`

**删除**指定 Shell channel（参数为 **shell_id**）。手动关闭是删除而非 DEAD：shell 从活跃列表、保留快照和 sessions.json 中一并移除，不会留下 `end`/死态 tab。不中断 SSH 连接，不影响同 Session 的其他 Shell。internal 主 shell 关闭为 no-op（进程可存活于 tab 之外）。

Pipe 会话的最后一个 shell 被关闭时，容器转为 `exited`（DEAD，保留只读）；PTY 容器保持 `running`，可再新建 shell。

已不存在的 shell 也返回 204（幂等）。

```
Response: 204 No Content
```

---

## 4. 终端 I/O

### WebSocket `GET /api/ui/ws`

双向实时通道。

**Client → Server：**

| type | 字段 | 说明 |
|------|------|------|
| `watch_add` | `id` | 订阅终端输出 |
| `watch_remove` | `id` | 取消订阅 |
| `input` | `id`, `d`(base64), `nl` | 发送键盘输入 |
| `resize` | `id`, `rows`, `cols` | PTY 尺寸变更 |

**Server → Client：**

| type | 字段 | 说明 |
|------|------|------|
| `sessions` | `sessions` | Session 列表（连接时 + 变更时） |
| `terminal` | `id`, `d`(base64) | 终端输出块 |
| `terminal_done` | `id` | Shell 已退出 |

> 终端 I/O 的 `id` 使用 Shell ID；Session ID 只用于连接级 REST 资源。

### `GET /api/shells/{id}/output-range`

读取 shell 保留输出片段（**path id 是 shell_id**）。

兼容：`GET /api/sessions/{id}/output-range` 仍可用。
当 id 是 shell_id 时直接命中；当 id 是 session_id 时回退到该 session 的 primary shell。

```
Query:
  start=0          起始字节（默认 0；与 tail=1 互斥）
  max=262144       最大返回字节（硬上限 512KiB）
  tail=1           从末尾取 max 字节（忽略 start）

Response 200:
{
  "start": 0,
  "end": 1024,
  "total": 4096,
  "d": "<base64>"
}
```

---

## 5. 端口转发 (Port Forward)

### `GET /api/forwards`

列出所有活跃端口转发。

```
Response 200:
{ "forwards": [{ "forward_id": "L-abc123", "session_id": "abc123", "direction": "local", ... }] }
```

### `GET /api/sessions/{id}/forwards`

列出指定 Session 的端口转发。

### `POST /api/sessions/{id}/forwards`

在指定 Session 上创建端口转发。session_id 来自 URL，无需传 `ssh_config`。

```
Request:
{
  "direction": "local",      // "local" | "remote" | "dynamic"
  "remote_host": "localhost",
  "remote_port": 80,
  "local_host": "0.0.0.0",   // remote 模式必填
  "local_port": 8080          // 0 = 自动分配
}
```

| direction | 必填参数 |
|-----------|---------|
| `local` | `remote_host`, `remote_port` (1-65535) |
| `remote` | `local_host`, `local_port`, `remote_host`, `remote_port` (1-65535) |
| `dynamic` | 无必填 |

```
Response 201: ForwardInfo
```

### `DELETE /api/forwards/{id}`

关闭指定端口转发。

```
Response 200: { "ok": true }
```

---

## 6. 文件操作

所有文件操作通过 Session 的 SFTP 通道（remote）或本地文件系统（internal）。

### `GET /api/sessions/{id}/files`

列出目录内容或获取文件信息。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 路径（必填） |

```
Response 200:
{ "name": "home", "size": 4096, "is_dir": true,
  "children": [{ "name": "file.txt", "size": 1024, "is_dir": false, "mod_time": "..." }] }
```

### `GET /api/sessions/{id}/files/download`

下载文件。支持 HTTP Range 头。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 文件路径 |

```
Response: application/octet-stream（支持 Range/206 Partial Content）
```

### `POST /api/sessions/{id}/files/upload`

上传文件。支持 multipart/form-data 或 raw body。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 目标路径 |
| `offset` | query | 写入起始偏移，默认 0 |
| Content-Range | header | 断点续传 |

```
Response 200: { "bytes_written": 1024 }
```

### `DELETE /api/sessions/{id}/files`

删除文件或空目录。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 路径 |

```
Response 200: { "ok": true }
```

### `PUT /api/sessions/{id}/files`

重命名/移动文件或目录（同文件系统内）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `from` | query | 源路径 |
| `to` | query | 目标路径 |

```
Response 200: { "ok": true }
```

### `POST /api/sessions/{id}/files/dir`

创建目录（含父目录）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 目录路径 |

```
Response 200: { "ok": true }
```

---

## 6.5 通知规则（shell_notify）

MCP `shell_notify` 注册的反向唤醒规则在这里查询与删除。Web UI 的 Notifications 标签页同源。

### `GET /api/notifications`

列出所有活跃通知规则。

| 参数 | 类型 | 说明 |
|------|------|------|
| `shell_id` | query | 可选，按 shell 过滤 |
| `session_id` | query | 可选，按会话过滤 |

```
Response 200:
{ "notifications": [{ "rule_id": "notif_...", "session_id": "...", "shell_id": "...",
                     "channel": "resource"|"sampling", "event": "output"|"exit"|"silence",
                     "created_at": "..." }] }
```

### `DELETE /api/notifications/{id}`

注销一条通知规则（`id` = `rule_id`）。

```
Response 200: { "ok": true, "rule_id": "notif_..." }
Response 404: { "error": "notification rule not found" }
```

---

## 7. 向后兼容路由

旧路由仍可用，委托到新路由。建议新代码使用上面的规范路径。

| 旧路径 | 规范路径 |
|--------|---------|
| `POST /api/sessions/start` | `POST /api/sessions` |
| `GET /api/sessions/{id}/child-shells` | `GET /api/sessions/{id}/shells` (301) |
| `POST /api/sessions/{id}/terminate` | `DELETE /api/sessions/{id}` |
| `POST /api/sessions/{id}/disconnect` | `DELETE /api/sessions/{id}` |

> 注意：遗留的 `terminate` / `disconnect` 仅**停止并归档**（保留历史），等价于归档语义，而非规范 `DELETE` 的永久删除。若需永久删除请用 `DELETE /api/sessions/{id}` 或 `DELETE /api/history/{id}`。

| `POST /api/sessions/{id}/close-shell` | close primary shell of session (`session_id` in path) |
| `POST /api/forwards` | `POST /api/sessions/{id}/forwards` |
| `DELETE /api/sessions/{id}/files/delete` | `DELETE /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/rename` | `PUT /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/mkdir` | `POST /api/sessions/{id}/files/dir` |
