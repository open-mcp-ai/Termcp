# Web UI 前端大改：需求与决策记录（RFC）

> 本文件记录"大改前端"的完整决策过程，作为开发计划与验收依据。
> 对应 Issue/阶段：见 GitHub issues（阶段 0 = 输出字节偏移，先行）。
> 工程原则：**零 node / 零 npm / 零 bundler**，原生 JS + Go 侧内容哈希缓存。

## 一、定位与目标

- Termcp 的 Web UI 是一个 **H5 应用**（无"页面"概念、无路由），是平台四入口之一（Web UI / MCP / SKILLS / REST）。
- 需满足三种形态：**桌面浏览器（PC）、平板、手机**。
- 最终可**对外分发 SDK**，供二开者在自己前端里嵌入终端（宿主注入 xterm.js）。

### 三大硬性要求（用户定死）

1. **模块分治**：优化缓存——只下载变动的 js/css；`index.html` 保持轻；js 可分发给其他项目做 H5 内嵌会话。
2. **响应式布局**：手机端浏览器与各分辨率浏览器高兼容；明确不兼容的列出（见 §6）。
3. **功能丰富且简洁，不引入前端框架**；功能数量不变。

## 二、已定架构决策

### 2.1 模块与加载（零 node）

- **不引入** node / npm / bundler / TypeScript 产物；Go 单二进制 + embed 资产不变。
- 原生 `<script src>` 多文件 + **单一全局命名空间 `window.Termcp`**（不使用 ESM import，保微信 WebView 兼容）。
- 每个模块：`(function (Termcp) { ... })(window.Termcp)`，依赖显式参数化。
- 组装点只有一个：`index.html` 末尾的 `Termcp.boot(...)`。
- **缓存**：Go 启动时对每个 js/css 计算 sha256 → 生成 `index.html` 引用 `/static/js/app.<hash>.js` 等 → 哈希文件 `Cache-Control: immutable`，`index.html` `no-cache`；gzip 预压缩。**哈希粒度按文件独立**（`app` / `core` / `transport` / `terminal-view` / `tools-panel` / `xterm`），改一行只重拉对应文件。
- xterm.js 也纳入同一哈希机制（vendored UMD，几乎永不变化）。

### 2.2 兼容性基线（已定）

- **一级支持**（完整体验）：桌面 Chrome/Edge 66+、Firefox 69+、Safari 13.1+；安卓 Chrome 66+；iOS Safari 13.1+；微信 WebView（iOS 跟随系统；安卓 Chromium 66+）。
- **二级支持**（可用但降级）：微信安卓 X5 老内核（浮动/平铺禁用）；平板（全功能按断点）；折叠屏。
- **明确不支持**（写进 README）：IE 11 及以下；Safari < 13.1（无 ResizeObserver）；Android WebView < 66；不支持 `fetch` / `Promise` / `ResizeObserver` 的环境。

### 2.3 代码分层（两套 DOM，一套 core）

```
core/            纯逻辑，零 DOM：transport（REST/WS）、会话状态管理、消息分发、xterm 实例管理
  ├── layout-pc.js       PC 外壳：浮动窗口、拖拽、标签栏（现有代码迁入）
  └── layout-mobile.js   手机外壳：全屏层、底部 shell tab、会话切换器、左侧抽屉
```

- 布局 DOM **分开写**；WS/REST/终端渲染/历史回放逻辑**共用**（不复制渲染代码、不复制状态管理）。
- SDK 抽取：只嵌"单会话终端视图"；xterm 由宿主注入；CSS 自包含且可加前缀；最后阶段实现。

### 2.4 已确认的手机端交互模型

| 主题 | 决策 |
|---|---|
| 页面模型 | 无路由、无"页面"；单页 + 全屏覆盖层 |
| 首页空闲态 | 左上 `[☰]` 展开 entries（**左侧划入、露出右侧部分、层叠效果**）；主区是 sessions |
| 打开终端 | 点 session / 连接 entry → **全屏终端层**（覆盖首页） |
| 终端页顶部 | `[←] 标题 [工具▾] [×]`（无 Maximize；`←`=最小化回首页，`×`=关闭 session） |
| shell tab | **底部**（位于 iOS safe-area 之上，不触发系统手势）；空闲时**不收起**（一直显示） |
| 会话切换器 | 终端态左上 `[≡]` → 弹出所有 session（含 DEAD）列表；切换**无缝**（见 §2.5） |
| 首页 sessions | **大卡片 + 分组列表**；DEAD 排序放到后面；卡片动作按钮直接放卡片上方 |
| entries 抽 抽屉 | 展开后左侧划入，露出会话区一部分作为层叠效果；动作（编辑/复制URL/启动选项）放在卡片上 |
| 关闭语义 | 复用现有三态：shell tab `×`=关闭 shell（终端页变空 session）；窗口 `×`=关闭 session 回首页；`←`=最小化（进程继续） |
| 工具面板 | 手机端 `[工具▾]` 弹出（Forwardings / Files / Terminal 切换） |

