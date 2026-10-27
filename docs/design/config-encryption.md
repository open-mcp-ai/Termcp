# 主机配置逐条信封加密实施方案

状态：P1 双层信封、P2 明文/密文混合 Store、P3 页面上主动启用与逐主机转换已实现；人端密码解锁、断点恢复与密钥轮换尚未实现。日期：2026-10-27。
代码核对基线：`dev@6a8af551206ef2dcd73abbf47871ac90b4fa568f`；参考分支：`encryption-test@d3e04a3fe220e86fe77e68101182a322b7b12ccc`。

本方案落实 Main Key → DEK → 配置正文的密钥层级，衔接 [Agent 权限设计](agent-permissions.md) 和 [命令解耦方案](command-decoupling.md)。下文明确区分已落地部分与后续合同；未标注已实现的管理接口、断点恢复和轮换仍不能视为当前运行时能力。

## 1. 范围与既有方案评估

### 1.1 本期交付

- 持久主机配置可选逐条加密，包括完整原始 TOML、递归跳板配置、代理认证信息及 `internal` 的可选覆盖配置。默认仍保存明文。
- 每份配置的每次正文保存使用独立随机 DEK；Main Key 只封装 DEK，不加密配置正文。
- Main Key 由系统秘密存储、显式配置的 ENV KEK 或恢复密码保护。
- 初始化、解锁、明文迁移、配置读写、改密码、轮换及中断恢复形成可验证闭环。
- 人主动导出仍可选择默认明文或独立文件密码；Agent 不获得凭据读取、导出及密钥管理权限。

会话输入输出、会话历史、会话 manifest、扩展模板及诊断日志不在本期加密范围内。暂不实现 age recipients、多设备同步、relay、SSH CA、运行中主动锁定或 token 签发体系。扩展配置中的 headers 等秘密仍是后续工作，不能宣称本期完成了整个数据目录的加密。

### 1.2 当前落地状态

当前代码已经提供以下闭环：

- `Main Key → 每条对象独立随机 DEK → 配置正文` 的 XChaCha20-Poly1305 双层信封，以及固定 v1 golden 向量。
- Provider KEK 封装 Main Key；已实现 ENV、恢复密码和 macOS Keychain、Windows DPAPI、Linux Secret Service Adapter。
- 加密目录 catalog、稳定 Profile/Object ID、每次保存新 Generation ID、改名只提交索引、删除先移除索引，以及目录排他锁和原子发布。
- `sshconfig.Store` 同时读取明文 `config.toml` 和加密仓库；未选择的主机保持原路径与原字节，临时 profile 仍只存在进程内。
- WebUI 的“配置加密”页面可选择是否加密新建配置，也可勾选具体主机执行“加密所选”或“还原所选”。
- 启用只改变新建 profile 的初始存储方式。编辑、改名或重启既有明文 profile 不会自动转换它；编辑既有加密 profile 则继续保存为密文。
- 启动时探测 `keys/config.keys` 并在 ENV/系统托管可用时自动解锁；已启用新加密方案但缺 key 或业务 TOML 不合法时拒绝降级。
- 检测 `encryption-test` 的 `keys/master.key`、`config/hosts.enc` 时记录兼容性警告并忽略这些文件；不会读取、迁移、删除或覆盖它们，旁边的明文与新加密配置仍可使用。

尚未提供：独立的人/Agent 角色鉴权、密码交互解锁、断点转换日志、改密码、Main Key rewrap、事件后 reencrypt、旧代际持久清理清单及三平台真实 Provider 端到端验收。管理页面当前复用实例现有的全局 HTTP 认证；在接口鉴权完成前不得把该页面暴露给不受信任网络。

### 1.3 三条基线不能混淆

| 对象 | 已核对行为 | 本期处理 |
| --- | --- | --- |
| 当前 `dev` | `sshconfig.Store` 原样保存 `config.toml`；`configcrypt` 只处理导入导出 | 新增本机加密后端，保留导出协议 |
| `encryption-test` | provider KEK 封装随机 master；HKDF 派生用途密钥及文件密钥；主机正文放在 `hosts.enc` | 复用经核对的托管、持久化与恢复经验，不整分支合并 |
| 本方案 | 每个配置代际有随机 DEK，EDK 与正文同文件 | 取代主机配置的 HKDF 派生数据密钥及整库正文文件 |

参考分支不是直接拿 master 原始字节加密正文。差异在于：其正文密钥依赖 master 的派生链；本方案的 DEK 独立随机生成，可由另一 Main Key 重新封装。修改恢复密码与轮换 Main Key 是不同操作。

源码核对位置：[`Store`](../../internal/sshconfig/store.go)、[`配置结构`](../../internal/sshconfig/config.go)、[`导出加密`](../../internal/configcrypt/configcrypt.go)、[`HTTP 适配`](../../internal/webui/handler_conn.go)、[`组合根`](../../main.go)。知识图谱没有可用的本项目索引，索引尝试失败；上述结论来自这些文件的定点阅读，不是全仓调用覆盖证明。

## 2. 密钥层级与生命周期

### 2.1 统一命名

