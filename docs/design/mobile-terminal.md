# 移动端终端：目标、陷阱与执行计划

> 承接 `docs/design/webui-redesign.md` 的阶段 4（手机外壳）。本文只记录**为达成该目标而做的实测调查**与已验证的陷阱，不改写原 RFC 的决策。
> 调查方式：真实 Go 服务 + headless Chrome（CDP），逐项测量，不靠读代码推断。

## 一、最终目标（可验收形态）

手机上有两个面：**首页**（会话列表 + 抽屉里的连接）和**全屏终端**。

首页：

```
┌──────────────────────────────┐
│ [☰] termcp        API/MCP    │  ← [☰] 拉开左侧抽屉
├──────────────────────────────┤
│ Sessions                     │  ← 首屏就是自己的会话
│ [会话卡片]                    │
│ [会话卡片]                    │
└──────────────────────────────┘
```

点 session / entry → **全屏终端**：

```
┌──────────────────────────────┐
│ [≡] conn · sid1234 [工具▾] [×] │  ← [≡] 切换会话
├──────────────────────────────┤
│                              │
│       xterm（占满剩余高度）      │
│                              │
├──────────────────────────────┤
│ [shell1] [shell2] [+]        │  ← 底部 shell tab
└──────────────────────────────┘
  ↑ safe-area 之上，不触发 iOS 手势
```

- `[☰]` 左侧划入抽屉列出连接 profiles（RFC §2.4 首页空闲态）
- `[≡]` 弹出所有已开会话（含 ended）做无缝切换（RFC §2.4 会话切换器）
- `×` 关 session，回到首页
- 底部 tab 在窗口内切 shell channel（现有 `.shell-channel-tabs` 已是底部，见 §二陷阱 9）
- 桌面端**行为完全不变**（浮窗/平铺/拖拽/右键全保留）

> 与 RFC §2.4 的差异：RFC 写终端页顶部为 `[←] 标题 [工具▾] [×]`（`←` 最小化回首页）。
> 实际实现是 `[≡]`，因为会话切换比回首页更常用，而“回首页”已有 `[−]`（关窗口，进程继续）
> 与关 session 两条路径。这是有意取舍，不是遗漏。

## 二、会卡住方案的陷阱（全部实测验证）

### 陷阱 1 ★ critical：JS 写 inline 尺寸，CSS 媒体查询压不过

`positionShellWindowFromClick` 直接给元素写 inline 样式：

```js
win.style.width  = winW + 'px';   // shell-windows.js:492
win.style.height = winH + 'px';   // :493
win.style.left   = left + 'px';   // :504
```

浏览器实测（390×844 视口，`matchMedia('(max-width: 768px)')` 为 **true**）：

```
inlineW: "640px"   computedW: "640px"   rectW: 640   vw: 390
```

**结论**：`@media` 规则命中也没用，inline 样式优先。任何"只在 CSS 里写全屏"的改法会**静默失效**——页面看起来正常，窗口还是 640px。

**对策（二选一，必须显式处理）**：
- (a) 移动端**不写** inline 尺寸，交给 CSS；
- (b) CSS 侧用 `!important` 强行压过。
选 (a) 更干净，但必须同时清理**已存在**的 inline 值（旋转屏幕、从桌面切到移动时）。

### 陷阱 2：没有任何 resize / orientationchange 监听

```
grep -rn "addEventListener('resize'|orientationchange" static/js/*.js  → 无
```

手机旋转后窗口停留在旧位置/尺寸。全屏化后这个问题更明显（横竖屏切换）。

### 陷阱 3：最大化用 `100vh` 而非 `100dvh`

```js
win.style.width  = 'calc(100vw - 16px)';
win.style.height = 'calc(100vh - 16px)';   // shell-windows.js:450
```

iOS Safari 的 `100vh` 含地址栏区域 → 最大化后底部被裁掉。同一个文件里别处已用 `dvh`，此处遗漏。

### 陷阱 4：`dblclick` 与移动端双击缩放冲突

```js
header.addEventListener('dblclick', function (e) { toggleShellWindowMax(win); });  // :467
```

移动端双击会同时触发浏览器缩放和这个 `dblclick`。

### 陷阱 5：`min-width: 320px / min-height: 240px`

```css
.shell-window { ... min-width: 320px; min-height: 240px; ... }
```

320px 宽的设备上正好卡在边界；若全屏化时残余 padding/border，会溢出。全屏规则里必须归零。

