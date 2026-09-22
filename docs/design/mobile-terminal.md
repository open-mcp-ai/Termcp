# 移动端终端：目标、陷阱与执行计划

> 承接 `docs/design/webui-redesign.md` 的阶段 4（手机外壳）。本文只记录**为达成该目标而做的实测调查**与已验证的陷阱，不改写原 RFC 的决策。
> 调查方式：真实 Go 服务 + headless Chrome（CDP），逐项测量，不靠读代码推断。

## 一、最终目标（可验收形态）

手机上点一个 session → **全屏终端**（不再是 374×480 的浮窗）：

```
┌──────────────────────────────┐
│ [←]  conn · sid1234   [工具▾] [×] │  ← 顶栏
├──────────────────────────────┤
│                              │
│       xterm（占满剩余高度）      │
│                              │
├──────────────────────────────┤
│ [shell1] [shell2] [+]        │  ← 底部 shell tab
└──────────────────────────────┘
  ↑ safe-area 之上，不触发 iOS 手势
```

- `←` 回首页（session 继续跑），`×` 关 session
- 底部 tab 在窗口内切 shell channel（现有 `.shell-channel-tabs` 已是底部，见 §三）
- 桌面端**行为完全不变**（浮窗/平铺/拖拽/右键全保留）

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

## 五、残余风险

- **真实 iOS Safari 无法在本机验证**（只有 Windows + Chrome）。safe-area 与 `dvh` 只能靠规范正确性 + 桌面模拟，需真机复核。
- WebView（微信/X5）行为差异未测。
- 全屏化改变了"多窗口"语义：手机上一次只能看一个终端（符合 RFC 的全屏层模型，但与桌面浮窗模型不同构）。
