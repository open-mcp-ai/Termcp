# 命令行参考

```text
termcp [flags]
termcp <subcommand> [action] [flags]
```

子命令写在最前，flag 在其后；daemon 的动作紧跟其子命令（`termcp daemon start --port 9000`），`stdio` 形式把可选的端点值也放在那里（`termcp stdio sse`、`termcp daemon stdio sse`）。动作写错位会被指回正确拼写（`termcp daemon start stdio` 提示 `termcp daemon stdio`）。

## Flags

| Flag | 默认 | 说明 |
| --- | --- | --- |
| `--host` | `127.0.0.1` | HTTP 绑定地址。`0.0.0.0` 监听所有网卡。绑定非 loopback **必须**配认证 token/hash，否则启动失败。 |
| `--port` | `18765` | HTTP 端口。Web UI、MCP SSE、MCP streamable HTTP 与文档端点（`/api.md`）共用。 |
| `--data-dir` | `~/.termcp` | 持久化目录（会话、SSH 配置、转录）。自动创建。可用 `$TERMCP_DATA_DIR` 覆盖。 |
| `--log-level` | `info` | 日志级别：`debug` / `info` / `warn` / `error`。`debug` 打印所有 MCP 工具调用；失败的工具调用与会话创建错误无论级别都记在 `warn`/`error`。 |
| `--no-internal` | `false` | 禁用内置的 loopback SSH profile。 |
| `--assets` | `~/.termcp/assets` | Web UI 与文档的外置静态资源目录。目录里存在的文件覆盖内嵌副本，不存在的回落到内嵌；目录不存在等于什么都没发生。可用 `$TERMCP_ASSETS_DIR` 覆盖。覆盖是**按文件**的：`app.css` 现在是 manifest，`@import` 掉 `static/css/` 下的分块，所以通过覆盖目录换肤意味着覆盖整个 `static/css` 目录——一个只导入内嵌分块的 manifest 会让未被覆盖的部分恢复成内嵌外观。 |
| `--mcp-manage-ssh-configs` | `false` | 允许 MCP 工具创建/编辑/删除 SSH 配置（凭据永不暴露）。 |
| `--auth-token` | *(未设)* | HTTP 认证的静态 token（或 `$TERMCP_AUTH_TOKEN`）。每个客户端——API、MCP、浏览器——都要出示。与 `--auth-hash` 互斥。 |
| `--auth-hash` | *(未设)* | token 的加盐 SHA-256（`sha256-<salt_hex>-<digest_hex>`），服务端不保存明文（或 `$TERMCP_AUTH_HASH`）。用 `termcp --gen-auth-hash` 生成。哈希配置的实例也接受哈希串本身作为凭据——`termcp daemon` 管理与 `termcp stdio` 只拿到哈希也能用——因此把哈希当机密对待。与 `--auth-token` 互斥。 |
| `--disable-auth` | `false` | **刻意**关掉 HTTP 认证，包括非 loopback 绑定（或 `$TERMCP_DISABLE_AUTH_TOKEN=1`）。请配合 loopback 端口，只让本机调用者可达。与 `--auth-token`/`--auth-hash` 同时给出是错误，而不是某个参数静默胜出。 |
| `--idle-timeout` | `30s / 不限` | 守护实例在无任何连接或请求时可自行退出前的时长，如 `10m`；`0` 关闭。默认：`termcp daemon stdio` 拉起的实例 30s，`termcp daemon start` 拉起的实例不限时。 |
| `--namespace` | 空（实例所有者） | 仅 stdio 桥：声明调用方命名空间（字母/数字/`-_.`，最长 64），桥发的每个请求都带 `X-Termcp-Namespace`。实例侧于是只列出/接受这个命名空间里的会话，多个 bridge 接到同一实例时互不干扰。空 = 今天的全局视图。名字非法在启动时就被拒，不会留到第一条请求才失败。 |
| `--gen-auth-hash` | *(动作)* | 生成 `--auth-hash` 用的加盐 SHA-256 哈希后退出（token 从参数取，或在终端下无回显地从 stdin 读）。 |
| `--version` | *(动作)* | 打印版本、commit 与构建日期后退出。`make build`/release 构建经 `-ldflags` 注入版本（HEAD 正好带 tag 时为该 tag，否则 `dev-<commit>`，脏工作区再缀 `-dirty`）；裸 `go build` 或 `go install module@vX.Y.Z` 回落到 Go 工具链嵌入的模块版本（release tag，或检出的伪版本）。`dev` 表示二进制里没有任何版本信息——`go run`、`-buildvcs=false`、解包出来的 tarball——不代表源码未打 tag。 |