| 名称 | 职责 | 是否落盘 |
| --- | --- | --- |
| Provider KEK | 保护 Main Key 所在的密钥环；来自系统托管、ENV 或密码 KDF | 只允许系统保护后的材料或 KDF 参数落盘 |
| Main Key | 本实例配置存储的随机 32 字节封装密钥 | 只以 provider 封装后的密钥环落盘 |
| DEK | 随机 32 字节数据密钥，加密一个对象的一个正文代际 | 不以明文落盘 |
| EDK | Main Key 对 DEK 执行认证加密得到的密文 | 与对应正文密文一起保存 |
| Store ID | 随机 16 字节存储身份 | 可公开，不是密钥 |
| Profile ID | 随机 16 字节主机身份，不因改名而变化 | 位于加密目录索引及文件认证信息 |
| Generation ID | 随机 16 字节正文代际身份，每次正文保存更新 | 可公开，不是业务版本号 |

```text
系统秘密存储 / ENV KEK / Argon2id(恢复密码)
                       │ Provider KEK
                       ▼
               认证加密的 Main Key 密钥环
                       │ Main Key
                       ▼
                 EDK ──解封──→ DEK
                                  │
                                  ▼
                              配置正文
```

EDK 是密文，不作为 AEAD 的 key 使用。Main Key、DEK、Provider KEK 具有不同类型或私有包装，避免接口中混用三个裸 `[]byte`。Main Key 不从登录 token、主机名、路径或配置内容派生；DEK 不从 Main Key 派生。

第一版正文加密及 DEK 封装统一使用 `chacha20poly1305.NewX`，32 字节 key、24 字节随机 nonce、16 字节认证标签。随机数来自 `crypto/rand`。两种用途使用不同密钥、不同 nonce 和不同 AAD 域标签；不实现自定义密码算法。

### 2.2 托管与恢复

按 `encryption-test` 中对应 provider 逐项移植并独立验证：macOS Keychain、Windows DPAPI、Linux Secret Service。容器可显式提供严格 Base64 编码的 32 字节 ENV KEK，变量建议为 `TERMCP_CONFIG_KEK`，不隐式使用会话存储变量。

- 未显式选择方式时，非空 ENV KEK 优先，否则尝试系统托管；系统不可用则进入待初始化状态，供人设置恢复密码，不降级明文。
- 已有加密存储只能用现有 slot 解锁；托管失效不能触发生成新 Main Key 或覆盖旧密钥文件。
- 恢复密码使用 Argon2id：初始参数为时间 3、内存 64 MiB、并行度 1，随机 16 字节 salt，输出 32 字节 KEK。参数写入 slot；读取先检查时间 1–10、内存 8–256 MiB、并行度 1–8，再运行 KDF。发布前在三系统测量，不在每次读取配置时运行 KDF。
- 添加、修改恢复密码与切换解锁方式均要求两次新密码一致；已有密码的管理操作重新验证当前密码。自动方式可保留密码恢复 slot；手动方式只保留密码 slot。
- 密码、Main Key、DEK 不放入 argv、URL、诊断日志、浏览器存储或临时文件。CLI 密码由交互输入或显式 stdin 读取，密钥管理操作关闭请求/响应正文日志。
- 解锁后进程可持有 Main Key；DEK 和明文配置按调用持有并尽快释放。`clear` 仅尽力清理，不能承诺 Go 内存完全擦除。

磁盘加密不能抵御已经控制解锁进程、能读取同一系统身份秘密存储，或已获得 Main Key 的调用者。配置名称、数量、文件大小及文件系统时间也不全部隐藏。本期不提供防整套目录回滚的外部可信检查点。

## 3. 磁盘布局与提交点

```text
<data-dir>/
  .config.lock
  keys/
    config.keys
    config.settings                     # 新建配置是否加密
  ssh_configs/
    store.enc
    objects/
      <profile_id>/
        <generation_id>.enc
    <plaintext-name>/config.toml         # 默认布局，可长期共存
    .internal/config.toml                # 明文 internal 覆盖
  sessions/...                          # 不改变现有格式
```

第一版使用上述新路径，不复用 `keys/master.key`，也不把原始配置正文放进 `store.enc`。ID 路径统一为小写 32 位十六进制；名字、绝对路径和客户端输入不得直接构造 object 路径。普通 Profile ID 拒绝保留值及碰撞；目录索引与 internal 使用各自固定、不同的保留 Object ID。

### 3.1 三类持久对象

| 对象 | 内容与作用 |
| --- | --- |
| `config.keys` | 有界 provider slot 列表；每个 slot 认证加密同一份密钥环，内含 Store ID、活动 Main Key ID、最多两把 Main Key、解锁方式及轮换状态 |
| `store.enc` | 加密目录索引：schema、单调 revision、加密模式已提交标记、名称 → Profile ID/Generation ID 映射、internal 覆盖引用、待清理的受控对象引用 |
| 单条 `.enc` | 一个配置代际的完整原始 TOML；包含它自己的 EDK、正文密文及认证头 |

目录索引只集中身份和提交元数据，主机正文仍逐条独立。名称碰撞继续按大小写不敏感规则处理；删除后同名重建使用新 ID。改名只提交索引，不重加密正文，活会话的 Profile ID 不变。索引本身也使用独立随机 DEK 和 EDK。

正常配置保存的唯一逻辑提交点是 `store.enc` 的持久化原子替换：先写好新代际，后发布索引引用。索引未引用的文件不是已提交配置。不要通过扫描并挑选“最新 mtime”或“最大文件名”恢复配置。

### 3.2 持久化与排他访问

