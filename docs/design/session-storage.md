# 会话存储布局

## 一条规则

**offset = `log.bin` 的文件位置。不计算，不推导。**

```go
off, err := f.Seek(0, io.SeekEnd)   // AppendLog：写之前先问文件多大
_, err = f.Write(data)
return off, nil                     // 这就是该段字节的起始 offset
```

读窗口就是定位读，`total` 就是文件大小：

```go
f.ReadAt(buf, offset)      // ReadLog
st.Size()                  // LogSize → total
```

三个数都来自同一个文件，**结构上不可能互相矛盾**。

这条规则是针对旧结构的：旧结构把 offset 定义成"输出记录长度之和"（`sum(byte_size)`），
而长度是**算出来的**。一旦算出来的长度与实际字节数不符，坐标系就分裂 ——
同一个 offset 在不同代码路径下指向不同字节。新结构让这个问题**结构上不可能发生**。

## 目录结构

```
<data-dir>/
  sessions/
    <session_id>/
      manifest.json           # 会话本身的信息
      <shell_id>/
        manifest.json         # shell 本身的信息
        log.bin               # 逐字节完整内容（唯一真相）
        log.jsonl             # 状态 + 时间戳 + 偏移，只索引，无载荷
```

- shell 列表由子目录派生，`manifest.json` 不冗余存 shells 数组
- `:N` 定位符顺序 = shell `manifest.json` 的 `created_at` 排序
- 清理 = 删除整个 `sessions/<session_id>/` 目录（点 X 即关闭+删除，无归档逻辑）

## log.jsonl

一行一个区段标记：

```json
{"s":"o","t":1758499200123,"i":0}
{"s":"a","t":1758499205123,"i":200}
{"s":"i","t":1758499206789,"i":210}
```

| key | 含义 |
|---|---|
| `s` | 状态：`o` = 输出，`a` = AI 输入，`i` = 接口输入 |
| `t` | 时间戳（Unix 毫秒，与 `internal/forward` 的 `UnixMilli()` 一致） |
| `i` | `log.bin` 的**字节偏移**，该区段起点 |

**区段语义**：从 `i` 起，到下一行标记之前，状态为 `s`。
最后一段以文件大小为止，所以 **mark 不存 end** —— 下一行的 `i` 就是本区段终点，
没有第二个副本可以失同步。

**只放索引和状态，无载荷。** 内容全部只在 `log.bin`。

`t` 是**状态变更时刻**，不是每个字节的时刻 —— 因为标记行只在状态变化时追加（见"写入"）。

新增状态只需追加一种 `s` 值，不需要改 schema。

### 输入是零长度标记，不是字节

**实测（PTY 回显）**：向 shell 输入 `ECHOPROBE12345` 并回车，输出流新增 462 字节，
该标记在其中出现 **3 次** —— 终端自己把按键回显到了输出流：

```
[1] <ESC>[93mECHOPROBE12345<ESC>[37m      ← PowerShell 的行编辑器回显
[2] ...ECHOPROBE12345: 术语...            ← 错误消息里的引用
[3] ...'ECHOPROBE12345' 不会被识别...     ← 错误正文
```

所以**输入字节已经在 `log.bin` 里了**（通过回显），而且实测 `UNIQUEMARKER42`
在 `log.bin` 中只出现 **1 次** —— 若再把按键写一份，同一按键会出现两遍。

因此：

- 输入只写一条 **零长度标记**（`i` = 当前 log 末尾，区段长度 0），记录「谁在何时输入」
- 输入字节**不写入** `log.bin`

**`log.bin` 因此严格等于终端画面**（逐字节 = 屏幕上出现过的字节），回放不会重复。

标记**与终端是否回显无关**：`WriteStdin` 成功即记一条零长度标记。回显与否只决定
`log.bin` 里有没有对应字节：

- 有回显 → 标记后跟着回显字节，标记是「点」，字节是终端的
- 无回显（密码、TUI 关回显）→ 标记仍在，但没有字节

**密码因此不进日志** —— 不是靠跳过标记，而是因为终端不回显就没有字节可存。
代价是「这里有输入」这个事实会留下，输入内容不会。

### 输入标记是「点」而不是「区段」

因为输入不写字节，「区段 = 到下个标记为止」这个规则对输入不成立：输入标记与紧随其后的
输出标记 offset 相同，长度为 0。实测序列：

```json
{"s":"i","t":...,"i":0}      ← 输入发生
{"s":"o","t":...,"i":0}      ← 同一位置立刻开始输出
{"s":"i","t":...,"i":725}
{"s":"o","t":...,"i":725}
{"s":"a","t":...,"i":995}
{"s":"o","t":...,"i":995}
```

所以：

- **输入标记只能当时间点读**（谁在何时输入），不能当区段读
- **输出区段可能被输入标记切成多段**（同一位置的 `i` 把前面的 `o` 区段边界提前）

