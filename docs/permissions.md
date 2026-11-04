# Termcp 权限配置清单

> 由当前 `GET /auth/access` 与 `GET /auth/group-presets` 返回内容提取，对应 catalog version 3。权限界面中的未选择动作按禁止处理；本文不替代服务端校验。

## 权限效果

| 界面名称 | 存储值 | 含义 |
| --- | --- | --- |
| 禁止 | `deny`，或没有该 action | 请求直接拒绝。当前 UI 删除 action 条目来表示禁止。 |
| 允许 | `allow` | 满足主机范围和参数条件后直接执行。 |
| 需审批 | `ask` | 完整请求先入审批队列；批准时按当前凭据、主体、权限组、主机和参数重新校验。 |

## 主机范围

| 模式 | 存储值 | 含义 |
| --- | --- | --- |
| 全部主机 | `all` | 当前和以后新增的已保存主机；共享配置修改也要求此模式。 |
| 指定主机 | `selected` | 只允许 `hosts` 中列出的稳定主机身份。空列表不授予主机。 |
| 无主机 | `none` | 不授予任何主机资源。 |

风险级别只用于治理提示，不替代 allow/deny/ask 和参数约束。

## 总览

- 动作总数：86
- 风险分布：标准 51，敏感 9，高风险 26
- 权限族：会话 5，终端与通道 12，文件 18，端口转发 5，外部协议 25，传输任务 3，主机配置 6，扩展 8，公共别名 3，通知 1
- 全局动作：修改共享主机配置、扩展定义或公共别名的动作必须选择“全部主机”。

## 动作权限

### 会话（5）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出会话<br><small>List sessions</small> | `session.list` | 标准 | 允许的主机范围 | DSL: `ls` / `ps`<br>工具: `session_list`<br>WebUI: `/api/sessions GET` | — | 未提供说明 |
| 查看会话<br><small>Inspect sessions</small> | `session.info` | 标准 | 允许的主机范围 | DSL: `info` / `get`<br>工具: `session_info`<br>WebUI: `/api/sessions/{id} GET` | — | 未提供说明 |
| 建立连接<br><small>Start sessions</small> | `session.start` | 标准 | 允许的主机范围 | DSL: `connect`<br>工具: `session_start`<br>WebUI: `/api/sessions POST` | — | 未提供说明 |
| 停止会话<br><small>Stop sessions</small> | `session.terminate` | 标准 | 允许的主机范围 | DSL: `rm session`<br>工具: `session_terminate`<br>WebUI: `/api/sessions/{id} DELETE` | — | 未提供说明 |
| 删除会话<br><small>Delete sessions</small> | `session.delete` | 敏感 | 允许的主机范围 | DSL: `rm session --force`<br>工具: `session_delete`<br>WebUI: `/api/sessions/{id}/purge DELETE` | — | 删除已经结束的会话记录。 |