- `.config.lock` 使用 OS 排他锁，持有到配置存储关闭；不是通过文件是否存在判断锁。Unix 使用 `flock`，Windows 使用文件锁。该锁不含秘密，也不改变会话文件格式。
- 目录默认 `0700`、配置与密钥文件默认 `0600`；Windows 明确处理 ACL。拒绝不符合预期类型的文件、符号链接及路径越界。只能把“完全停止旧版本进程”作为首次迁移前提，不能假设旧版本认识新锁。
- 密文先在最终文件同目录的随机临时文件写入，随后 Sync、Close、平台原子替换，并确认平台支持的目录持久化。新建对象目录也要确认相应目录项持久化。
- Unix 与 Windows 的持久化 Adapter 分开；复用参考分支经核对的能力，不把目前仅 `os.Rename` 的 helper 当作完整断电保证。
- 替换前失败保留旧状态；替换已发布但持久化未确认时返回 `ErrDurability`，内存跟随已发布状态，并阻止继续修改直到核对完成。不能向调用方报告成功后再异步保存密钥。
- `Store` 变更提交与快照复制在锁内完成；通知在解锁后发送，避免 callback 重新读取 Store 时自锁。

## 4. 配置密文格式 v1

### 4.1 编码与长度约束

使用固定顺序的二进制头，整数为小端序，不使用无序 JSON map 拼接 AAD。字段严格按下表所列顺序编码；实现 PR 必须提供固定 golden 字节向量，校验字段顺序、枚举值、长度和 AAD。

| 区域 | 字段 |
| --- | --- |
| 稳定正文头 D | magic `TCPCFG01`、格式版本 uint16、算法 uint16、对象种类 uint8、Store ID 16、Object ID 16、Generation ID 16、data nonce 24、正文密文长度 uint32 |
| 可变封装头 | Main Key ID 16、wrap nonce 24、EDK 48 |
| 正文 | DEK 加密得到的正文密文及 16 字节标签 |

版本固定为 1，算法枚举 1 表示 XChaCha20-Poly1305，kind 1/2/3 分别表示 profile/catalog/migration。稳定头 D 为 89 字节，可变封装头为 88 字节，正文从第 177 字节起；总长必须等于 177 加正文密文长度，正文至少含 16 字节标签。

每种 kind 的 ID、用途及大小限制由读取方声明，不由文件内容自行决定。profile 明文上限沿用现有 256 KiB；catalog 上限 4 MiB 且最多 10,000 条；migration 上限 8 MiB 且同样最多 10,000 条。超限在分配内存前拒绝，不压缩正文，不接受尾随字节。所有上限均为格式 v1 合同，不能悄悄放宽读取器。

### 4.2 两层认证

```text
C        = XChaCha20Poly1305(DEK).Seal(data_nonce, plaintext,
              "termcp/config/data/v1" || D)

EDK      = XChaCha20Poly1305(MainKey).Seal(wrap_nonce, DEK,
              "termcp/config/wrap/v1" || D || MainKeyID || SHA256(C))
```

D 的编码长度固定；域标签固定；各个枚举值与整数只存在一种合法编码。写入时先确定明文长度及标签开销，再构造 D。EDK 的 48 字节不含独立保存的 wrap nonce。

Main Key ID、wrap nonce 和 EDK 不加入正文 AAD，否则更换 Main Key 将迫使正文重新加密。它们仍由封装层的 key 选择、nonce 与 AAD 验证。正文哈希将 EDK 绑定到确切密文，避免独立拼接封装与正文。

读取顺序：

1. 限长解析；验证版本、算法、kind、字段长度、总长度和无尾随数据。
2. 以运行时已确认的 Store ID、索引中的 Profile ID/Generation ID 核对文件，不能信任文件自报的身份。catalog 使用固定 Object ID，Store ID 来自已解锁的密钥环。
3. Main Key ID 只允许命中当前密钥环，不能触发任意网络密钥查找或无限遍历。
4. 验证并解封 EDK，再认证解密整个正文；全部成功后才交给 TOML 解析器或业务读取者。
5. 校验 kind 对应的业务结构。错误密钥、篡改及跨对象替换对业务返回统一认证失败，不泄露猜测出的密码或正文。

已知身份的文件互换必须失败；整套密钥、索引及配置被一起回滚仍可能通过认证，这是本期明确的保护边界。

### 4.3 `config.keys` 的封装合同

密钥文件是最大 64 KiB 的严格版本化 JSON，最多 8 个 provider slot；字节字段统一标准 Base64。每个 slot 包含 slot ID、provider ID、参数、nonce 和密钥环密文。provider 参数只允许其声明字段；拒绝未知版本、重复 ID、非法 KDF 成本及未知字段。

slot AAD 使用固定字段编码，绑定域标签 `termcp/config/keyring/v1`、版本 uint16、Store ID 16 字节、slot ID 16 字节，再依次编码 provider ID 与参数。变长字符串均为 uint32 字节长度加 UTF-8 正文；参数按 provider 的固定 schema 编码，不对任意 JSON 文本直接求 AAD。密码参数固定为 salt 16 字节、时间 uint32、内存 KiB uint32、并行度 uint8；ENV 参数为空；系统参数为随机引用字符串及长度前缀的系统保护 blob。provider ID 枚举及各平台参数在 P0 与 golden 向量一起冻结，第一版仅接受正式注册的 provider。

