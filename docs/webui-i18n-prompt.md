# 任务：Termcp Web UI 多语言（简体中文 / 繁体中文 / English）

> 这是一份**可独立执行的交付提示词**。执行者不需要参与过前期设计讨论，所有决策已定，
> 不要重新讨论方案、不要扩大范围。有疑问先提问，不要自行发挥。

---

## 0. 交付目标

给 Termcp 的浏览器 Web UI 加三语言支持，满足三条硬性要求：

1. **自动检测浏览器语言** —— 用户不做任何操作时，界面语言跟随浏览器
2. **可手动设置语言** —— 用户可覆盖，选择被记住
3. **简繁英三语** —— `zh-Hans` / `zh-Hant` / `en`

**必须做到**：切换语言时**页面不刷新、已打开的终端窗口不关闭**，全站文案立即切换。

---

## 1. 项目背景（必读，决定实现形态）

Termcp 是一个 Go 单二进制的终端平台。Web UI 是 `internal/webui/assets/` 下的静态资源，
通过 `//go:embed assets`（`internal/webui/handler.go:28`）编进二进制。

### 1.1 前端的硬性约束（违反会导致测试红或不兼容微信 WebView）

| 约束 | 原因 | 有测试强制 |
|---|---|---|
| **零 node / 零 npm / 零 bundler / 零框架** | 项目 RFC 定死，`docs/design/webui-redesign.md §2.1` | — |
| 模块是**普通 `<script src>`**，共享同一个全局作用域 | 兼容老 WebView，不用 ESM | ✅ |
| **禁止 IIFE 包裹**模块 | 会把 `var`/`function` 重新私有化，其他模块看不见 | ✅ |
| **禁止顶层 `let` / `const`**，一律 `var` | script 作用域的绑定对其他模块不可见 | ✅ |
| **禁止顶层重名声明** | 后加载的模块会静默遮蔽前一个 | ✅ |
| **新增 JS/CSS 文件必须在 `index.html` 里被引用** | 未被引用的模块 = 死代码 | ✅ |
| 模块**加载顺序**有意义，不能随意调整 | 后者在 load 期就调用前者的函数 | ✅ |
| `index.html` 必须 < 64 KiB | 它每次加载都不缓存，模块才缓存 | ✅ |
| 不新增任何第三方依赖 | 同上 | — |

以上约束由 `internal/webui/assetsplit_test.go` 自动校验。**改动前先读这个文件**，
它同时是本仓库"前端测试"的范式参考。

### 1.2 当前前端文件

```
internal/webui/assets/
  index.html                     200 行，页面骨架 + 所有弹窗的静态标记
  api.html                       395 行，API 文档页（本次不动）
  static/css/app.css            1262 行
  static/js/util.js              319 行   通用工具、通知条、资源 URL
  static/js/shell-windows.js     805 行   浮动窗口、标签栏、平铺
  static/js/forward-modal.js      79 行   端口转发弹窗
  static/js/approval.js          197 行   审批/复核队列
  static/js/sessions.js          474 行   会话卡片网格
  static/js/ui-socket.js         718 行   WebSocket、终端数据流
  static/js/terminal-view.js    1500 行   终端窗口本体（最大，文案最多）
  static/js/dialogs.js           228 行   连接卡片网格、确认框
  static/js/conn-form.js         766 行   连接编辑器、启动弹窗、boot
  static/xterm/                  第三方 xterm.js（不动）
```

加载顺序（`index.html` 末尾，**新模块要插进去**）：

```html
<script src="static/xterm/xterm.js"></script>
<script src="static/js/util.js"></script>
<script src="static/js/shell-windows.js"></script>
<script src="static/js/forward-modal.js"></script>
<script src="static/js/approval.js"></script>
<script src="static/js/sessions.js"></script>
<script src="static/js/ui-socket.js"></script>
<script src="static/js/terminal-view.js"></script>
<script src="static/js/dialogs.js"></script>
<script src="static/js/conn-form.js"></script>
```

### 1.3 动手前必须先做的事

工作区可能有**未提交的在途改动**：

```bash
git status --short
git diff --stat
go test -count=1 ./...        # 必须先全绿，否则后面分不清是谁的锅
```