### 陷阱 6：`fitShellTerminal` 在容器尺寸为 0 时**静默 return false**

```js
if (w <= 0 || h <= 0) return false;   // ui-socket.js:450
if (!container || !container.isConnected || !term) return false;  // :445
```

窗口在 `display:none`（`win-hidden`）或布局未提交时测得 0，不报错也不 resize，终端会呈现**错误的行列数**。

### 陷阱 7：手机隐藏 tabbar 造成的能力缺失（已部分处理）

上一个 commit 已隐藏手机 tabbar，但它同时是：
- **tile 模式的唯一出口** → 已用 `body.tile-mode .session-tabbar { display: flex }` 保留；
- **"显示/隐藏所有窗口"（眼睛按钮）入口** → 手机上直接消失（该功能在手机上意义不大，接受）；
- **会话切换器** → RFC 明确要求手机有 `[≡]` 切换会话，目前**没有**，是本次要补的。

### 陷阱 8：`[−]` 按钮的真实语义

```js
function bindShellWindowMinButton(win, minBtn) {
  minBtn.addEventListener('click', function (e) {
    closeShellWindow(win);   // ← 不是"最小化"，是移除窗口
  });
}
```

手机上的回程路径 = 点首页 session 卡片重开（`openOrFocusShellWindow`）。功能上够用，但按钮 `title` 写的是 "Minimize"，语义已偏。

### 陷阱 9：底部 tab 已在**窗口内**底部，不是**屏幕**底部

```css
.shell-channel-tabs { order: 10; border-top: 1px solid #333; min-height: 28px; }
```

`.shell-window-content` 是 `flex-direction: column`，`order:10` 让它排在视觉底部。全屏化后窗口=屏幕，这条自动满足；但**没考虑 safe-area**。

### 陷阱 10：`safe-area-inset` 完全未使用

```
grep -c "safe-area-inset" static/css/app.css  → 0
```

且 `viewport` meta 缺 `viewport-fit=cover`：

```html
<meta name="viewport" content="width=device-width, initial-scale=1">
```

没有 `viewport-fit=cover`，`env(safe-area-inset-*)` 恒为 0，iPhone 的 home indicator 会压住底部 tab。

## 三、执行计划

按"改动面小、可独立验收"排序，每步都能单独跑通再进下一步。

| 步 | 内容 | 状态 |
|---|---|---|
| **1** | 移动端全屏终端（消除陷阱 1/5） | ✅ 完成 |
| **2** | 旋转/resize 重算 + 清 inline（陷阱 2） | ✅ 完成 |
| **3** | 修 `100vh`→`dvh`、`dblclick` 守卫（陷阱 3/4） | ✅ 完成 |
| **4** | safe-area 支持（陷阱 9/10） | ✅ 完成 |
| **5** | 手机会话切换器 `[≡]`（陷阱 7） | ✅ 完成 |
| **6** | 手机 entries 改为左侧抽屉，入口为 `sec-entries` 行（RFC §2.4 首页空闲态） | ✅ 完成 |
| **7** | 单列布局：**entries** 卡片撑满整宽；移除 add/refresh 按钮，改为列表末尾同款卡片 | ✅ 完成 |

> 阶段 7 最初写成 `.conn-grid > .conn-tile { width: 100% }` —— **无差别命中 entries 和 sessions 两个 grid**，
> 把 session 卡也拉成了整行宽。entries 是列表行（该撑满），session 是紧凑卡片（不该撑满），
> 已于同一阶段内拆开，见陷阱 17。

**验收**：桌面 5 个视口行为不变（逐项 A/B 对照），手机全屏、底部 tab 不被 home indicator 压住、旋转后仍全屏、零 console 异常、`go test ./...` 绿。

**不做**（本次范围外）：阶段 5 的 3-xterm 历史轮换（属优化，用户已明确后置）、SDK 抽取。

## 四、执行中发现的额外陷阱（调查阶段未预见）

### 陷阱 11 ★ critical：手机判据不能用宽度

原计划用 `@media (max-width: 768px)` 判定手机，**横屏手机宽 844px > 768 会漏判** ——
既不全屏，CSS 也没隐藏 tabbar，结果是浮窗 + tabbar 盖在上面。

实测 `pointer: coarse` / `hover: none` 才是正确判据（横竖屏都命中，桌面 false）：