### 终端与通道（12）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出通道<br><small>List channels</small> | `shell.list` | 标准 | 允许的主机范围 | DSL: `ls`<br>工具: `shell_list` | — | 未提供说明 |
| 打开通道<br><small>Open channels</small> | `shell.open` | 标准 | 允许的主机范围 | DSL: `open`<br>工具: `shell_open`<br>WebUI: `/api/sessions/{id}/shells POST` | — | 未提供说明 |
| 关闭通道<br><small>Close channels</small> | `shell.close` | 标准 | 允许的主机范围 | DSL: `close`<br>工具: `shell_close` / `shell_archive`<br>WebUI: `/api/shells/{id} DELETE` | — | 未提供说明 |
| 指定启动程序<br><small>Start programs</small> | `shell.exec` | 标准 | 允许的主机范围 | DSL: `connect --argv` / `open --argv`<br>工具: `session_start` / `shell_open` | — | 在终端中显式启动程序或 argv。 |
| 调整终端大小<br><small>Resize channels</small> | `shell.resize` | 标准 | 允许的主机范围 | DSL: `resize`<br>工具: `shell_resize`<br>WebUI: `/api/shells/{id}/resize POST` / `/api/ui/ws resize` | — | 未提供说明 |
| 读取终端输出<br><small>Read output</small> | `shell.output` | 标准 | 允许的主机范围 | DSL: `read` / `run`<br>工具: `shell_output`<br>WebUI: `/api/shells/{id}/lines GET` | — | 未提供说明 |
| 发送终端输入<br><small>Type input</small> | `shell.input` | 标准 | 允许的主机范围 | DSL: `input` / `run`<br>工具: `shell_input`<br>WebUI: `/api/shells/{id}/input POST` / `/api/ui/ws input` | — | 向已打开终端输入文本；仍需目标系统账号权限。 |
| 发送按键<br><small>Press keys</small> | `shell.key` | 标准 | 允许的主机范围 | DSL: `key` / `run`<br>工具: `shell_key`<br>WebUI: `/api/shells/{id}/key POST` | 允许按键 (`keys`)<br>最大重复次数 (`repeat_max`) | 未提供说明 |
| 读取状态跨度<br><small>Read spans</small> | `shell.marks` | 标准 | 允许的主机范围 | DSL: `span`<br>工具: `message` | — | 未提供说明 |
| 检测 Termcp 本机环境<br><small>Detect local Termcp shell</small> | `shell.detect` | 标准 | 允许的主机范围 | 工具: `shell_detect` | — | 检测 Termcp 本机使用的 shell 环境，不检查远端主机。 |
| 注册 reader<br><small>Register readers</small> | `shell.reader.register` | 标准 | 允许的主机范围 | DSL: `reader`<br>工具: `shell_reader_register` | — | 未提供说明 |
| 注销 reader<br><small>Unregister readers</small> | `shell.reader.unregister` | 标准 | 允许的主机范围 | DSL: `reader --drop`<br>工具: `shell_reader_unregister` | — | 未提供说明 |