基线不绿就先停下报告，不要边改边查。

---

## 2. 语言解析规则（定死，不要改）

```
优先级（高 → 低）：
  1. localStorage['termcp.lang']  ∈ { "auto", "en", "zh-Hans", "zh-Hant" }
     缺省值 = "auto"
  2. "auto" 时按 navigator.languages 数组顺序匹配第一个命中的：
       /^zh-(hant|tw|hk|mo)/i   → zh-Hant
       /^zh/i                   → zh-Hans
       /^en/i                   → en
  3. 都没命中 → en
```

**要点**：

- 必须读 **`navigator.languages`（数组）**，不是 `navigator.language`（单值）。
  港台浏览器的首位常常是 `en-US`，只看单值会把用户全判成英文。
- 必须**按数组顺序**，第一个命中的胜出，不要遍历完再"优先中文"。
- `"auto"` 必须是存储里的合法值。"用户可自己设置语言"包含"恢复跟随浏览器"，
  否则用户手动选过简中之后就再也回不到自动。

**不做**：`?lang=` URL 参数、服务端 `Accept-Language` 协商、Cookie。

---

## 3. 文件改动清单

### 3.1 新增

| 文件 | 内容 | 估计行数 |
|---|---|---|
| `internal/webui/assets/static/js/i18n.js` | 引擎：语言解析、`t()`、`applyI18n()`、切换、重放注册表 | ~80 |
| `internal/webui/assets/static/js/i18n-catalog.js` | 数据：`var I18N_CATALOG = { en:{…}, "zh-Hans":{…}, "zh-Hant":{…} }` | ~350 |
| `internal/webui/i18n_test.go` | 静态断言（见 §8 L1/L2） | ~180 |

**为什么词典是同步 `<script>` 而不是 `fetch` JSON**：
3 个 JSON = 3 次额外网络往返 + 首屏异步竞态 + 未翻译闪烁（FOUC）。
同步加载的 `var` 在同一段脚本执行里就把静态 DOM 换完，首帧即正确语言，
并且不破坏"零 bundler"约束。**不要改成 fetch，不要改成 JSON 文件。**

### 3.2 修改

| 文件 | 改什么 |
|---|---|
| `index.html` | 77 处加 `data-i18n*` 标记；header 加语言选择器；插入 2 个 `<script>` |
| `static/css/app.css` | +~10 行：`.lang-select` 样式、`.lang-hant` 字体栈 |
| `static/js/util.js` | 3 处文案（`showCopyToast` 默认值、`setLoadBanner` 的 Dismiss、通知关闭按钮） |
| `static/js/dialogs.js` | 连接卡片网格全部文案 + 确认框默认值 + **新增 `window._lastConnections` 缓存** |
| `static/js/sessions.js` | 会话卡片网格 ~33 处 |
| `static/js/shell-windows.js` | 6 个 tooltip + 4 个 `<option>` |
| `static/js/terminal-view.js` | **最大头**：37 处内联 `title=`、8 处原生 `prompt/confirm/alert`、面板标题、列表空态；**新增 `reapplyWindowLanguage()`** |
| `static/js/approval.js` | 队列文案 ~6 处 |
| `static/js/conn-form.js` | 表单校验 ~12 处 + toast ~8 处 + 弹窗标题 |
| `static/js/forward-modal.js` | 校验与提示 ~8 处 |

### 3.3 明确不动

- ❌ `internal/webui/assets/api.html`（本次不翻）
- ❌ `assets/api.md`、`assets/skills.md`（面向 agent，不翻）
- ❌ 任何 Go 业务逻辑（`handler.go` / `approval.go` 的 `http.Error(...)` 英文诊断信息保持原样透传）
- ❌ `static/xterm/*`（第三方）
- ❌ `escapeHtml()` 的行为、`formatSize()` 的单位、`fmtTime()`（已用 `toLocaleString()`，本就跟随浏览器）

---

## 4. 引擎契约

### 4.1 `i18n-catalog.js`