消费者若要「一段连续输出」，应忽略零长度标记，将相邻同状态区段合并。当前 MCP 的
`message(action=list)` 直接列出标记，不做合并 —— 客户端能看到输入发生的时刻。

**这是一个取舍，不是缺陷**：如果将来需要输入字节本身（而非仅时刻），必须回到
「输入也写入 `log.bin`」的方案，并接受与回显的重复（或先关闭终端回显）。

## 写入

```
1. append log.bin       ← 先
2. append log.jsonl     ← 后（且仅在状态变化时追加一行）
```

**顺序不能反。** 崩在 1 和 2 之间：`log.bin` 尾部多一段无标记字节，按上一区段状态归属 —— 安全。
反序会产生**指向 EOF 之外的标记**，不可恢复。

**状态不变时不追加标记行。** `lastStatus` 按 shell 记住上次状态；否则每 4096 字节产生一行，
jsonl 退化成碎片列表。实测 400 KB 输出只产生 **4 行**标记（不是 97 行）。

`log.bin` 句柄按 shell 缓存（`map[string]*os.File`），否则每 4096 字节一次 `open()`。

### 不变量：`log.bin` 的位置由自己定义，内存 buffer 只能跟随

输出循环的顺序是：

```
1. append log.bin  → 返回 offset   （位置的唯一来源）
2. buffer.WriteAt(chunk, offset)   （校验自身编号是否等于 offset）
```

`WriteAt` 不做放置，只做**断言**：若 buffer 认为的末尾与 log 报的 offset 不符，
立即报错并停止记录，而不是让后续每次读取都静默地指向错误的字节。

这使「两个组件各自数字节」在结构上不可能分叉 —— 一处定义，另一处跟随并校验。

落盘失败时的选择：**不为内存保住字节**。若 `log.bin` 写失败，停止该 shell 的记录
而不是仅留在内存 —— 否则就回到了「内存有、磁盘无」的漂移。

### buffer compact 不移动坐标系

buffer 有 `baseOffset`（已丢弃前缀的绝对偏移）。compact 时：

```go
b.master = b.master[minPos:]
b.baseOffset += minPos        // ← 对外编号不变
```

`Len()` = `baseOffset + len(master)`，**流长度只增不减**。compact 只回收内存，
不改变任何一个字节的对外 offset。这是旧版偏移漂移的根因：旧实现用
`len(master)` 当长度，丢弃前缀后所有 offset 一起左移。

## 读取

| 操作 | 实现 | 复杂度 |
|---|---|---|
| 任意窗口 | `pread(log.bin, offset, max)` | O(窗口) |
| tail | 从文件尾向前 seek | O(窗口) |
| 行对齐 | 窗口内找 `\n` | O(窗口) |

不需要跨记录拼接、不需要累加长度、不需要索引自愈。

**没有"合并流"概念。** 读的是一个 shell 的窗口；session 级读取解析到 primary shell。

## 传输编码：标准 JSON，不用 base64

WS / REST 直接把 `log.bin` 的字节放进**标准 JSON 字符串**，不引入 base64。

**约束**：JSON 字符串只能承载合法 UTF-8。依赖前提：`log.bin` 内容是合法 UTF-8。

- Windows：ConPTY 在到达 Termcp 之前已把非法序列替换为 U+FFFD（实测确认），前提成立
- Linux：PTY 原样透传，`cat` 二进制会产生非法序列，此时 JSON 会替换为 U+FFFD

即：**乱码内容可能变成 U+FFFD，但结构不受影响**（offset 仍是文件位置，`log.bin` 本身不丢字节）。

前端删除 `bytesToUtf8Lossy`（那是历史回放的损坏源），直接把 `Uint8Array` 交给 xterm，
让它的**有状态**解码器拼接跨帧的多字节字符。

## 不变量

1. **`log.bin` 是字节的唯一真相。** 内存 buffer 只是缓存，其位置必须等于文件偏移，
   **不得独立定义坐标系**。
2. **offset 只由文件位置定义。** 任何地方不得"累加推导"偏移。
3. **`log.bin` 只追加，永不改写、永不截断。** 截断会移动所有 offset。
4. **`log.jsonl` 只在状态变化时追加。**
5. **先 `log.bin`，后 `log.jsonl`。**

## 为什么"不切分"

字节流是 append-only 的：**已写入的数据永不改变，查询也不需要切分。**

`4096` 这个数字**不是设计决策** —— 它是 `make([]byte, 4096)` 这个 I/O 读缓冲的大小。
一个 I/O 细节被提升成了存储模型，于是"一次 read"变成了"一条记录"，进而必须有一个东西回答
"第 N 条在偏移多少" —— 这就是 `index.json` 和 `byte_size` 的来源。

**没有切分 → 不需要索引 → 没有 `byte_size` → 没有偏移分裂。**

## 实测：旧结构的代价