### 文件（18）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 读取文件<br><small>Read files</small> | `file.read` | 标准 | 允许的主机范围 | DSL: `read file` / `get file --op readlink` / `copy source`<br>工具: `file_read`<br>WebUI: `/api/files GET` | 文件形式 (`forms`)<br>最大字节数 (`max_bytes`) | 未提供说明 |
| 读取文件属性<br><small>Stat files</small> | `file.stat` | 标准 | 允许的主机范围 | DSL: `ls file` / `get file --op stat`<br>工具: `file_stat`<br>WebUI: `/api/files/stat GET` | — | 未提供说明 |
| 解析真实路径<br><small>Resolve real paths</small> | `file.realpath` | 标准 | 允许的主机范围 | DSL: `get file --op realpath`<br>工具: `file_fs` | — | 未提供说明 |
| 读取链接目标<br><small>Read links</small> | `file.readlink` | 标准 | 允许的主机范围 | DSL: `get file --op readlink`<br>工具: `file_link` | — | 未提供说明 |
| 读取工作目录<br><small>Read working directory</small> | `file.getwd` | 标准 | 允许的主机范围 | DSL: `get file --op getwd`<br>工具: `file_getwd` | — | 未提供说明 |
| 读取文件系统信息<br><small>Read filesystem stats</small> | `file.statvfs` | 标准 | 允许的主机范围 | DSL: `get file --op statvfs`<br>工具: `file_fs` | — | 未提供说明 |
| 获取传输地址<br><small>Get transfer URLs</small> | `file.urls` | 标准 | 允许的主机范围 | DSL: `get file --op urls`<br>工具: `file_urls`<br>WebUI: `/api/files/urls GET` | — | 未提供说明 |
| 写入文件<br><small>Write files</small> | `file.write` | 敏感 | 允许的主机范围 | DSL: `write file` / `copy destination`<br>工具: `file_write`<br>WebUI: `/api/files PUT` / `/api/files POST` | 文件形式 (`forms`)<br>最大字节数 (`max_bytes`) | 写入内联数据，或按明确字节上限从本地路径传输。 |
| 替换文件文本<br><small>Replace file text</small> | `file.replace` | 敏感 | 允许的主机范围 | DSL: `write file --old-text`<br>工具: `file_replace` | 文件形式 (`forms`) | 未提供说明 |
| 删除文件<br><small>Delete files</small> | `file.delete` | 高风险 | 允许的主机范围 | DSL: `rm file`<br>工具: `file_delete`<br>WebUI: `/api/files DELETE` | — | 删除目标账号命名空间内的文件或目录。 |
| 创建目录<br><small>Create directories</small> | `file.mkdir` | 标准 | 允许的主机范围 | DSL: `mkdir`<br>工具: `file_mkdir`<br>WebUI: `/api/files/mkdir POST` | — | 未提供说明 |
| 截断文件<br><small>Truncate files</small> | `file.truncate` | 高风险 | 允许的主机范围 | DSL: `write file --op truncate`<br>工具: `file_fs` | — | 截断或扩展远程文件，可能破坏已有内容。 |
| 修改文件权限<br><small>Change file modes</small> | `file.chmod` | 高风险 | 允许的主机范围 | DSL: `write file --mode`<br>工具: `file_perm` | — | 修改目标文件的权限位。 |
| 修改文件所有者<br><small>Change file owners</small> | `file.chown` | 高风险 | 允许的主机范围 | DSL: `write file --owner`<br>工具: `file_perm` | — | 修改目标文件的所有者或所属组。 |
| 修改文件时间<br><small>Change file times</small> | `file.chtimes` | 高风险 | 允许的主机范围 | DSL: `write file --time`<br>工具: `file_perm` | — | 修改目标文件的访问或修改时间。 |
| 创建符号链接<br><small>Create symbolic links</small> | `file.symlink` | 高风险 | 允许的主机范围 | DSL: `write file --op symlink`<br>工具: `file_link` | — | 在目标主机创建符号链接。 |
| 创建硬链接<br><small>Create hard links</small> | `file.link` | 高风险 | 允许的主机范围 | DSL: `write file --op link`<br>工具: `file_link` | — | 在目标主机创建硬链接。 |
| 重命名文件<br><small>Rename files</small> | `file.rename` | 敏感 | 允许的主机范围 | 工具: `file_rename`<br>WebUI: `/api/files/patch` | — | 未提供说明 |

### 端口转发（5）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出转发<br><small>List forwards</small> | `forward.list` | 标准 | 允许的主机范围 | DSL: `ls forward`<br>工具: `forward`<br>WebUI: `/api/forwards GET` | — | 未提供说明 |
| 创建本地转发<br><small>Open local forwards</small> | `forward.open.local` | 敏感 | 允许的主机范围 | DSL: `fwd --action local` / `open forward local`<br>工具: `forward`<br>WebUI: `/api/forwards POST` | — | 通过本机监听转发到远程目标。 |
| 创建远程转发<br><small>Open remote forwards</small> | `forward.open.remote` | 敏感 | 允许的主机范围 | DSL: `fwd --action remote` / `open forward remote`<br>工具: `forward`<br>WebUI: `/api/forwards POST` | — | 请求远程主机监听并转发流量。 |
| 创建动态代理<br><small>Open dynamic forwards</small> | `forward.open.dynamic` | 敏感 | 允许的主机范围 | DSL: `fwd --action dynamic` / `open forward dynamic`<br>工具: `forward`<br>WebUI: `/api/forwards POST` | — | 在目标连接上启动仅监听回环地址的 SOCKS 代理。 |
| 关闭转发<br><small>Close forwards</small> | `forward.close` | 标准 | 允许的主机范围 | DSL: `close forward` / `fwd --action close`<br>工具: `forward`<br>WebUI: `/api/forwards/{id} DELETE` | — | 关闭一个已存在的端口转发。 |