```js
/* 三份词典。key 为扁平点号命名，值为译文。
   en 是权威集合：任何 key 必须三语齐全（有测试强制）。 */
var I18N_CATALOG = {
  en: {
    'sec.entries': 'Entries',
    'session.delete.title': 'Delete session',
    'session.delete.message': 'Delete "{name}"?',
    // …
  },
  'zh-Hans': { /* … */ },
  'zh-Hant': { /* … */ }
};
```

**禁止**在 catalog 里写逻辑、写 IIFE、写 `let/const`。

### 4.2 `i18n.js`

必须导出以下全局（顶层 `var` / `function`，无 IIFE）：

```js
/* 当前生效语言，形如 'en' | 'zh-Hans' | 'zh-Hant' */
var _termcpLang;

/* 取译文。缺 key 依次回退：当前语言 → en → 返回 key 本身。
   params 里的 {name} 占位符做插值，缺失的占位符原样保留。 */
function t(key, params) { … }

/* 把 data-i18n* 标记的静态 DOM 刷成当前语言。root 默认 document。 */
function applyI18n(root) { … }

/* 注册"语言变化后需要重放"的回调。见 §6。 */
function onLangChange(fn) { … }

/* 语言选择器 onchange 的入口。value ∈ {auto,en,zh-Hans,zh-Hant} */
function termcpApplyLang(value) { … }

/* 只解析不写入：把 localStorage/浏览器偏好算成一个具体语言码 */
function resolveLang(stored) { … }
```

`applyI18n` 支持的属性（**只这四个，不要扩**）：

| 属性 | 写入目标 |
|---|---|
| `data-i18n="key"` | `textContent` |
| `data-i18n-title="key"` | `title` |
| `data-i18n-placeholder="key"` | `placeholder` |
| `data-i18n-aria="key"` | `aria-label` |

**不做**：`data-i18n-html`（XSS 面）、MutationObserver（动态路径显式调 `t()` 即可）、
`t()` 之外的复数/性别规则。

**插值安全顺序**：先 `t()` 拿整句，再 `escapeHtml()`，再进 `innerHTML`。别反。
`textContent` 路径不要 escape。

**缺 key 回退到返回 key 本身**——让错别字在界面上一眼可见，比静默空白好查。

### 4.3 `termcpApplyLang(value)` 必须做的事（顺序固定）

```js
function termcpApplyLang(value) {
  // 1. 存储（try/catch：隐私模式下 localStorage 会抛）
  try { localStorage.setItem('termcp.lang', value); } catch (e) {}
  // 2. 重新解析并写入 _termcpLang
  _termcpLang = resolveLang(value);
  // 3. <html lang> + body 字体标记
  document.documentElement.setAttribute('lang', _termcpLang);
  document.body.classList.toggle('lang-hant', _termcpLang === 'zh-Hant');
  // 4. 静态 DOM
  applyI18n(document);
  // 5. 动态 DOM：重放所有注册的回调（见 §6）
  _langReappliers.forEach(function (fn) {
    try { fn(); } catch (e) { console.error(e); }
  });
}
```

第 5 步的 `try/catch` 不能省：一个模块重放失败不能连累其余模块。

### 4.4 加载顺序（有硬约束）

`index.html` 里插成：

```html
<script src="static/xterm/xterm.js"></script>
<script src="static/js/i18n-catalog.js"></script>   <!-- 新增：数据 -->
<script src="static/js/i18n.js"></script>           <!-- 新增：引擎 -->
<script src="static/js/util.js"></script>
…
```

- 两个新模块必须排在 **`xterm.js` 之后、`util.js` 之前**。
- `i18n.js` 在自身 load 结尾就要执行一次初始化（`applyI18n(document)` +
  设置 `<html lang>`），此时 body 已解析完，可以安全操作 DOM。
- `conn-form.js`（最后一个模块）在 load 期就调用 `loadConnections()`，所以词典必须在那之前就位。
- **不要**把初始化放到 `DOMContentLoaded` 里再多一层异步——那会引入首帧闪烁。

---

## 5. 语言选择器 UI

在 `index.html` 的 `<nav class="app-header-nav">` 里、API 链接**之前**插入：