解密后再次核对密钥环内部的 Store ID、解锁方式、活动 Key ID 及所有 Key ID 唯一性。密钥环最多保留活动与轮换中待退役两把 Main Key，不无限积累历史密钥。普通文件不能指定 provider 或 KDF 参数。

## 5. Module 与 Interface

将格式验证、密钥生命周期和提交恢复集中在小 Interface 后；调用方不需要了解 nonce、slot 或文件提交顺序。生产调用者与测试通过同一 Interface 使用这些行为。

### 5.1 文件职责

| 位置（拟新增/修改） | 职责 |
| --- | --- |
| `internal/configstore/envelope.go` | 固定格式解析、Seal/Open/Rewrap、限长与 AAD；不决定权限和迁移 |
| `internal/configstore/keyring.go` | provider slots、Main Key 私有生命周期、密码重封与轮换状态 |
| `internal/configstore/provider_*.go` | OS/ENV/密码 Adapter，平台调用与秘密传输 |
| `internal/configstore/atomic_*.go` | 原子发布、Sync、文件锁、ACL 与平台错误归一 |
| `internal/configstore/store.go` | 目录索引、稳定 ID、代际文件、读写提交、孤儿清理 |
| `internal/configstore/migration.go` | 旧 TOML 迁移、清理确认、可续跑日志 |
| `internal/configstore/controller.go` | 初始化、解锁、Ready 门禁、关闭、密钥管理串行化 |
| `internal/sshconfig/store.go` 等 | 保留解析、名称规则、临时配置与业务 Store 外观，接入持久后端 |
| `main.go`、管理 handler、CLI | 依赖组装、人端授权、传输解码与状态展示 |

这不是通用加密文件系统，不移植 `sessionlog`、追加记录或历史索引。crypto primitive 留在同一 Module 的私有实现内，不向 handler 暴露原始 key 或任意用途解密入口。

### 5.2 持久后端 seam

为 `sshconfig.Store` 增加私有持久后端 Interface，概念操作为：

| 操作 | 输入/输出合同 |
| --- | --- |
| List | 返回已提交名称与稳定 ID 的快照，不暴露密钥 |
| Read | 名称解析为稳定身份，返回调用方独占的原始配置字节 |
| Apply | 接受已验证的强类型变更列表：Put、Rename、Delete；原子提交目录索引，返回 ID/版本与明确的持久化结果 |
| Status / Close | 状态快照；关闭等待所有存储操作后清理密钥并释放目录锁 |

同一 Store 组合明文 Adapter 与 envelope Adapter。每个持久 profile 有独立存储模式：明文继续位于原 `config.toml`，密文由 catalog 定位。全局“新建配置加密”只决定首次持久化选择，既有 profile 的保存、改名和启动读取保持原模式；只有用户显式选择“加密所选”或“还原所选”才改变。临时配置仍由 `sshconfig.Store` 的内存表管理，不创建密文对象或 key slot。

`Apply` 不接受任意闭包、传输 request、客户端自报路径或 key ID；业务 batch 的校验与冲突命名先完成，再提交一个确定变更集。读写序列化和 CAS revision 在 Module 内处理，调用方不用按顺序组合 Seal、写文件、写索引。

原 `Load`、`ReadRaw`、`Save`、`Rename`、`Delete` 外观可保留，逐项适配后端。需要原子“改名并更新”的 PUT 分支增加 Store 级变更方法，不能继续先 Rename 成功再 Save 失败。批量 DELETE 的逐条结果语义不改成全批事务；批量导入在完整验证后用同一提交实现。

Profile ID 随 Store 的描述/建连结果进入会话元数据，供后续主机授权使用；增加此元数据不代表加密会话文件。已有历史没有 ID 时不得仅凭同名新配置授予权限，按权限方案做保守兼容。

### 5.3 管理 Interface

Controller 提供 `Status`、`Initialize`、`Unlock`、`ChangePassword`、`SetUnlockMode`、`RotateMainKey`、`Resume`、`Close` 的明确操作。Profile Store 的普通调用者拿不到管理 Interface。持久化策略、轮换状态及解锁上下文由 Controller 负责，不由 REST、MCP 各写一份。

锁顺序固定为 Controller 管理锁 → Store 变更锁 → 私有 KeyRing 锁。普通读取只通过 Store 获得短期快照，不持 KeyRing 锁回调 Store。注入 RNG、文件持久化及 provider 的私有测试 seam，故障测试仍从正式 Interface 发起。

## 6. 保存、改名、删除和临时配置

### 6.1 正常保存

1. 沿用现有 TOML 校验，包括最大长度、跳板链、internal 覆盖约束和大小写名称碰撞。
2. 在 Store 变更锁下解析目标。新建分配 Profile ID；修改复用原 ID；复制分配新 ID。
3. 生成新 Generation ID、DEK、data nonce、wrap nonce；加密完整原始 TOML。原文 PUT 与迁移保留未知字段，不通过重建 `Entry` 丢字段。
4. 写入并持久化新代际文件；读回并完成认证校验。失败不改变索引。
5. 构建下一版目录索引，指向新代际；索引也以新 DEK 加密并原子发布。这是逻辑提交。
6. 发布内存快照，解锁后通知；旧代际进入受控清理集合。只在提交持久化确认后报告成功。