### 外部协议（25）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| MCP · 建立连接<br><small>Open MCP peers</small> | `mcp.open` | 高风险 | 允许的主机范围 | DSL: `open mcp` / `open session mcp` | — | 经现有会话连接远程 MCP 服务。 |
| MCP · 列出连接<br><small>List MCP peers</small> | `mcp.list` | 标准 | 允许的主机范围 | DSL: `ls mcp` | — | 未提供说明 |
| MCP · 查看连接<br><small>Inspect MCP peers</small> | `mcp.info` | 标准 | 允许的主机范围 | DSL: `get mcp` | — | 未提供说明 |
| MCP · 关闭连接<br><small>Close MCP peers</small> | `mcp.close` | 标准 | 允许的主机范围 | DSL: `rm mcp` / `close mcp` | — | 未提供说明 |
| MCP · 读取帧<br><small>Read MCP frames</small> | `mcp.read` | 标准 | 允许的主机范围 | DSL: `read mcp` / `recv mcp` | — | 未提供说明 |
| 列出 MCP 工具<br><small>List MCP tools</small> | `mcp.tools.list` | 标准 | 允许的主机范围 | DSL: `get mcp tools` / `ls mcp tools` | — | List the tools exposed by a connected remote MCP server. |
| 调用指定 MCP 工具<br><small>Invoke MCP tools</small> | `mcp.tool.invoke` | 高风险 | 允许的主机范围 | DSL: `call mcp <tool>` | 允许工具名 (`tool_names`) | 调用远程 MCP 服务上的指定工具。 |
| MCP · 发送自定义协议帧（广泛权限）<br><small>Send raw MCP frames</small> | `mcp.raw` | 高风险 | 允许的主机范围 | DSL: `call mcp --send` | — | 发送未分类的 MCP 帧，范围大于单个工具授权。 |
| CDP · 建立连接<br><small>Open CDP peers</small> | `cdp.open` | 高风险 | 允许的主机范围 | DSL: `open cdp` | — | 经现有会话连接 CDP 端点。 |
| CDP · 列出连接<br><small>List CDP peers</small> | `cdp.list` | 标准 | 允许的主机范围 | DSL: `ls cdp` | — | 未提供说明 |
| CDP · 查看连接<br><small>Inspect CDP peers</small> | `cdp.info` | 标准 | 允许的主机范围 | DSL: `get cdp` | — | 未提供说明 |
| CDP · 关闭连接<br><small>Close CDP peers</small> | `cdp.close` | 标准 | 允许的主机范围 | DSL: `close cdp` / `rm cdp` | — | 未提供说明 |
| CDP · 读取帧<br><small>Read CDP frames</small> | `cdp.read` | 标准 | 允许的主机范围 | DSL: `read cdp` / `recv cdp` | — | 未提供说明 |
| 读取 CDP 协议<br><small>Read CDP signatures</small> | `cdp.protocol.read` | 标准 | 允许的主机范围 | DSL: `get cdp protocol` | — | 未提供说明 |
| 附加浏览器目标<br><small>Attach CDP targets</small> | `cdp.attach` | 高风险 | 允许的主机范围 | DSL: `open cdp --target` | — | 附加一个浏览器目标。 |
| 分离浏览器目标<br><small>Detach CDP targets</small> | `cdp.detach` | 高风险 | 允许的主机范围 | DSL: `close cdp <target@` | — | 分离一个浏览器目标。 |
| 调用指定 CDP 方法<br><small>Invoke CDP methods</small> | `cdp.method.invoke` | 高风险 | 允许的主机范围 | DSL: `call cdp <method>` | 允许方法 (`methods`) | 调用一个指定的 CDP 方法。 |
| CDP · 发送自定义协议帧（广泛权限）<br><small>Send raw CDP frames</small> | `cdp.raw` | 高风险 | 允许的主机范围 | DSL: `call cdp --send` | — | 发送未分类的 CDP 帧。 |
| JRPC · 建立连接<br><small>Open JSON-RPC peers</small> | `jrpc.open` | 高风险 | 允许的主机范围 | DSL: `open jrpc` | — | 经现有会话连接 JSON-RPC 服务。 |
| JRPC · 列出连接<br><small>List JSON-RPC peers</small> | `jrpc.list` | 标准 | 允许的主机范围 | DSL: `ls jrpc` | — | 未提供说明 |
| JRPC · 查看连接<br><small>Inspect JSON-RPC peers</small> | `jrpc.info` | 标准 | 允许的主机范围 | DSL: `get jrpc` | — | 未提供说明 |
| JRPC · 关闭连接<br><small>Close JSON-RPC peers</small> | `jrpc.close` | 标准 | 允许的主机范围 | DSL: `close jrpc` / `rm jrpc` | — | 未提供说明 |
| JRPC · 读取帧<br><small>Read JSON-RPC frames</small> | `jrpc.read` | 标准 | 允许的主机范围 | DSL: `read jrpc` / `recv jrpc` | — | 未提供说明 |
| 调用指定 JSON-RPC 方法<br><small>Invoke JSON-RPC methods</small> | `jrpc.method.invoke` | 高风险 | 允许的主机范围 | DSL: `call jrpc <method>` | 允许方法 (`methods`) | 调用一个指定的 JSON-RPC 方法。 |
| JRPC · 发送自定义协议帧（广泛权限）<br><small>Send raw JSON-RPC frames</small> | `jrpc.raw` | 高风险 | 允许的主机范围 | DSL: `call jrpc --send` | — | 发送未分类的 JSON-RPC 帧。 |