```html
<select id="lang-select" class="lang-select" onchange="termcpApplyLang(this.value)">
  <option value="auto">自动</option>
  <option value="zh-Hans">简体中文</option>
  <option value="zh-Hant">繁體中文</option>
  <option value="en">English</option>
</select>
```

要点：

- **原生 `<select>`**。零依赖、手机上是系统原生选择器、无障碍免费。不要自定义下拉。
- 四个选项的显示文字**不翻译**（三种语言名用各自语言书写是惯例）；
  只有 `自动` 走词典（`lang.auto`）。
- 选中项由 JS 初始化时设置：`document.getElementById('lang-select').value = <stored>`,
  其中 stored 是**用户存储的原始值**（可能是 `"auto"`），不是解析后的语言码。
- 中文环境下 `font-family` 用系统栈，不要引入 webfont。

CSS（`app.css`）：

```css
.lang-select {
  font: inherit; font-size: 0.82rem; padding: 6px 8px; border-radius: 6px;
  border: 1px solid #d0d7de; background: #fff; color: #24292f; cursor: pointer;
}
/* 繁体字形靠系统 locale 猜，显式指定更稳 */
body.lang-hant { font-family: ui-sans-serif, system-ui, "PingFang TC", "Microsoft JhengHei", sans-serif; }
```

`.lang-select` 要适配 `@media (pointer: coarse)`（触屏加高到 ~34px）。

---

## 6. 响应式更新（本任务的核心难点）

**目标**：切换语言时**不刷新页面、不重建窗口 DOM**，全站文案立即变。

### 6.1 为什么不能重建窗口 DOM

`terminal-view.js` 里所有事件监听器都直接挂在窗口内部的子元素上
（`win.querySelector('.shell-window-max-btn').addEventListener(...)` 这类），
并且 `win._channels` 里持有活动的 xterm 实例。
一旦 `win.innerHTML = …` 重刷，监听器全部丢失、xterm 实例悬空、终端数据流断掉。

**所以：只重放属性与文本，绝不重建结构。**

### 6.2 重放注册表

`i18n.js` 里：

```js
var _langReappliers = [];
function onLangChange(fn) { _langReappliers.push(fn); }
```

各模块在**自身 load 结尾**注册：

| 文件 | 注册内容 | 说明 |
|---|---|---|
| `sessions.js` | `renderSessionGrid(window._lastSessionsSnapshot \|\| [], '')` | 用内存快照重渲染，**不发请求** |
| `dialogs.js` | `renderConnGrid(window._lastConnections \|\| [], '')` | 需要先补 `window._lastConnections` 缓存（现在没存） |
| `shell-windows.js` | `updateTileToggleButton(); updateSessionTabbarToggle(); refreshSessionTabbar();` | 已有函数，幂等 |
| `terminal-view.js` | `allShellWins().forEach(reapplyWindowLanguage);` | 需新增函数，见下 |
| `approval.js` | `applyApprovalCounts();` | 已有函数，幂等；**不要**在这里重拉队列（会与 `loadReviewQueue` 循环） |
| `forward-modal.js` / `conn-form.js` | 无需注册（`applyI18n(document)` 已覆盖其静态弹窗） | 若校验错误正显示，会保持旧语言直到下次触发——可接受 |

**必须使用内存快照而不是重新 `fetch`**：重新请求会造成列表闪烁，
且离线/断线时会把列表清空。

### 6.3 `reapplyWindowLanguage(win)`

在 `terminal-view.js` 新增，只写属性、只重设文本：

```js
function reapplyWindowLanguage(win) {
  if (!win) return;
  // 1) 重放已有的幂等函数（副作用传回当前值，等价 no-op）
  applyApprovalMode(win, !!win._approvalMode);   // 自读自写，安全
  updateTermScrollButton(win);
  // 2) 面板内的固定标题（它们在 innerHTML 模板 SHELL_WINDOW_PANELS_HTML 里）
  var fw = win.querySelector('[data-stab="fw"] > div > span');
  if (fw) fw.textContent = t('panel.forwardings');
  var file = win.querySelector('[data-stab="file"] > div');
  if (file) file.textContent = t('panel.files');
  var ntf = win.querySelector('[data-stab="notify"] > div > span');
  if (ntf) ntf.textContent = t('panel.notifications');
  // 3) 重扫窗口内所有 data-i18n* 标记（模板里那 37 处 title/api-label）
  applyI18n(win);
  // 4) 列表重渲染（从全局缓存读，不发请求）
  if (win._refreshFwList) win._refreshFwList();
  if (win._refreshNtfList) win._refreshNtfList();
  if (win._shellFileBrowse) win._shellFileBrowse();
}
```