若在第 4 步之后、第 5 步之前崩溃，旧配置仍有效，新文件只是孤儿；若第 5 步已提交，第 6 步未完成，重启以索引恢复新配置。孤儿清理只处理新 objects 布局下已验证 ID 和受控引用，不扫描删除用户任意文件。

### 6.2 其他变更

| 操作 | ID、正文与提交要求 |
| --- | --- |
| 改名 | 保留 Profile ID 和正文代际；仅更新名称索引；更新活会话显示引用 |
| 复制 | 新 Profile ID、新 DEK、新正文代际；必须先获准读取/复制源配置 |
| 删除 | 索引删除先提交，再清理相关密文；活会话继续持有已建 SSH 连接，沿用现有行为 |
| persistent → temporary | 先提交持久引用移除，再发布内存临时配置；提交不确定时报错；旧密文列入清理，不能宣称安全擦除 |
| temporary → persistent | 用正式保存流程提交成功后再删除临时项；失败保留临时项 |
| 新临时配置 | 仅内存；沿用重名检查与进程期生命周期，不为了注入写索引或密钥文件 |
| internal 覆盖 | 用保留 Profile ID 独立加密，加载仍叠加内置默认；缺失已提交覆盖时不得静默回退 |

不在本次存储改造中顺带修改临时条目的导出规则、增加 TTL 或改变会话清理；如需这些行为，另列变更及测试，不能从参考分支整包带入。

## 7. 启动、解锁与管理入口

### 7.1 配置状态机

```text
UNINITIALIZED → LOCKED → MIGRATING → CLEANUP_PENDING → READY
                         └────无需迁移───────────────→ READY
READY → ROTATING → READY
任一不可恢复认证/格式错误 → FAILED
Close → CLOSED
```

自动初始化/解锁可快速经过中间状态，但不跳过验证。进入 READY 前验证 catalog 的全部活动引用，逐条认证并检查业务结构；清理完成前不报告 READY。操作失败后按日志恢复原状态或明确 FAILED，不以空目录替代损坏配置。

| 状态 | 允许的行为 |
| --- | --- |
| UNINITIALIZED / LOCKED | 人端配置状态与初始化/解锁；不读取持久凭据，不新建 SSH 连接 |
| MIGRATING / CLEANUP_PENDING | 人端进度、重试、关闭；拒绝配置写入、注入和新建连接 |
| READY | 正常配置操作；仍受人/Agent 与资源授权限制 |
| ROTATING | 读取可用，冻结配置变更、导入及模式切换；已建会话继续运行；新连接只读取已验证快照 |
| FAILED | 状态及人端恢复指引；失败关闭配置能力，不自动新建密钥或回退明文 |

会话历史仍按现有格式读取，配置未解锁不要求把历史删除或迁移。启动时先提供管理与历史读取能力；扩展协调器、自动 SSH 建连及任何依赖 profile 的操作等待配置 READY。即使请求 internal，也不能跳过待解锁的 internal 覆盖规则。第一版不提供运行中 lock；退出时先停止依赖配置的工作，再关闭配置 Store 和 KeyRing。

daemon 空闲倒计时在配置就绪后才开始，避免等待用户解锁时自动退出。stdio stdout 只输出 MCP 协议，存储未就绪通过协议错误和明确状态报告，不把密钥提示写入 stdout。

### 7.2 人端管理接口

| 接口 | 请求要点 |
| --- | --- |
| `GET /api/config-storage` | 返回新建是否加密及每台主机的 `plaintext/encrypted/temporary/builtin` 模式；不返回 key/slot 正文 |
| `PUT /api/config-storage` | `{"encrypt_new":true|false}`；启用时创建/解锁 keyring，不转换现有主机 |
| `POST /api/config-storage/encrypt` | `{"names":[...]}`；提交所选密文与 catalog 后删除对应明文源 |
| `POST /api/config-storage/restore` | `{"names":[...]}`；先原子写回明文，再移除密文引用 |

以下密码与轮换接口尚未实现：

| 接口 | 请求要点 |
| --- | --- |
| `POST /api/config-storage/unlock` | 密码只在 JSON body 中 |
| `POST /api/config-storage/password` | 当前密码、新密码、确认；保存成功后才能确认 |
| `POST /api/config-storage/unlock-mode` | automatic/password；验证当前密码和目标 provider 可用性 |
| `POST /api/config-storage/rotate` | 常规 rewrap 或事件后 reencrypt；重新验证管理凭据/恢复密码 |
| `POST /api/config-storage/resume` | 只恢复已持久化的迁移/轮换计划，不接受客户端任意路径 |

这些路由必须在配置 LOCKED 时可由可信 human 访问；Agent 即使已有 MCP token 也不能访问。不能把操作者认证记录只放进待解锁的配置存储，造成“必须先解锁才能认证解锁”的循环。可信人端识别是上线前置条件；当前全局 token 模型不能自动满足它，也不能仅凭 loopback、REST 路径或客户端 role 字段认定是人。

入口先认证与识别人/Agent，再做配置状态与操作授权。Agent 的非 MCP 业务请求先拒绝，不借锁定状态获得初始化窗口。配置加密 Module 不另造 token 签发机制，也不复用恢复密码作为 HTTP 登录密码。