### 传输任务（3）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出任务<br><small>List tasks</small> | `task.list` | 标准 | 允许的主机范围 | DSL: `ls task` | — | 未提供说明 |
| 查看任务<br><small>Inspect tasks</small> | `task.info` | 标准 | 允许的主机范围 | DSL: `get task` | — | 未提供说明 |
| 取消任务<br><small>Cancel tasks</small> | `task.cancel` | 标准 | 允许的主机范围 | DSL: `close task` | — | 取消由 Termcp 启动的传输任务。 |

### 主机配置（6）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出主机<br><small>List profiles</small> | `profile.list` | 标准 | 允许的主机范围 | DSL: `ls host`<br>工具: `ssh_config`<br>WebUI: `/api/connections GET` | — | 未提供说明 |
| 查看主机摘要<br><small>View profile summaries</small> | `profile.get` | 标准 | 允许的主机范围 | DSL: `get host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} GET` | — | 未提供说明 |
| 创建主机配置<br><small>Create profiles</small> | `profile.create` | 高风险 | 全部主机（全局） | DSL: `create host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} PUT` | — | 创建包含连接材料的主机配置。 |
| 修改主机配置<br><small>Edit profiles</small> | `profile.edit` | 高风险 | 全部主机（全局） | DSL: `edit host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} PUT` | — | 修改主机配置的目标或凭据。 |
| 复制主机配置<br><small>Copy profiles</small> | `profile.copy` | 高风险 | 全部主机（全局） | DSL: `copy host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/batch GET` | — | 复制主机配置；副本仍包含连接材料。 |
| 删除主机配置<br><small>Delete profiles</small> | `profile.delete` | 高风险 | 全部主机（全局） | DSL: `rm host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} DELETE` | — | 删除主机配置及其连接材料。 |

### 扩展（8）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出扩展<br><small>List extensions</small> | `ext.list` | 标准 | 允许的主机范围 | DSL: `ls ext`<br>工具: `extension`<br>WebUI: `/api/extensions GET` | — | 未提供说明 |
| 查看扩展摘要<br><small>View extensions</small> | `ext.get` | 标准 | 允许的主机范围 | DSL: `get ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} GET` | — | 未提供说明 |
| 启动并使用扩展<br><small>Use extensions</small> | `ext.use` | 敏感 | 允许的主机范围 | DSL: `open ext`<br>工具: `extension` | — | 启动或获取扩展实例的使用通道。 |
| 释放扩展<br><small>Release extensions</small> | `ext.release` | 标准 | 允许的主机范围 | DSL: `close ext`<br>工具: `extension` | — | 释放扩展使用通道，保留扩展定义。 |
| 重建扩展<br><small>Retry extensions</small> | `ext.retry` | 标准 | 允许的主机范围 | DSL: `edit ext --restart`<br>工具: `extension`<br>WebUI: `/api/extensions/{name}/retry POST` | — | 按当前扩展定义重建受管实例。 |
| 创建扩展定义<br><small>Create extensions</small> | `ext.create` | 高风险 | 全部主机（全局） | DSL: `create ext`<br>工具: `extension`<br>WebUI: `/api/extensions POST` | — | 创建可执行扩展定义。 |
| 修改扩展定义<br><small>Update extensions</small> | `ext.update` | 高风险 | 全部主机（全局） | DSL: `edit ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} PUT` | — | 修改扩展的程序、端点、白名单或目标。 |
| 删除扩展定义<br><small>Delete extensions</small> | `ext.delete` | 高风险 | 全部主机（全局） | DSL: `rm ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} DELETE` | — | 删除扩展定义并停止其受管实例。 |