配套改动：

- 在 `_initShellWindowUI`（或文件浏览初始化处）加一行 `win._shellFileBrowse = shellFileBrowse;`
- `_refreshFwList` / `_refreshNtfList` 已经存在（`win._refreshFwList = function() {…}`），直接用。

**第 3 步是覆盖率的关键**：把 `terminal-view.js` 里 37 处内联 `title="Copy URL"`
改成 `title="Copy URL" data-i18n-title="win.copyUrl"`，
`applyI18n(win)` 一次全刷，不必逐个字段硬编码。

### 6.4 已知且可接受的残留

在代码注释里用一行说明（`ponytail:` 风格），不要试图消灭它们：

| 残留 | 原因 |
|---|---|
| 切换瞬间正显示的**原生 `prompt/confirm/alert`** | 浏览器系统弹窗，阻塞中用户不可能操作选择器 |
| 已打开的**会话切换菜单** `.shell-switch-menu` | 每次 `createElement` 重建，关掉重开即新语言 |
| 切换瞬间正显示的**表单校验错误行** | 下次触发即新语言 |

**这些都不需要额外处理，也不需要为此写代码。**

---

## 7. key 命名与术语表

### 7.1 key 规范

扁平点号，形如 `域.元素.属性`，全小写 + 驼峰段：

```
sec.entries                    区域标题
section.batch.selectAll        按钮
modal.forward.title            弹窗标题
modal.conn.field.host.label    表单标签
modal.conn.field.host.placeholder
toast.copy.ok                  提示条
tooltip.win.maximize           悬浮提示
msg.session.delete.message     整句消息
reason.explicit                结束原因
```

**禁止**把中文原文当 key（`t('删除会话')`）。有测试强制 key 形态。

### 7.2 整句翻译，禁止片段拼接

现有代码里有这种：

```js
// ❌ 中文语序必错
'Deleted ' + n + ' session' + (n === 1 ? '' : 's')
```

一律改成整句 + 占位符：

```js
// ✅
t('toast.session.deleted', { count: n })
```

复数只处理这三处，用**分键**而不是 ICU：

| 位置 | 分键 |
|---|---|
| `N forwards` | `fwd.count.one` / `fwd.count.other` |
| `N dead sessions` | `dead.count.one` / `dead.count.other` |
| `N selected` | `batch.count.selected`（中文无需复数，但三语必须都有这两个 key 或都不用） |

若某语言不需要区分单复数，两个 key 填同一个值即可——**三语 key 集合必须完全一致**。

### 7.3 术语表（必须严格遵守，三语一致性靠它）

| 英文 | zh-Hans | zh-Hant（通用繁中，台湾用语为主） |
|---|---|---|
| session | 会话 | 工作階段 |
| connection / entry | 连接 / 入口 | 連線 / 項目 |
| forwarding | 端口转发 | 通訊埠轉送 |
| review / approval | 复核 / 审批 | 複核 / 審批 |
| pane / tile | 窗格 / 平铺 | 窗格 / 並排 |
| dead | 已结束 | 已結束 |
| shell | shell | shell |
| pty / pipe | pty / pipe | pty / pipe |
| MCP / SSH / SOCKS5 / TOML / Termcp | 不翻 | 不翻 |
| banner / toast | 提示条 / 提示 | 提示列 / 提示 |
| Rename | 重命名 | 重新命名 |
| Upload / Download | 上传 / 下载 | 上傳 / 下載 |
| Refresh | 刷新 | 重新整理 |
| Delete | 删除 | 刪除 |
| Close | 关闭 | 關閉 |
| Cancel | 取消 | 取消 |
| Confirm | 确认 | 確認 |