密码接口要求有界 JSON、禁止跨源、`Cache-Control: no-store`、尝试频率限制；远程必须 HTTPS。只有显式可信代理可以提供转发协议声明；不信任客户端自报 `X-Forwarded-Proto`。接口在 `no_webui` 构建中同样可用，WebUI 只增加人端“配置加密”设置、解锁及进度展示。

当前 CLI 只提供只读 `termcp config-storage status`。加密开关及逐主机转换在 WebUI 完成；后续 `init/unlock/password/rotate/resume` 仍按同一鉴权原则补充。CLI 不把管理操作开放成 Agent DSL。

### 7.3 错误合同

| 内部错误 | 人端 HTTP | 业务语义 |
| --- | --- | --- |
| ErrLocked / ErrNotReady | 503 | 配置能力尚不可用，可显示状态与 Retry-After |
| ErrBusy | 409 | 正在迁移、轮换或存在并发修改 |
| ErrInvalid | 400 | 既有 TOML/输入校验失败 |
| ErrAuthentication | 解锁为 401；读取损坏数据为 503 | 密码不正确或数据不能认证；不返回秘密片段 |
| ErrVersion / ErrMissingKey | 503 | 格式不受支持或已有密文缺密钥，需人工恢复 |
| ErrDurability | 503 | 已发布但持久化未确认，不能宣称成功或简单回滚 |
| ErrForbidden | 403 | 身份已识别但无权执行；匿名认证失败仍由外层返回 401 |

MCP 使用等价结构化错误与非秘密状态，不把所有错误拼成原始系统 stderr。列表遇到已提交 profile 解密失败应明确失败，不能沿用逐条 `continue` 后返回一个看似正常但少主机的列表。

## 8. 显式逐主机转换与还原

### 8.1 启动识别

1. 无密钥、无 catalog：所有持久 profile 以明文运行，新建配置也默认明文。
2. 用户启用“新建配置加密”：创建或解锁 keyring 与空 catalog，并持久化 `config.settings`；已有 `config.toml` 不移动。
3. 此时新配置进入密文仓库；已有明文在编辑、改名或重启时仍保持明文，直到用户显式选择。
4. 密钥存在但 catalog 缺失、或已启用加密但 keyring 缺失：报错并拒绝降级，不生成替代密钥。
5. 明文目录与密文 catalog 可长期共存。同名项以密文为权威；转换/还原完成后清理残留明文，避免旧副本重新生效。

启用操作先持久化 provider 封装的密钥环，再写新的新建模式设置。任一步失败时，未完成部分不能把现有明文主机的读取路径截断。

### 8.2 加密所选主机

1. 页面只允许选择持久化且当前为明文的 profile；临时项与无 override 的内置本机项不可选。
2. 后端对整批所选 TOML 重新校验，读取原始字节并生成独立 DEK/EDK 和代际对象。
3. 所有密文对象写入并回读认证后，catalog 一次提交整批名称映射。
4. 只有 catalog 提交成功，才删除各所选主机的明文 `config.toml`。任一未选择主机保持原字节和原路径。
5. 清理失败会报告具体主机；密文已是权威状态，明文残留可重试删除，不能覆盖密文内容。

### 8.3 还原所选主机

1. 只允许选择当前为密文的 profile。
2. 先从 catalog 路径读取、解封并认证全文，再原子写入对应明文 `config.toml`。
3. 明文写入成功后提交 catalog 删除；启动和运行时均以密文为权威，因此中途失败留下的双份内容不会让旧密文被明文意外覆盖。
4. `encrypt_new` 设置保持不变。若它仍为 true，下一台“新配置”继续加密；刚还原的主机因已存在明文而保持明文，除非再次显式加密。

文件删除不是 SSD、快照、备份或系统历史中的安全擦除，不作此保证。转换期间生成的 staging 与临时文件只能包含密文。

### 8.4 参考分支数据的兼容

第一版的必备升级输入是当前 dev 的逐条明文 TOML，不直接承诺读取 `encryption-test` 的全部私有存储。若探测到其 `keys/master.key`、`config/hosts.enc` 等标志，启动与配置页显示兼容性警告并完全忽略这些文件；它们不能被当成当前明文或新 catalog 的一部分。

从参考分支迁移的人工路径为：在隔离目录用对应版本解锁并执行独立密码加密导出，再在新实例导入；不搬运会话加密数据。恢复和验证成功前保留旧目录。直接 reader 如确有需求，单独实现只读导入 Adapter 及固定样本，不复活该分支的启动链和会话格式。

## 9. 改密码与两类轮换

### 9.1 改恢复密码 / 切换 provider

重新验证当前密码，将同一密钥环封装到新 slot。Main Key、现有 EDK、正文密文和 Profile ID 均不改变。保留仍有效的其他 provider slot；切换方式时验证目标 provider 后才发布。保存未确认前不能退役旧 slot、删除系统托管项或确认新密码已生效。

### 9.2 常规 Main Key 轮换：只重封 EDK

目标是后续不再使用旧 Main Key，不声称修复已泄露的 DEK。过程允许读取，冻结持久配置变更和垃圾回收；操作数量及进度有界并可取消/续跑。