### 公共别名（3）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 列出公共别名<br><small>List aliases</small> | `alias.list` | 标准 | 允许的主机范围 | DSL: `ls alias` | — | 查看所有 Agent 共用的词面。 |
| 修改公共别名<br><small>Write aliases</small> | `alias.write` | 敏感 | 全部主机（全局） | DSL: `alias create` / `alias edit` | — | 修改所有 Agent 共用的词面。 |
| 删除公共别名<br><small>Delete aliases</small> | `alias.delete` | 高风险 | 全部主机（全局） | DSL: `alias rm` | — | 删除所有 Agent 共用的词面。 |

### 通知（1）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 发送通知<br><small>Notify the operator</small> | `user.notify` | 标准 | 允许的主机范围 | DSL: `notify`<br>工具: `notify_user`<br>WebUI: `/api/notify POST` | — | 向 WebUI 发送操作员通知。 |

## 组合依赖

一次组合请求必须同时获得下列全部动作权限。

| Recipe | 名称 | 必需动作 | 可选动作 | 说明 |
| --- | --- | --- | --- | --- |
| `dsl.run` | Run a command line | `shell.input`, `shell.key`, `shell.output` | — | Types a line, presses Enter, and reads the requested output window. |
| `dsl.connect` | Connect to a host | `session.start`, `shell.open` | `shell.exec` | Starts a session and opens its first channel. |
| `dsl.file-copy` | Copy file data | `file.read`, `file.write` | — | Reads one endpoint and writes the other. |

## 预置权限组

| 模板 | 名称 | 版本 | 允许动作数 | 主机范围 | 说明 |
| --- | --- | ---: | ---: | --- | --- |
| `no-access` | 未授权 / No access | 3 | 0 | `all` | 业务动作默认禁止；主机范围本身不授予操作权限。 |
| `viewer` | 资源观察 / Viewer | 3 | 8 | `all` | 查看安全摘要和状态，不读取终端输出或文件正文。 |
| `diagnostics` | 只读诊断 / Diagnostics reader | 3 | 18 | `all` | 读取已分配资源的输出和文件内容，需要已有的可达会话。 |
| `file-maintainer` | 文件维护 / File maintainer | 3 | 21 | `all` | 允许文件读写和目录维护；不授予终端执行、网络转发或全局配置管理。 |
| `host-operator` | 主机运维 / Host operator | 3 | 51 | `all` | 允许连接主机、操作终端、常用文件、转发和已配置扩展；不授予凭据配置、别名管理或 raw 协议。 |

### 未授权 / No access (`no-access`)

- 不授予任何业务动作；`host_mode=all` 只表示没有额外主机范围限制。

### 资源观察 / Viewer (`viewer`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `task.list`（allow）
- `task.info`（allow）
- `profile.list`（allow）
- `profile.get`（allow）
- `ext.list`（allow）

### 只读诊断 / Diagnostics reader (`diagnostics`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `shell.output`（allow）
- `shell.marks`（allow）
- `shell.detect`（allow）
- `file.read`（allow；forms=inline）
- `file.stat`（allow）
- `file.realpath`（allow）
- `file.readlink`（allow）
- `file.getwd`（allow）
- `file.statvfs`（allow）
- `file.urls`（allow）
- `task.list`（allow）
- `task.info`（allow）
- `profile.list`（allow）
- `profile.get`（allow）
- `ext.list`（allow）