输出 400 KB，实测磁盘产物：

```
碎片文件数        : 131
碎片总大小        : 639588 bytes
实际内容          : ~400000 bytes
存储放大倍数      : 1.60x
index.json 大小   : 27927 bytes (131 条目)
```

每写一条输出记录的 I/O：

```
1. 新建 1 个文件（uuid.json）+ fsync
2. 读整个 index.json    (27927 bytes)
3. 重写整个 index.json  (27927 bytes, 全量)
```

**写第 N 条的索引成本 = O(N)，总成本 = O(N²)。** 索引全量重写对并发写不友好。

新旧对比（同样 400 KB 输出）：

```
旧: 131 个碎片文件, 639588 bytes (1.60x 放大), index.json 27927 bytes
    每次写入：新建文件 + 读整个 index + 重写整个 index  → O(N²)

新: 3 个文件 (log.bin 456128 + log.jsonl 140 + manifest.json 274)
    存储放大 1.14x, 标记 4 行
    每次写入：append log.bin + 偶发 append 一行  → O(1)
```

## 旧布局：不读、不迁移、不清理

旧布局（`sessions.json` + `messages/<session_id>/index.json` + 碎片记录）**不再读取**。

- 旧数据解析代码**已删除**，仓库中不存在
- 磁盘上的旧文件**原地不动**：没有任何主动清理、迁移或删除旧文件的代码
- 旧会话不再出现在 `session_list` 中（它们只在旧布局里）
- `api.Message` / `api.MessageIndexEntry` / `MsgType` 等旧记录类型已随解析代码一起移除

**"宽容"指不乱删磁盘文件，不是保留解析代码。** 不写清理逻辑是怕误删；不读旧数据是
因为新路径只有一个实现。两者不矛盾：**只写入，不读取。**

判据：新写入走新布局；旧目录**原地不动**，也不出现在会话列表里。

## 实测：跨重启偏移稳定

120 KB 多字节输出：

```
live:         total=120938
重启后:       total=120938          ← 未变
同一 offset:  同一字节              ← 稳定
全流:        逐字节相同
U+FFFD:      无
"中文测试数据" 出现 5411 次（live 也是 5411）
```

>120 KB 是必要的：8.5 KB 时实测**假绿**（PTY 读边界恰好不切在字符中间），200 KB 才暴露。

`>8 MiB` 触发 buffer compact 后：

```
total = 11377969 == log.bin 大小
窗口 start 与请求一致，两次读取相同
tail 结束于 total
REST 返回的字节 == 文件对应区间
```

崩溃一致性：模拟 `log.bin` 已写、`log.jsonl` 未写 —— 读取不崩溃，
尾部字节按上一区段状态归属。

## 实现中修正的两个设计问题

1. **DEAD 会话读不出内容**（实测发现）：恢复的会话没有 live shell，`outputSource.shellID`
   被留空，于是去读一个不存在的 shell，返回空。修正：解析到该会话的第一个 shell。
   教训：日志属于 shell，空的 shell id 不是一个合法的日志地址。
2. **`RestoreDead` 与旧列表**：旧实现只加载新布局。现已改为只用新布局 ——
   旧会话不出现，这是预期行为。

## 验证时要注意：PTY 会插入换行，不要误判为丢字节

用「重复模式出现次数」验证输出完整性会得到偏低的数字 —— **不是丢字节，是终端换行**。
PowerShell 控制台在 80 列处换行，并在字符中间插入 `\r\n` 与光标定位转义，
于是 `中文测试数据` 被拆到两行，模式计数下降。

实测：

```
重复模式 "中文测试数据"×6000 → 捕获 5411 次     ← 看似丢了 589
改用单字符 "中"×20000 计数       → 捕获 20487 个  ← 完整（多出的是命令回显）
```

**所以验证输出完整性时，要按「单个字符出现次数」或「总字节数」判定，不要按多字符模式判定。**
（或者干脆不经过 PTY：直接比对 `log.bin` 与写入端。）

## 涉及文件

```
internal/storage/store.go           log.bin / log.jsonl / manifest.json
internal/message/message.go         按状态追加字节，标记只在变化时写
internal/session/session.go         输出先写 log 再校验 buffer；输入为零长度标记
internal/session/manager.go         每会话/每 shell 写 manifest；RestoreDead 只认新布局
internal/mcp/outputsource.go        DEAD 会话解析到第一个 shell
internal/mcp/handlers.go            message(action=list) 返回 span；输入标为 AI
internal/webui/handler.go           统一读路径；传输改为 JSON 字符串
internal/webui/ws.go                WS 帧去掉 base64
internal/webui/assets/index.html    前端去 base64，直接写 Uint8Array
internal/buffer/buffer.go           baseOffset 绝对偏移 + WriteAt 一致性校验
pkg/api/types.go                    LogStatus / LogMark
main.go                             关停时 Store.Close()
```