**繁体必须人工写第二份，禁止用字符转换工具**：
`数据→資料`、`软件→軟體`、`网络→網路` 是**词级**差异，字符级替换必错。

---

## 8. 测试流程（五层，缺一不可）

### L1 — Go 静态断言（`internal/webui/i18n_test.go`）

复用现有的 `readAsset`（`internal/webui/shellio_test.go:229`）读取嵌入资源。
用 `package webui`（内部测试，与 `assetsplit_test.go` 同包）。

必须实现以下断言：

```go
// 1. 三语言 key 集合完全一致（双向差集为空）
func TestCatalogsHaveIdenticalKeys(t *testing.T)

// 2. index.html 里每个 data-i18n* 引用的 key 都在词典中存在
func TestIndexHTMLKeysExist(t *testing.T)

// 3. 无死键：词典每个 key 都被引用（扫 HTML 的 data-i18n* + JS 的 t('…')）
func TestNoOrphanKeys(t *testing.T)

// 4. 同一 key 各语言的 {占位符} 集合一致
func TestPlaceholdersMatchAcrossLanguages(t *testing.T)

// 5. 三种语言下 t() 都不返回 key（抽样核心 key）
func TestCoreKeysTranslated(t *testing.T)
```

**解析建议**（本仓库零 node，用 Go 正则）：

- catalog 是 JS 字面量，用正则提取 `'key': 'value'` 对，或按语言块切分后逐行解析。
  不要为此引入 JS 引擎。
- `t('…')` 调用用 `regexp.MustCompile(`\bt\(\s*'([^']+)'`)` 扫 `static/js/*.js`。
- key 形态校验：`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)+$`，用它同时排除"中文原文当 key"。

### L2 — 漏翻审计（Go，**审计型不断言失败**）

加一个测试，扫所有 `static/js/*.js` 找出"看起来像 UI 文案"的字面量：

```go
// TestUntranslatedAudit 扫出疑似未翻译的字符串，用 t.Logf 列出。
// 这是审计工具不是红灯：会有少量误报（'application/json'、'calc(100vw - 16px)'
// 这类），硬断言会逼人去堆白名单，最后白名单比代码还长。
//
// 用法：go test ./internal/webui/ -run TestUntranslatedAudit -v
func TestUntranslatedAudit(t *testing.T)
```

启发式：匹配 `'[A-Z][a-z]+ [a-z][^']*'`（首字母大写 + 空格 + 小写）形态的字面量，
排除白名单：HTTP 方法（`GET`/`POST`/…）、`Content-Type`、`application/json`、
`text/html`、事件名（`click`/`change`/…）、`className` 片段、
CSS 值（含 `px`/`vh`/`dvw`/`calc(`/`rgba(`）、`localStorage` key（`termcp`）等。

**产物是一份清单，供人核对决定是否补翻。** 不要把它改成 `t.Errorf`。

### L3 — 构建与既有测试（机器判）

```bash
go test -count=1 ./...        # 包含 assetsplit_test.go 的三个前端结构测试
go vet ./internal/webui/
go build -o /tmp/termcp-test .
```

`TestIndexHTMLReferencesOnlyExistingAssets`、`TestModulesLoadedInSourceOrder`、
`TestModuleDeclarationsReachSharedScope` 会自动校验新模块被引用、无 IIFE、
无顶层 `let/const`、无重名、顺序正确。**这三个红了不要绕过，是模块写错了。**

### L4 — CDP 端到端回归（机器判）

真实浏览器里验证"切换语言不刷新且全站生效"。**先探测环境再选实现语言。**

#### L4.1 环境探测

```bash
node --version 2>&1 | head -1
python --version 2>&1 | head -1
# Windows
ls "/c/Program Files/Google/Chrome/Application/chrome.exe" 2>&1
ls "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" 2>&1
# Linux/macOS
which google-chrome chromium chromium-browser \
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" 2>&1
```

**优先 Node**：Node 22+ 自带全局 `WebSocket` 和 `fetch`，CDP 客户端**零依赖**。
Python 需要 `websockets` 或 `websocket-client`，装了才用。
两者都没有就跳到 L5，并在报告里说明 L4 未执行。