## Web UI 主题

在 Web UI 右上角的主题菜单中选择 `default-light`、`default-dark` 或
`cyberpunk`。切换即时生效，终端连接和输出保留；选择保存在浏览器中，文档页
使用同一选择。默认是**自动**：跟随系统配色（`prefers-color-scheme`）在浅色与
深色主题之间切换，系统配色变化时页面即时跟随，无需刷新；没有存过任何选择的
浏览器首次打开就走这条路，所以深色系统的用户不会再先看到一页浅色。想固定
下来就选具体主题，此后系统配色不再影响它。`cyberpunk` 保留原 dev 外观。

自定义主题放在 `~/.termcp/themes/<主题名>/`，该目录不随 `--data-dir` 改变。
每个主题必须有 `theme.css`，可以携带 `assets/` 下的 CSS、图片、字体等文件：

```text
~/.termcp/themes/soft-blue/
  theme.css
  theme.json                # 可选：终端字体、配色及窗口样式
  strings.json              # 可选：替换界面文案
  assets/
    static/css/tokens.css
    icons/custom.svg
```

一个最小的 `theme.css` 示例：

```css
@import "./assets/static/css/app.css";

:root {
  --accent: #627eac;
  --accent-hover: #536d97;
  --accent-ring: rgba(98, 126, 172, 0.18);
}
```

缺少的资源按文件回落到内嵌副本。重新打开主题菜单即可发现新增目录，无需
编辑索引、重启服务或增加接口；修改当前主题后再次选择它即可应用。

主题可以通过 `strings.json` 替换界面文案，支持 `en`、`zh-Hans`、`zh-Hant`。
例如，将连接列表标题替换为“我的主机”：

```json
{
  "zh-Hans": {
    "nav.nethub": "我的主机",
    "nethub.add": "添加主机"
  }
}
```

使用 `internal/webui/assets/static/js/i18n-catalog.js` 中已有的键，并保留
`{count}` 等占位符。未覆盖的文案使用当前语言的默认内容；切换主题会同时
更新文案和样式，切换语言后仍使用主题的对应翻译。

主题的 `theme.json` 可以配置终端外观。例如：

```json
{
  "terminal": {
    "fontFamily": "JetBrains Mono, var(--font-mono)",
    "fontSize": 14,
    "lineHeight": 1.2,
    "letterSpacing": 0,
    "background": "#f7f8fa",
    "foreground": "#292e36",
    "cursor": "#4470b2",
    "selectionBackground": "rgba(68, 112, 178, 0.22)",
    "transparent": false,
    "opacity": 1
  }
}
```

默认浅色、深色主题的终端均不透明。需要半透明背景时设置
`"transparent": true`，并设置 `opacity`（如 `0.85`）；透明度只作用于背景，
文字保持不透明。也可通过 `colors` 设置 16 色 ANSI 配色，通过 `window`
设置窗口背景、文字、标题栏、边框和模糊效果。字体可使用主题携带的字体文件，
在 CSS 中用 `@font-face` 声明即可。