| 视口 | max-width:768 | pointer:coarse |
|---|---|---|
| 390×844 竖屏 | ✓ | ✓ |
| 844×390 横屏 | ✗ 漏判 | ✓ |
| 834×1112 平板 | ✗ | ✓ |
| 1440×900 桌面 | ✗ | ✗ |

JS（`isMobileViewport`）与 CSS（媒体查询）统一改用这个判据，避免两者不一致。

### 陷阱 12：inline `z-index` 同样压过 CSS

`bringShellWindowToFront` 写 `win.style.zIndex`，导致 `.win-fullscreen { z-index: 21000 }`
失效（实测 `zWin: 10051`），全屏终端会被 session tabbar（20000）压住。
同样地 `refreshSessionTabbar` 靠 inline z-index 判断哪个是活动窗口。

修法：全屏窗口不走 inline z-index 分支；活动窗口判定把 fullscreen 视为最高。

### 陷阱 13：`z-index` 与活动窗口判定

在修复陷阱 12 时发现，`refreshSessionTabbar` 用 inline `z-index` 决定哪个窗口是"活动"的。
全屏窗口不写 inline z-index，会被当成 0，导致活动 tab 标错。因此该处把 fullscreen 视为最高。

### 陷阱 14：entries 平铺占 233% 首屏

RFC §2.4 要求首页空闲态为"左上 `[☰]` 展开 entries（左侧划入、露出右侧部分、层叠效果）"，
但实现里 entries 只是一个可折叠的普通 section。手机实测：

| 项 | 值 |
|---|---|
| entries 展开高度 | 1964px（**233% 首屏**，22 张卡片） |
| Sessions 标题位置 | 2081px（**需滚两屏才看到自己的会话**） |

手机上用户最需要的是自己的会话，却先看到一长串连接配置。

改为左侧抽屉后：

| 项 | 改前 | 改后 |
|---|---|---|
| Sessions 首屏位置 | 2081px | **117px** |
| entries 呈现 | 平铺 1964px | 划入抽屉（宽 335px） |

**入口就是 `sec-entries` 那一行本身**，没有额外的汉堡按钮：同一行在大屏就地展开、在触屏弹出抽屉。
关闭方式：点卡片 / 点遮罩 / Esc / 转回桌面。

层级：抽屉 `24000` > 全屏终端 `21000` > scrim `23900`，因此**在终端里也能拉开抽屉换连接**。
桌面完全不变（scrim `display:none`，entries 仍内联展开，原有折叠功能保留）。

### 陷阱 15：媒体查询的位置会决定它是否生效

「单列撑满」最初写成文件**开头**的媒体查询，结果完全不生效 ——
卡片宽度仍是 188–300px（entry）/ 128px（session）。
原因：与基础规则同优先级时**后写的胜出**，而 `.conn-tile.entry-card { min-width:188px }`
在第 117 行、`.conn-tile.sess-tile { width:128px }` 在第 220 行，都晚于开头的媒体查询。
移到文件末尾（并补 `min-width:0` 对抗 `min-width` 优先于 `width`）后生效。

### 陷阱 16：删按钮必须连带删 JS 绑定

删掉 `btn-add-conn` / `btn-refresh-conn` 的 HTML 后，`conn-form.js` 仍在
`document.getElementById('btn-add-conn').onclick = ...`，抛 `TypeError`。
因为它在 IIFE 顶层，**整个脚本中断**，后续所有初始化都没跑 ——
表现是 entries 卡片一张都不渲染（不是"按钮消失"，而是页面半瘫）。
教训：删 DOM 元素时要 grep 它的 id。

### 陷阱 17：两个 grid 共用一条宽度规则

`.conn-grid` 是 **entries（`#conn-grid`）和 sessions（`#session-grid`）共用**的类。
阶段 7 的「撑满整宽」写成 `.conn-grid > .conn-tile { width: 100% }`，于是两个 grid 一起被拉满。

但两者语义不同：

| | 形态 | 手机上的正确宽度 |
|---|---|---|
| entries | 列表行（图标 + 名称 + 地址） | 撑满整宽（310px） |
| sessions | 紧凑卡片（图标 + 名称 + sid + 按钮） | 保持 128px，一行排 2–6 张 |

sessions 撑满后变成一条 366px 宽、里面只有 128px 内容的空带，所以看着像"居中"。
把 `text-align` 改成 left 只是把这种空带从居中变左靠 —— 治标。**真正的修法是把宽度规则限定到 entries**：