#### L4.2 启动被测实例（隔离，不污染用户环境）

```bash
# 临时数据目录 + 独立端口。internal profile 是内置回环 SSH，不需要任何真实凭据。
go build -o /tmp/termcp-i18n .
/tmp/termcp-i18n --host 127.0.0.1 --port 18765 --data-dir /tmp/termcp-i18n-data &
```

注意：**不要**读取或写入用户的 `~/.termcp`，不要碰任何已存在的连接配置。
报告里不要粘贴任何真实主机名/用户名/密钥。

```bash
# 启动 headless Chrome 并开 CDP
"<chrome>" --headless=new --remote-debugging-port=<port> \
  --user-data-dir=/tmp/cdp-i18n --no-first-run --disable-gpu about:blank &
```

#### L4.3 断言清单（脚本内容）

用 `GET http://127.0.0.1:<cdp-port>/json/list` 找 `type == "page"` 的 target，
取其 `webSocketDebuggerUrl`，连上后依次发 CDP 命令。

**必须实现的最小 CDP 客户端**：

```
WebSocket 连接 → {id, method, params} → 按 id 匹配响应
用到的方法：
  Page.enable
  Page.navigate { url }
  Runtime.evaluate { expression, returnByValue: true }
  Console.enable  /  Runtime.consoleAPICalled 事件
  Emulation.setLocaleOverride { locale }   // 断言自动检测用
  Emulation.setDeviceMetricsOverride { width, height, deviceScaleFactor, mobile }
```

**必过断言**（失败即报告，不要静默跳过）：

| # | 场景 | 断言 |
|---|---|---|
| 1 | `localStorage.clear()` → 以 locale `zh-CN` 加载 | `<html lang>` 以 `zh-Hans` 开头；`#sec-entries span` 文本 = 简体译文 |
| 2 | 以 locale `zh-TW` 加载 | 同上但为 `zh-Hant` |
| 3 | 以 locale `en-US` 加载 | `<html lang>` = `en`；文本为英文 |
| 4 | 切语言不刷新 | 切换前在页面里打一个标记 `window.__probe = 1`，调用 `termcpApplyLang('zh-Hant')` 后断言 `window.__probe === 1`（证明未 reload）；且 `document.body.innerText` 已变 |
| 5 | 全站无残留英文（简体） | 断言若干关键元素文本等于预期简体译文（不要做"不含英文字母"的粗暴断言——`shell`/`MCP`/`SSH` 是刻意不翻的） |
| 6 | 已打开窗口跟随切换 | 先点连接卡片开会话窗口，再切语言；断言 `.shell-window` 上至少 3 个元素的 `title` 属性已变为新语言；断言窗口数量与切换前相同（**没有重建**） |
| 7 | 刷新后语言保持 | `location.reload()` 后断言 `<html lang>` 与刷新前一致 |
| 8 | 恢复自动 | `termcpApplyLang('auto')` 后 `localStorage['termcp.lang'] === 'auto'`，且语言回到浏览器语言 |
| 9 | 手机视口 | `Emulation.setDeviceMetricsOverride` 设 390×844 + `mobile:true`，重复 #4，断言无异常且切换生效 |
| 10 | **零 console error** | 全程监听 `Runtime.consoleAPICalled`，`type === 'error'` 必须为 0 |

**#6 和 #10 是这次改动最有价值的两个断言**，不要省略。

#### L4.4 脚本存放

把 CDP 脚本放在仓库**外**或放在已 gitignore 的位置，例如 `/tmp/i18n-cdp.js`。
若放在仓库根目录（如 `i18n-cdp.js`），**不要 commit**，并在报告里说明它是临时验证工具。

### L5 — 人工判据（机器判不了的）

写进报告，让人类复核：

1. **译文地道性**：简繁译文是否自然，术语是否与 §7.3 一致
2. **繁体字形**：`body.lang-hant` 字体栈在目标系统上是否真的给出繁体字形
3. **移动端观感**：三语言 × 手机视口，选择器是否好点、有无溢出错位
4. **首帧语言**：冷启动（清 localStorage）打开，语言是否正确且**无闪烁**

---