### 2.5 历史加载与滚动（手机端与 PC 统一）

- **按需加载**：打开/切换会话只灌最后一屏多一点（约 150–200 行），秒开。
- **直读服务器**：前端只持**绝对字节偏移游标**，数据永远从服务器 `output-range` 读；前端不做长历史缓存（避免内存爆炸）。
- **渲染：3 个 xterm 环形轮换，没有独立的“屏幕区”。**
  三个实例等价于一个终端：滚到最顶 → 切到下一个（更早内容），滚到最底 → 也切到下一个。
  不再需要一个常驻的 live 单例接收 WS 流。
- **上滚加载**：滚到边界 → 从服务器拉相邻段；**不设“点击加载更多”按钮**（滚动即加载）。
- **scrollback**：`Terminal({ scrollback })` 设小值，靠按需加载保证“历史无限、内存有界”。
- **已知约束**：xterm 的 buffer 是环形列表，只能追加、不能前插；
  且 `write()` 是流式解析器，从任意字节开始会丢掉 ANSI 状态（TUI 段尤其明显）。
  所以每个段必须**从自身起点起解析**，段边界选在有意义的边界上。
- **服务端前置**（已完成）：见 `docs/design/session-storage.md`。

> **状态**：本节是设计决策，**尚未实现**。当前代码是 1 个 xterm + 全量灌入
> （`bootstrapShellFullHistory`，32 MiB 显示上限），见阶段 5。

### 2.6 待继续确认的开放项

- [x] 移动端工具面板（Forwardings/Files）的具体形态（`[工具▾]` 弹出后如何呈现）——**结论：不再需要独立面板**。
      Forwardings/Files 已随终端窗口本身提供（窗口内 `.shell-tab-bar` 的 term/fw/file/notify 四个页签，触屏与桌面一致），
      原先为此预留的 `#panel-tools` 悬浮窗口从未有触发入口（`openToolPanel` 无调用点），已删除；
      `tools-panel.js` 只剩共享的转发弹窗逻辑，更名为 `forward-modal.js`。
- [ ] 平板（横屏 768–1024）的布局断点与浮动窗口策略
- [x] 卡片 hover-only 按钮的移动端替代（长按菜单）——**结论：不再需要**。卡片动作已改为直接作用在对象上
      （名字=重命名、sid=复制，各自 `role=button` + Enter/Space），不依赖 hover，触屏可直接点。
- [ ] 已关闭会话回放视图在移动端的呈现
- [ ] SDK 提取的具体 API 形态（最后阶段）
- [ ] `notify_user` 在移动端的呈现（浏览器级通知，不做会话定位）

## 三、开发阶段（提议）

| 阶段 | 内容 | 状态 |
|---|---|---|
| **0** | 输出字节偏移坐标系 + 字节安全存储（见 `docs/design/session-storage.md`） | ✅ 已完成 |
| 1 | 模块拆分 + Go 侧哈希缓存（`window.Termcp` 命名空间、多 js/css、immutable） | ✅ 拆分已完成（`c87c157`，index.html 252 KB → 21 KB，8 个模块 + `app.css`）；**Go 侧哈希缓存/immutable 未做** |
| 2 | 数据层/core 抽取（transport、会话状态） | 部分完成：transport 在 `ui-socket.js`、会话状态在 `sessions.js`；未抽成独立 core 层 |
| 3 | PC 外壳迁入 layout-pc.js（保真，行为不变） | 未做（按功能域拆分，未按 PC/手机外壳分文件） |
| 4 | 手机外壳 layout-mobile.js（全屏层、左侧抽屉、底部 shell tab、会话切换器） | ✅ 已完成（行为层面；代码未单独成 layout-mobile.js） |
| 5 | 历史按需加载 + 3-xterm 轮换（PC/手机统一） | 待定（用户明确后置） |
| 6 | 测试与回归（见 §五） | 部分完成：`assetsplit_test.go`、`session_switch_test.go` 覆盖拆分不变量与触屏交互；6 视口实测通过 |
| 7 | SDK 提取（宿主注入 xterm） | 最后 |
| 8 | 文档（README 兼容矩阵、移动端截图） | 部分完成：新增 `docs/design/mobile-terminal.md`；**README 兼容矩阵与移动端截图未做** |

## 四、验收标准（草案）

- [ ] 浏览器刷新：`index.html` 重取（no-cache），js/css 全部命中缓存（immutable）
- [ ] 修改一个模块后：只有该模块重拉
- [ ] PC 上原功能全部可操作（浮动/平铺/拖拽/右键/工具面板/回放）
- [ ] 手机上：首页 sessions 大卡片分组 → 点入全屏终端 → 底部 shell tab 切换 → 上滚按需加载历史 → `[≡]` 切换会话 → `[←]` 回首页
- [ ] 微信 WebView / iOS Safari 13.1+ / Chrome 66+ 三端冒烟
- [ ] 无 console 错误