```css
/* 只给 entries，不要用 .conn-grid > .conn-tile 连 sessions 一起命中 */
.conn-tile.entry-card { width: 100%; min-width: 0; max-width: none; }
```

改后实测（手机 390×844）：`tileW=128`（与桌面一致）、一行 2 张、无横向滚动。
横屏 844 → 一行 6 张，iPad 834 → 5 张，320 → 2 张。

**教训**：给共用类写规则前，先 grep 谁在用它。

### 陷阱 18：模态弹窗的层级低于抽屉和全屏终端

抽屉里的「编辑连接」按钮调 `openConnModal` → `showModal('modal-conn')`，而 `.modal-backdrop`
的 `z-index` 是 `20000`，低于抽屉（`24000`）和 scrim（`23900`）。手机上的实际表现：
点编辑，弹窗确实打开了（`hidden` 已摘），但被抽屉整个盖住 —— 看起来像“点了没反应”。

同一条路径从全屏终端（`21000`）走也一样被盖：汉堡 → 抽屉 → 编辑。

**根因**是层级阶梯把模态弹窗当成了**内容层**（和 session tabbar 同为 20000），
但它是**模态层**：任何能打开它的表面都必须排在它下面。修法是把抬到所有内容层之上，
同时仍留在 toast/notify 之下（它们本来就在弹窗之上）：

| 层 | z-index |
|---|---|
| notify stack（`notify_user`） | 26000 |
| toast | 25000 |
| **模态弹窗** | **24500** |
| 抽屉 | 24000 |
| scrim | 23900 |
| 全屏终端 | 21000 |
| session tabbar / 内容层 | 20000 |

24500 是**最小提升**：刚好越过抽屉，并保留弹窗原本就在 toast/notify 之下的相对顺序。
（最初写成 28000 是错的 —— 那把弹窗抬到了 toast 之上，与本条自己的理由相反：
`notify_user` 推送的错误提示被弹窗遮住，正是用户最该看到的那条消息。）
`.win-fullscreen` 那句「nothing may float over it」也随之收窄 —— 它约束的是**页面内容层**，
弹窗、toast 与通知本就不是页面内容。

**实测验证**（真浏览器 + `elementFromPoint` 打弹窗中心）：改前 `modalZ=20000`，中心命中
`sec-entries-body`（抽屉自己）、`hitIsInsideModal=false`；改后 `modalZ=24500`，命中 `LABEL`、
`hitIsInsideModal=true`。即“点了编辑没反应”的真相是：弹窗开了、铺满全屏，但用户点到的是抽屉。

**教训**：新增一个可打开弹窗的表面时，先查弹窗在层级阶梯里的位置。
「弹窗能打开」不等于「弹窗可见」—— 两者在 CSS 里是两件事，`classList.remove('hidden')` 不会报错。

### 陷阱 19：「加号」卡片在手机上不占满一行

条目列表在触屏上是单列撑满（陷阱 17），但加号卡在桌面被写成三个类的
`.conn-tile.entry-card.entry-card-add { width: auto }`（`0,3,0`），**特异性压过**触屏块的
`.conn-tile.entry-card { width: 100% }`（`0,2,0`），于是手机上它是列表里唯一一张
按字形宽度收缩的卡片，夹在一列满宽行中。

修法：最简的是把桌面那条的 `width` 从 `auto` 改成 `100%` —— 但**桌面不是单列**，
会得到一张横跨整个 grid 的空白条，比手机上更难看。所以加号卡的宽度必须**按断点分开写**：
桌面 `auto`（贴字形），触屏 `100%`（跟其他条目一样占满行），并且触屏那条要带够类名压过桌面那条。

**教训**：特异性是单向的 —— 桌面规则写三个类，触屏规则就必须写三个类以上才能覆盖，
哪怕触屏那条在文件更后面（陷阱 15 的同一条规律，反过来用）。

实测（手机 390×844，抽屉打开态）：加号卡 `310px` = 内容宽 `310`，与普通条目一致；桌面 `78px`。

### 陷阱 20：满宽之后，字形仍停在左边

加号卡去掉文字后只剩一个字形，而它复用的是真实条目的行布局
`.entry-card-inner { flex-direction: row }` —— 真实条目是「图标 + 名称」左对齐，
加号卡没有名称可以并排，于是单个字形就停在了左边。手机上卡片撑满后（陷阱 19），
这个偏移被放大成一张 310px 宽、字形贴在左边缘的空白条。