## 9. 执行顺序（按此推进，每步可独立验收）

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P0** | `i18n-catalog.js` + `i18n.js` + 选择器 + `index.html` 的 77 处标记 | 浏览器里能切语言，页面骨架全变；L1 的 #2 通过 |
| **P1** | 9 个 JS 模块的动态文案（按 §3.2 逐文件过） | L1/#3 死键检查通过；L2 审计清单为空或仅剩白名单项 |
| **P2** | `reapplyWindowLanguage` + 5 处 `onLangChange` 注册 | L4 的 #4/#6 通过 |
| **P3** | 繁体人工校对 + 术语统一 | L5 第 1、2 项 |
| **P4** | `i18n_test.go` 全部断言 + `go test ./...` 绿 | L1、L2、L3 |
| **P5** | CDP 脚本跑通 | L4 全绿 |

P0 完成后**先停下来给人看一眼效果**再继续 P1——
如果语言解析或切换机制有问题，早发现比在 200 处文案铺开之后发现便宜得多。

---

## 10. 明确不做（YAGNI，不要自行扩大范围）

- ❌ 不翻 `api.html`、`api.md`、`skills.md`
- ❌ 不做 `?lang=` URL 参数
- ❌ 不做服务端 `Accept-Language` 协商、不做 Cookie 存储
- ❌ 不翻译服务端 `http.Error(...)` 的英文诊断信息（保持原文透传，可搜索性更好）
- ❌ 不引入 i18next / ICU / 日期库 / 语言包热加载 / 外置 JSON 语言文件
- ❌ 不做 RTL、不做 `/zh/` 路径路由、不做 URL 重定向
- ❌ 不新增任何第三方依赖，不引入 node/npm/bundler
- ❌ 不引入 Playwright / Puppeteer / jsdom / 任何 headless 测试框架
- ❌ 不重构与 i18n 无关的代码
- ❌ 不改任何 Go 业务逻辑（除新增测试文件）

---

## 11. 交付要求

1. **不 commit、不 push。** 改完给出 `git diff --stat` 和改动说明，由人类确认后再提交。
2. 报告中**不得出现**：本机 hostname、本地用户名、真实主机名/IP、任何密钥 token 密码、
   邮箱手机号等个人信息。测试实例必须用临时 `--data-dir`。
3. 注释用中文或英文均可，与周边代码保持一致；**不要**在代码或 commit message 里出现
   `Co-Authored-By: Claude` 之类字样。
4. 报告需包含：
   - 新增/修改文件清单
   - L1–L5 每层的执行结果（L5 未执行要说明原因）
   - L2 审计清单全文（即使为空）
   - 新的 top-level 声明名单（证明无重名、无 `let/const`）
   - 已知未覆盖项与原因
5. 遇到与本文档冲突的实际情况：**先停下报告**，不要自行改方案。

---

## 12. 现成的参考点（省时间用）

| 你要找什么 | 去哪看 |
|---|---|
| 前端模块约束的权威定义 | `internal/webui/assetsplit_test.go` |
| 读嵌入资源的测试写法 | `internal/webui/shellio_test.go:229` 的 `readAsset` |
| 涉及 terminal 窗口的全部文案 | `static/js/terminal-view.js`，搜 `title="` 和 `innerHTML =` |
| 会话卡片的全部文案 | `static/js/sessions.js` 的 `renderSessionGrid`（第 31 行起） |
| 连接卡片的全部文案 | `static/js/dialogs.js` 的 `renderConnGrid`（第 61 行起） |
| 现有的 localStorage 命名风格 | 搜 `termcp.` 与 `termcp_`（两种并存，新键用点号风格 `termcp.lang`） |
| 窗口内哪些函数是幂等的 | `applyApprovalMode`、`updateTileToggleButton`、`updateSessionTabbarToggle`、`updateTermScrollButton`、`refreshSessionTabbar`、`applyApprovalCounts` |
| 项目背景与前端架构决策 | `docs/design/webui-redesign.md` |
| 中文术语先例 | `README.zh.md` |

---

**开始前请先读 `internal/webui/assetsplit_test.go` 和 `internal/webui/assets/index.html` 全文，
再动手。**