修改后重新选择主题即可即时应用，字号、字体及行距变化会重新计算终端网格，
保留连接和历史。省略或无效的配置使用该主题的 CSS 默认值。完整字段说明见
[`design/themes.md`](./design/themes.md#terminal-settings)。

`--assets` / `$TERMCP_ASSETS_DIR` 的原有覆盖仍然有效，并优先于主题自己的
资源。例如外置 `static/css/tokens.css` 会覆盖所有主题对应的文件。也可以在
外置资源目录的 `themes/<主题名>/` 下提供主题包。主题实现与优先级详见
[`design/themes.md`](./design/themes.md)。

## 子命令

| 命令 | 作用 |
| --- | --- |
| `stdio` | 用 stdin/stdout 提供 MCP，把每条消息转发到 `--host`/`--port` 上已在应答的 HTTP MCP 端点——一个独立桥（它从不自己启动实例）。可选端点值跟在 `stdio` 后面：`sse`/`/sse` 选 SSE 传输，`stream`/`/stream` 选 streamable（默认），完整 `http(s)://` URL 指向任意 MCP HTTP 端点（路径以 `/sse` 结尾 = SSE）。`--namespace <name>` 让这个桥成为一个独立调用方（见「权限边界」）。 |
| `daemon` | 管理在 `--host`/`--port` 应答的实例；不带动作时列出动作，`termcp daemon --help` 展开完整参数与细节。 |
| `daemon start` | 确保后台实例在跑：复用端点上应答的东西（手动起的实例也算），否则拉起一个分离实例并等它就绪。它运行到被停止——除非传 `--idle-timeout`，否则没有空闲倒计时。 |
| `daemon stdio` | 与 `start` 相同，然后留在前台当该实例的 stdio 桥（可选端点值紧跟动作：`termcp daemon stdio sse`）。它拉起的实例在没有桥连接时开始倒计时。 |
| `daemon stop` | 请求后台实例优雅停机；只有守护实例接受——手动实例会被指回它被启动的地方。 |
| `daemon status` | 汇报实例的 pid / URL / 版本 / 日志路径。它是被动查询，它自己的请求不计入空闲倒计时，所以询问不会让它汇报的实例活着。 |
| `config-storage status` | 只读检查 `--data-dir` 下的主机配置存储；已加密时在 ENV/系统凭据可解锁的前提下报告 catalog revision 与密文 profile 数。 |

每个 daemon 动作都经 HTTP 找它的实例，**从不查进程表**——实例在哪里监听就能在哪里找到。

## 主机配置加密

新目录默认使用明文 `config.toml`。WebUI 页头的“配置加密”入口可以主动启用
“新建配置加密”、选择具体主机加密，或把所选加密主机还原为明文。启用不会自动
转换任何已有配置，也不会改变未选择主机的存储方式。会话正文和历史仍使用现有格式，
不因这个开关加密。

```bash
# 容器/非交互环境：先用一个显式 32 字节 KEK，再在 WebUI 中启用
export TERMCP_CONFIG_KEK="$(openssl rand -base64 32)"
termcp config-storage status --data-dir ~/.termcp
```

没有设置 `TERMCP_CONFIG_KEK` 时，迁移使用操作系统凭据存储：macOS Keychain、
Windows DPAPI 或 Linux Secret Service。命令不读取 argv 中的密码，也不把 Main
Key、DEK 或明文配置写到日志。`status` 看到 `encrypted (locked)` 表示文件名与
catalog 存在，但当前调用方没有可解锁的 Provider；它不会生成替代密钥或退回明文。
页面的勾选操作是显式授权：加密所选主机时，只有该主机的明文 `config.toml` 会在
密文 catalog 提交并被回读验证后删除；还原时先原子写入明文，再移除密文引用。

`encryption-test` 分支的 `keys/master.key`、`config/hosts.enc` 不是兼容格式；
迁移会拒绝该布局。保留旧目录，并在隔离副本中先用对应版本导出，再导入新实例。

## 权限边界

这些 flag 是你的**能力闸门**：`--no-internal` 把 Agent 限制在远程主机，`--mcp-manage-ssh-configs` 打开 SSH 配置写权限。按场景收紧或放宽 Agent 能碰的东西。认证见 [`clients.md`](./clients.md#认证)。

## 示例

```bash
# 监听所有网卡
./termcp --host 0.0.0.0 --auth-token "your-long-random-token"

# 监听所有网卡，服务端只保存加盐哈希
./termcp --host 0.0.0.0 --auth-hash "$(./termcp --gen-auth-hash)"

# 允许 AI Agent 管理 SSH 配置
./termcp --mcp-manage-ssh-configs

# 禁用内置 loopback profile（Agent 只能连远程主机）
./termcp --no-internal
```
