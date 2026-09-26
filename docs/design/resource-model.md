# 资源模型与生命周期

## 资源层级

```
SSH 连接
  └── Session（会话容器）
        ├── Shell[]（终端 channel，0..N 个，平等，无根/子区分）
        ├── Forward[]（端口转发 tunnel，0..N 个）
        └── File（SFTP 客户端，Session 级别复用）
```

## Session

**定义**：SSH 连接的管理容器。持有 SSH Client，维护下属 Shell 和 Forward 列表，不负责 I/O。

**创建**：`session_start` → 建立 SSH 连接 → 创建 Session 实例。

**销毁时机**：
- 用户显式 `session_terminate` / REST `DELETE /api/sessions/{id}` / `disconnect`
- SSH 连接意外断开（被动检测，见下文）

**销毁行为**：级联关闭所有 Shell → 关闭所有 Forward → 关闭 SSH Client → 从 registry 移除。

**状态**：
- `running`：Session 未关闭，SSH 连接存活（不要求存在 `running` 的 Shell，PTY 容器可在所有 Shell 退出后继续复用）
- `exited`：Session 已结束；保留在 registry 中供只读查看（自然退出/异常断线）或归档（显式 terminate），显式 `DELETE` 后移除

## Shell

**定义**：SSH 连接上的一个 terminal channel。所有 Shell 无论何时创建都是同级的，代码中不存在 "根 shell" 或 "主 shell" 的概念。

**创建**：
- `session_start`：建 Session 时同时创建第一个 Shell（由参数 command/args 决定具体行为）
- `shell_open`：在已有 Session 上创建新 Shell

**I/O**：所有输入输出通过 Shell ID 寻址。`shell_input`、`shell_output` 的目标都是 Shell。

**销毁时机**：
- 用户显式 `shell_close`（手动关闭 = 删除，不是 DEAD）
- 所属 Session 终止时级联关闭

**销毁行为**：关闭 channel → 从 Session 的 Shell 列表和保留快照中移除 → 推送 UI 更新。手动关闭的 shell 不会以 `end`/死态 tab 复活；只有自然退出/异常断线的 shell 才保留 `exited` 元数据供只读视图使用。

**退出（自然）与关闭（手动）的区别**：
- 自然退出：shell 状态置为 `exited`，保留在 channel 列表中供读取末尾输出；DEAD/只读视图会展示其快照 tab。
- 手动关闭：直接从 Session 中删除，不留任何状态。shell 的生命周期不决定容器的生命周期：即使关掉最后一个 shell（或最后一个 pipe shell 自然退出），容器保持 `running`，端口转发、SFTP 和新建 shell 都继续可用。

## Forward

**定义**：基于 SSH 连接的端口转发 tunnel。属于 Session，不绑定特定 Shell。

**创建/销毁**：通过 `forward(action=local/remote/dynamic/close)` MCP 工具操作（OpenSSH 语义：-L / -R / -D）。

**级联清理**：Session 终止（显式或 SSH 断开）时，Session 持有的所有 Forward 一并关闭。

## File（SFTP）

**定义**：SSH 连接上的 SFTP 客户端。Session 级别复用——首次文件操作时打开，后续操作共用，Session 终止时关闭。属于 Session 持有的持久资源。

**当前状态**：每次文件操作临时 `sftp.NewClient` + `defer Close`，不复用。

## SSH 断线检测

**场景**：SSH 连接因网络故障、服务端超时等原因意外断开。

**当前状态**：**已实现**。检测挂在每个 Shell 的退出 watcher 上（`startReaders` 中等待 `<-execSession.Done()`）：

- Shell 的 SSH channel 结束时先落定退出码。若本次结束**既非主动关闭**（`deliberateClose`，来自 `TerminateShell`/`CloseChildShell`）**又非正常进程退出**（`ExecSession.Aborted()` == `err != nil && !isExitError(err)`，即 transport/连接错误而非 `*ssh.ExitError`），判定为 **SSH 断线**。
- 此时由该 Shell 把**父 Session 标记为 DEAD**，并写入系统消息 `❌ SSH connection lost — network disconnected`。
- 一条 SSH 传输断开时其上所有 channel 会一并结束，因此最先观察到 abort 的 watcher 即完成整个 Session 的收敛（`exitOnce` 保证只跑一次）。

**自然退出与容器状态**：

- Shell 正常退出后，容器保持 `running`（可继续新建 Shell 或使用端口转发/SFTP），退出的 Shell 以 `exited` 元数据保留，供只读读取末尾输出。
- 容器的状态只由 SSH transport 决定，跟 shell 的数量与生命周期无关：关掉最后一个 shell（或最后一个 pipe shell 自然退出）不会把容器翻为 DEAD —— 零 shell 但 `running` 的容器是合法的可复用状态（transport 仍承载端口转发、SFTP 与 `shell_open`）。

**已知边界**：断线检测没有独立的后台探测器，它跟着 shell 的 watcher 跑。零 shell 时 transport 断了不会立即被抓到，要等下一次操作（`shell_open` / 端口转发 / SFTP）失败才暴露；TCP keepalive 已开启，半开连接最终也会被内核回收。若要求零 shell 也即时收敛，需要加一个与 shell 无关的连接监视器。

**DEAD 的边界**：断线收敛只把 Session **就地置为 DEAD（`exited`），保留在 registry 中只读**（"断开 ≠ 删除"）。释放资源需显式 `Delete`，移入历史库需 `ArchiveAndForget`（见 `docs/api.md`）。

## 推送机制

### 后端
所有影响 UI 状态的变更统一通过 `notifyListChange()` 推送（WebSocket / SSE）：
- Session 创建/终止
- Shell 创建/关闭/自然退出
- Forward 创建/关闭

当前实现：Session 变更通过 `Manager.notifyListChange()`，Forward 变更通过 `ForwardManager.notifyChange()`，Shell 变更通过 `Session.onChildChange()`。三者合一一同推送到 `sessionHub.broadcast()`。

### 前端
WebSocket `{type: "sessions"}` 消息到达时：
- 刷新 Session Grid（`renderSessionGrid`）
- 刷新 Forward（`loadForwards`，含 tools panel + shell window FW tab）
- 刷新 Shell Tab（`refreshAllWindowTabs`，重新拉取 `/api/sessions/{id}/child-shells`）

## 与当前代码的差异

| 项目 | 当前代码 | 目标状态 |
|------|---------|---------|
| Session 持有 execSession | 是，`sendInput` 直接写 `s.execSession.Stdin` | 剥离，I/O 全部走 Shell |
| Session 持有 buf | 是，`ReadTerminalStream` 读 `s.buf` | 剥离 |
| primaryShell 字段 | 存在（`primaryShellID`），供 legacy Session 级 helper 使用 | 删除，所有 Shell 平等存储在 `childShells` map |
| rootShell / 根 shell 概念 | 首个 Shell 在代码中仍以局部变量 `root` 命名并特殊处理 | 删除，无此概念 |
| SSH 断线检测 | **已实现**：Shell channel `Done()` 时用 `Aborted()` 判定 → 父 Session 置 DEAD（见上节） | 保持 |
| Shell 自然退出清理 | **已实现**：保留 `exited` 元数据于 map、drain 管道、关缓冲、推送 UI；仅手动关闭才从 map 移除 | 保持 |
| Forward 级联清理 | 有（`CloseBySession`） | 保持 |
| File（SFTP）复用 | 无，每次操作临时 `NewClient`（见上） | Session 级复用 |
