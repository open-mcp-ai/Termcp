# 文件操作：有效参数与字节权限

状态：2026-11-02，#232 的 P02 在当前收敛范围内完成；尚未提交或合并。当前范围按用户要求仅覆盖鉴权，普通 DSL 读取接口不扩展。

## 共享鉴权证据

`internal/fileop` 不访问文件、解析主机或启动任务，只解释现有操作的有效读取/写入参数。MCP 执行 handler 和 capability 权限分类使用同一份参数；SFTP 内联写入使用同一解码器。DSL 保持既有定义、参数和含义，调用能力前做权限判断。

V2 读取的 local_path 只在 mode=file 时构成真实本地传输；text/hex 不借此要求额外本地写权限。file_replace 不使用 local_path，按 inline 分类。执行保持原有行为；未发布的 V1 权限兼容门禁已删除，仅使用当前动作策略。

## 既有接口可提供的字节上限

| 形式 | 鉴权证据 | 检查的动作 |
| --- | --- | --- |
| 内部 file_read text/hex | 既有正 length 与 8 MiB 内存上限的较小值；非正/省略使用既有内存上限 | file.read，forms=inline |
| 普通 DSL 文件读取 | 既有 8 MiB 内存上限；不新增 length，也不把 bytes 改成普通文件读取别名 | file.read，forms=inline |
| 内联写入 | hex、hex: 或文本转义解码后的真实字节数；length 不缩小 inline data | file.write，forms=inline |
| 内部下载到本地 | 既有正 length；非正/缺失则没有可证明上限 | 源 file.read + internal 的 file.write，forms=local |
| 本地上传 | 既有正 length；非正/缺失则没有可证明上限 | internal 的 file.read + 目标 file.write，forms=local |
| 文件到文件管道 | 源端既有正 --bytes | 源 file.read + 目的 file.write，forms=pipe |

两端必须分别满足各自动作的权限和同一字节上限。仅依赖 term 的展示 window，或终端输出管道，当前不提供更小的执行前字节证据；带 max_bytes 的策略在证据不足时拒绝。

不修改长度为零时的原有行为，不改变字符集、hex、转义、写入位置或管道复制语义。上限计量源文件/实际写入字节；解码和序列化后的回复字符数可以不同。它是单次操作限制，不是累计读取配额。

file.replace 继续只支持 forms，不接受 max_bytes：操作会读取并重写整个文件，不能拿 new_text 长度作为整个操作上限。其他高级条件仍属于 P01。

## MCP 后台工作授权

SSE message 在 HTTP 202 后执行。后台登记检查实际 message context，并绑定 admission 时的主体与角色入口修订。消息取消或权限改变后，即使权限随后恢复，旧消息也不能登记新工作；新请求可以使用恢复后的权限。已登记工作继续按 O01 在撤权时取消。

管道参数展开属于 DSL 解析行为，本轮已撤回该修改。权限回归使用既有管道语法与确定的目标路径，避免把解析器修复混入鉴权任务。

## 接口边界

普通 DSL 文件读取没有提供小于既有内存上限的有效 length。按用户要求不扩展 DSL；配置更小的 max_bytes 会保守拒绝，使用既有 8 MiB 上限可以正常读取。该行为是明确合同而非部分实现，因此 P02 在当前范围内完成。