1. 重新验证管理资格及所需 provider。密钥环有密码 slot 时需要当前恢复密码，保证新密钥环能在所有保留的 slot 下重新封装；不能因无人提供密码就悄悄丢掉恢复方式。
2. 生成新 Main Key，原子持久化包含旧/新两把 key 的密钥环，记录轮换状态。持久化确认前不产生只依赖新 key 的文件。
3. 将新 key 设为活动封装 key；逐个重封 catalog 当前引用的 profile、迁移对象及 catalog 自身。每个文件先完成原封装与正文认证，再保留 D/data nonce/C，更新 Main Key ID、wrap nonce、EDK，原子替换整份文件。
4. 旧代际/孤儿密文在轮换清单中逐项清理或重封；清理失败保留旧 key 和 ROTATING 状态，不能被垃圾回收扫描漏掉。
5. 再扫描所有受控活动对象，确认没有依赖旧 key 的文件，认证验证全部通过；随后原子保存只含新 key 的密钥环，标记完成并恢复写入。

轮换中崩溃时，两把 key 都已保存；重启先识别文件 Main Key ID，已重封文件跳过，未完成继续。扫描以认证 catalog、计划与受控布局为准，不信任单独的“完成计数”。不得先删旧 key 再重封。

验收应逐字节比较轮换前后正文密文 C 及 D 完全相同，仅封装区域变化。由于 EDK 与正文同文件，物理写入仍可能替换整份文件；“无需重加密正文”不等于零正文 I/O 或 O(1) 操作。

### 9.3 泄露处置：换 Main Key、DEK 和正文代际

若 Main Key 或 DEK 已泄露，旧 EDK 可被解出，常规重封不能撤销攻击者已经获得的 DEK。选择 `reencrypt` 模式，使用新 Main Key，对受影响对象生成新 DEK、nonce 和 Generation ID，按正常代际保存协议逐条提交。目录索引及其他敏感元数据也更换 DEK。

该模式冻结变更，保持旧/新 key 和可续跑计划，完成所有对象校验与受控清理后才能退役旧 key。旧备份、快照及已泄露明文仍可被原密钥解开；SSH 密码/私钥等真实凭据的撤销与更换需另行执行，存储轮换不能替代目标主机上的凭据轮换。

## 10. 导入导出与权限集成

- 保持 `TERMCP-ENC 1` 导出格式和独立文件密码兼容；不向导出文件复制本机 Main Key、EDK、provider slot、Store ID 或授权身份。
- 明文导出与密码加密导出均先检查 `profile.export`；原文读取检查 `profile.credentials.read`；普通 list/get 仅返回安全摘要。加密文件密码由用户决定，不作为获得导出权限的证明。
- Agent 从本期权限目录中不能获授上述凭据读权和配置存储管理权。凭据设置/临时注入只写不读，按可信 human/Agent 上下文、主机范围及独立动作授权。
- 导入文件先完整认证和校验，再按本机规则分配新 Profile ID、DEK 和 EDK；不能复用导入端的存储身份。现有冲突自动命名与大小限制保持。
- SSH 建连在获准执行后，由内部 Store 解密并生成现有 `RemoteSSH` 参数；不把 DEK/Main Key 放进 MCP 参数、结果 DTO 或会话元数据。普通摘要同时排除 inline private key、代理认证、递归跳板秘密。
- 配置正文不会自动进入会话日志；但命令主动打印凭据、终端回显、未加密扩展模板及宿主机 shell 历史仍是独立路径。`secrets` 的日志记录控制继续独立存在。

命令解耦阶段保持其“行为不变”目标；本方案作为独立后续阶段变更磁盘格式与访问行为。如果统一 Command/Executor 已落地，在其 profile/连接命令内接入 Store，不在每个 WebUI/MCP 适配器复制解密代码。人/Agent 可信识别及凭据读权限收紧必须在加密管理功能对外启用前交付。

## 11. 分阶段交付

下列阶段是工作拆分，不自动创建 Issue、commit 或 push。未完成整条闭环前，不把单独的加密原语标为用户功能已完成。

| 阶段 | 具体改动 | 完成标准 |
| --- | --- | --- |
| P0 合同与准入 | 冻结格式/golden、状态/错误；确认可信人端管理身份及安全摘要权限接入；列出原文与导出调用点 | 格式合同与敏感读取清单已冻结；可信人端/Agent 鉴权仍待完成，因此管理路由尚未上线 |
| P1 加密核心 | envelope、provider slot、KeyRing、平台文件锁及原子持久化 | 已实现双层认证、跨对象替换、畸形输入、密钥错误及持久化核心测试；三平台真实 Provider 验收未完成 |
| P2 Store 闭环 | 私有后端 Interface、catalog、单条代际、稳定 Profile ID、临时/internal 适配 | 已实现创建→重启→读取/改名/删除、raw TOML 保留、临时/internal 分支；复制沿用现有读后写路径 |
| P3 迁移与启动 | 状态机、明文迁移、清理续跑、组合根及扩展延迟启动 | 已实现页面主动启用、逐主机转换/还原与启动自动探测；断点转换日志和清理重试状态机尚未完成 |
| P4 人端管理 | REST、CLI、WebUI 解锁/恢复密码/方式选择；日志、传输与频率限制 | 已实现配置加密 WebUI、状态/开关/逐主机转换 REST 与本地 `status` CLI；细粒度人/Agent 鉴权仍是上线前置条件 |
| P5 轮换与恢复 | 改密码、Main Key rewrap、reencrypt、并发冻结及断点恢复 | 未实现 |
| P6 发布验收 | 三系统原生 provider、升级/备份说明、Unreleased、文档与嵌入资产同步 | 当前只完成本地 race、跨平台 compile 与 focused tests；发布验收未完成 |