### 文件维护 / File maintainer (`file-maintainer`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `shell.output`（allow）
- `shell.marks`（allow）
- `shell.detect`（allow）
- `file.read`（allow；forms=inline）
- `file.stat`（allow）
- `file.realpath`（allow）
- `file.readlink`（allow）
- `file.getwd`（allow）
- `file.statvfs`（allow）
- `file.urls`（allow）
- `file.write`（allow；forms=inline）
- `file.replace`（allow；forms=inline）
- `file.mkdir`（allow）
- `task.list`（allow）
- `task.info`（allow）
- `profile.list`（allow）
- `profile.get`（allow）
- `ext.list`（allow）

### 主机运维 / Host operator (`host-operator`)

- `session.list`（allow）
- `session.info`（allow）
- `session.start`（allow）
- `session.terminate`（allow）
- `shell.list`（allow）
- `shell.open`（allow）
- `shell.close`（allow）
- `shell.exec`（allow）
- `shell.resize`（allow）
- `shell.output`（allow）
- `shell.input`（allow）
- `shell.key`（allow）
- `shell.marks`（allow）
- `shell.detect`（allow）
- `shell.reader.register`（allow）
- `shell.reader.unregister`（allow）
- `file.read`（allow；forms=inline, local, pipe）
- `file.stat`（allow）
- `file.realpath`（allow）
- `file.readlink`（allow）
- `file.getwd`（allow）
- `file.statvfs`（allow）
- `file.urls`（allow）
- `file.write`（allow；forms=inline, local, pipe）
- `file.replace`（allow；forms=inline）
- `file.delete`（allow）
- `file.mkdir`（allow）
- `file.truncate`（allow）
- `file.chmod`（allow）
- `file.chown`（allow）
- `file.chtimes`（allow）
- `file.symlink`（allow）
- `file.link`（allow）
- `file.rename`（allow）
- `forward.list`（allow）
- `forward.open.local`（allow）
- `forward.open.remote`（allow）
- `forward.open.dynamic`（allow）
- `forward.close`（allow）
- `task.list`（allow）
- `task.info`（allow）
- `task.cancel`（allow）
- `profile.list`（allow）
- `profile.get`（allow）
- `ext.list`（allow）
- `ext.get`（allow）
- `ext.use`（allow）
- `ext.release`（allow）
- `ext.retry`（allow）
- `alias.list`（allow）
- `user.notify`（allow）

## 常用权限预览

权限页面提供以下只读预览入口；预览不会执行命令、创建会话或启动传输。

| 名称 | DSL 模板 |
| --- | --- |
| 列出会话 | `ls` |
| 查看主机 | `get host <已保存主机名>` |
| 建立连接 | `connect <已保存主机名>` |
| 读取终端 | `read <session>:<channel>` |
| 运行命令 | `run <session>:<channel> $1` |
| 读取文件 | `read <session>:<channel> file <remote-path>` |
| 写入文件 | `write <session> file <remote-path> --data <text>` |
| 列出转发 | `ls forward` |

## 说明

- `shell.detect` 检测 Termcp 本机使用的 shell，不探测远端主机。
- `shell.input`、`shell.key` 和 `shell.exec` 是主机运维的基础动作，风险级别为“标准”；真正的权限边界还包括目标主机、namespace、目标系统账号和资源限制。
- 终端动作本身不是操作系统沙箱。获得 shell 权限后，远端命令仍可能绕开单独关闭的文件操作。
- 原始 MCP/CDP/JSON-RPC 帧权限比指定工具或方法更宽，默认仍为高风险。
- 文件 `forms`、`max_bytes`，按键 `keys`、`repeat_max`，以及工具/方法 allowlist 都是服务端可验证条件；未知长度或无法证明的约束应保守拒绝。
- 预设组只提供初始策略快照。管理员可以在组级策略中修改，未覆盖的实例按缺省组处理。