修法：加号卡的 `entry-card-inner` 覆盖 `justify-content: center`。

**这个 bug 在桌面上看不出来**：桌面卡片只有 78px 宽，20px 的字形在 78px 里
“看起来”就是居中的（实测偏移 0）。只有手机满宽时才暴露（实测偏移 −116px）。

**教训**：断点相关的问题不能只在桌面验收 —— 陷阱 19 和 20 都是同一类：
一个在窄卡片下成立、在宽卡片下崩掉的布局假设。

### 陷阱 21：重命名按钮放在 sid 旁边，读起来是“编辑 id”

session 卡片原来的重命名是一个铅笔按钮，位于 `.sess-meta-row` —— 与 **sid 同一行**。
位置就是语义：用户看到的是「一行 sid 旁边一个铅笔」，自然读成“改这个 id”，
而它实际改的是**会话名字**（卡片上那一行 `.sess-entry-line`）。

同时复选框被绝对定位钉在图标左上角（与删除 × 关于图标中线镜像），读起来像“属于图标”。

改成：

- 复选框从图标左上角移到名字行 `.sess-name-row`，与它选中的名字同一行；
- 铅笔按钮**删掉**，名字本身成为重命名控件（`role="button"` + `tabindex="0"`，支持 Enter/Space）；
- sid 旁边那个复制按钮也**删掉**，改成**点 sid 本身复制** session URL —— 理由与铅笔同源：
  id 已经是你要对准的东西，旁边再放一个按钮是多余的目标；
- 状态从名字旁的 `dead` 文字徽章改成 **sid 左侧的灯**：绿=running、红=dead，
  reason 仍走 tooltip；
- 卡片打开会话的 `tile.onclick` 必须**跳过**名字、sid 与状态灯，否则改名/复制会在背后顺带打开终端。

`SVG_PENCIL_12` 只有那一处用，随按钮一起删（否则是死代码）。
状态灯是 **CSS 圆点**而非 SVG：一个 7px 的点用图标描边反而更难看，而且不需要额外的全局常量。

**教训**：控件的**位置**就是它的文案。把一个“改名”的铅笔放在 id 旁边，
再清楚的 `title="Rename session"` 也救不回来 —— 用户看的是位置，不是 tooltip。
反过来，把动作**合并进已有的靶子**（名字改名、sid 复制）比在它旁边再加一个按钮更省。

#### 陷阱 21 补：名字行的两个元素要对齐方式做决定

复选框移到名字行后，两种排法都试过，结论是**两个作为一组居中**：

| 排法 | 效果 |
|---|---|
| 复选框顶左 + 名字在剩余空间居中 | 短名字时两者隔一大段空白，看着像没关系（实测间隙 **42px**，而 CSS 里写的 gap 是 4px） |
| 复选框顶左 + 名字紧贴（当前） | 两个元素作为一组居中，间隙就是 gap |

关键在名字的 `flex`：写成 `flex: 1 1 auto` 时名字盒**撑满剩余宽度**，
文字再在盒子里 `text-align: center` —— 于是 `gap` 根本不起作用，
看起来的间距由那个被撑满的盒子的宽度决定。改成 `flex: 0 1 auto`
（按文本收缩，保留 `min-width: 0` 以便长名字省略号生效）后，两个元素才真正相邻。

最终形态：`.sess-name-row { justify-content: center; gap: 4px; padding: 0 4px; }`
+ `.sess-entry-line { flex: 0 1 auto; }`。

改后实测（390×844 与 1440×900 一致）：短名字 `leftInset 38 = rightInset 38`、
`groupCenterOffset 0`、`gapCheckboxToText 4`；长名字 `ellipsis: true` 且不溢出；
复选框与名字中心 y 相同（同一中线）、行高仍 16px。

**教训**：`gap` 只在两个元素**相邻**时才等于视觉间隙。中间夹一个 `flex: 1` 的盒子，
`gap` 就只是个数字，看起来的间距由那个盒子的宽度决定。

## 五、残余风险

- **真实 iOS Safari 无法在本机验证**（只有 Windows + Chrome）。safe-area 与 `dvh` 只能靠规范正确性 + 桌面模拟，需真机复核。
- WebView（微信/X5）行为差异未测。
- 全屏化改变了"多窗口"语义：手机上一次只能看一个终端（符合 RFC 的全屏层模型，但与桌面浮窗模型不同构）。