P0 的身份与授权工作可复用权限阶段，不能用配置加密代替；P1/P2 可先开发测试，但管理入口不得提前以无身份或共用越权模型上线。P2/P3 共同完成存储转换；没有迁移路径时不对既有目录自动启用新后端。

## 12. 测试与验收矩阵

| 场景 | 必须证明的结果 |
| --- | --- |
| 同配置连续保存、不同配置相同内容 | DEK/nonce/Generation ID 独立，密文不同；原文可完整恢复 |
| 修改稳定头、EDK、nonce、Main Key ID、正文或拼接不同文件 | 认证失败；不输出部分明文 |
| 超限/截断/未知版本/重复 slot/畸形 KDF 参数 | 有界拒绝，无 panic、超额内存分配或运行昂贵 KDF |
| 保存每个持久化边界故障 | 已提交索引始终指向完整代际；不会确认未持久化成功 |
| 目录同步失败、Windows 文件占用与替换冲突 | 错误与内存发布状态一致，重试不破坏旧数据 |
| 并发读写、相同名大小写冲突、并发导入/改名/删除 | 稳定 ID、CAS、通知及提交结果一致，无 callback 自锁 |
| 临时项与两种模式切换 | 新临时数据不落盘；转持久失败不丢临时项；进程退出不恢复临时项 |
| internal 覆盖损坏、丢失 | 明确失败，不能丢弃审阅设置回退默认 |
| 多级跳板、代理认证、未知 TOML 字段 | 迁移按原字节保留，摘要不泄露秘密 |
| 迁移第 1 至 7 步任意中断、重复执行、源被修改 | 可续跑、幂等、没有重复身份或误删；清理未完成不 READY |
| 缺密钥/坏 catalog、目录只有残留 objects | 不自动创建替代 key，不返回空主机列表假成功 |
| 改密码失败及切换 provider 失败 | 旧方式仍可用；成功时 Main Key、EDK、正文均不改 |
| 轮换中断与恢复 | 两把 key 可读混合封装；旧 key 到最后才退役 |
| rewrap 与 reencrypt 对照 | 前者 D/C 完全一致；后者 DEK/代际/正文密文改变 |
| 人、Agent、匿名访问每个管理/原文/导出入口 | 身份与能力检查先执行；MCP/REST/WS 无换门绕过 |
| 存储未就绪、daemon 等待、stdio 协议 | 不自动发起 SSH；历史保持原格式；空闲倒计时不打断初始化；stdout 无杂讯 |
| 全数据目录与故障遗留文件检查 | 合成测试秘密不出现在新配置存储的明文文件或日志；测试不创建会话输出后宣称“全目录零明文” |

测试用合成字符串，不放真实主机、账户、凭据或私钥。所有隔离目录使用 `t.TempDir()`；fake provider 仅覆盖编排，三平台另以隔离的真实系统后端验证跨进程重启、错误密码、托管丢失和恢复。故障点用私有 Adapter 注入或受控子进程退出，不用固定 sleep 猜持久化时刻。

代码交付时运行：

```sh
go test ./... -count=1 -shuffle=on -race -timeout 600s
make test
make test-stress STRESS_PKGS='./internal/configstore ./internal/sshconfig ./internal/webui'
go build ./...
go build -tags no_webui ./...
```

上列新 package 路径在实现后才存在。CI 的完整 race 测试继续遵循届时 `.github/workflows/test.yml`（当前 runner timeout 为 1800s）；本地基线命令为 600s，不把本机通过冒充三系统通过。配置变更影响建连/扩展生命周期时，将对应 package 纳入压力测试。

## 13. 升级、备份和回退

- 上线前说明磁盘格式变化、停止旧实例、备份及所需解锁方式；由人确认备份可恢复后迁移真实目录。
- 新备份必须同时保留 `config.keys`、catalog、所有活动代际及未完成迁移/轮换状态；系统托管或 ENV 方式还需要对应独立恢复能力。密码恢复 slot 可用于跨系统恢复，但不把 ENV KEK 与数据备份混放。
- 备份通过 Store 一致性快照或停止实例获得，不能边写边随意复制文件树。恢复时在新目录验证全部活动引用；托管项缺失且无恢复方式时明确不能解密。
- 回退到旧二进制只能在隔离目录恢复迁移前备份，或由已授权的人显式导出；不能让旧版直接打开新目录继续写 TOML。
- `CHANGELOG.md` 的 Unreleased 在发布前写入用户可见变化和升级条件；本设计文件本身不冒充已实现特性，不执行发布或提交操作。

## 14. 参考依据

- [Google Cloud：Envelope encryption](https://docs.cloud.google.com/kms/docs/envelope-encryption)：DEK 与封装密钥分工、DEK 密文与正文一起保存。
- [AWS Encryption SDK concepts](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/concepts.html)：EDK、封装密钥、加密上下文；本方案不引入 AWS SDK 或云端依赖。
- [Go XChaCha20-Poly1305](https://pkg.go.dev/golang.org/x/crypto/chacha20poly1305)、[Argon2id](https://pkg.go.dev/golang.org/x/crypto/argon2)：密码学原语的正式 Interface。
- [接口现状与敏感读取清单](interface-inventory.md)、[Agent 权限设计](agent-permissions.md)、[测试规范](../agents/testing.md)。
