# 终端字体栈：为什么不内嵌字体，以及各系统怎么配

> 对应 issue #77「需要固定一个小体积字体，目前在 kali 环境下显示较差」。
> 实现见 `internal/webui/assets/static/css/tokens.css`（`--font-mono` /
> `--font-sans` / `--font-sans-hant`），回归见 `internal/webui/font_stack_test.go`，
> 实测脚本见 `scripts/fontlab.py`。

## 一、结论：不内嵌字体

**不内嵌，只用系统字体栈。** 三条理由：

1. **要内嵌就得嵌全字集，而全字集不可接受。** CJK 全集 woff2 约 9–10 MB
   （Noto Sans SC 实测 9.5 MB）；子集到 GB2312 一级（3755 字）能压到 400–800 KB，
   但生僻字（人名、古籍、罕见姓氏）依然缺失，于是「内嵌字体没有 + 系统没有」叠加，
   反而比纯字体栈更不可控。Termcp 是单二进制分发的工具，为一个美化项付这个体积不划算。
2. **字体栈已覆盖目标平台。** macOS / Windows / iOS / Android / 主流桌面 Linux
   （GNOME/KDE 默认安装）都自带 CJK 字体。CJK 字体不是终端能工作的前提，
   能被画出来就够了。
3. **字宽由 wcwidth 决定，不由字体决定。** 终端里 CJK 占 2 格是 `wcwidth` 的判定。
   所以真正的风险不是「不好看」，而是**所选字体与格子不匹配**。字体栈的排序规则
   就是为压制它。

## 二、机制：格子是怎么对齐的（实测，不是推测）

读随仓库分发的 xterm 构建（DOM renderer）可知，它**不拒绝**不合适的字体，
而是**纠正**它。`DomRendererRowFactory` 对每个 span 计算：

```
letterSpacing = cells × cellWidth − glyphWidth      // CJK 时 cells = 2
```

而 `cellWidth` 由拉丁 `W` 的宽度决定。于是：

- Han 推进宽度**恰好**等于 2 × cellWidth → letterSpacing = 0，视觉理想；
- 大于 → 负 letterSpacing，字形被压，可能咬到邻格；
- 小于 → 正 letterSpacing，字被撑开，中文看起来松散（**这才是最常见的实际表现**）。

关键推论，也是排序规则的来源：

> **浏览器是逐字形回退的**（per-glyph fallback）。字体栈里只要**任何位置**
> 出现有汉字的字体，汉字就有覆盖——位置不决定「有没有覆盖」，只决定
> **由哪个字体来画**。而拉丁字形与汉字字形可能来自两个不同的字体，
> 所以问题变成**让两者宽度匹配**，而不是让某一个字体单独好。

在 Windows 上实测（13px，`scripts/fontlab.py`）：

| 字体栈 | cellW | Han padding | 说明 |
|---|---|---|---|
| `Consolas, Monaco, monospace`（**旧代码**） | 7.15 | +0.30 | 汉字由系统回退字体画 |
| 单个 CJK 字体（`SimSun`，拉丁 7px / 汉字 14px） | 7.00 | +1.00 | 同一个字体供两套字，接近 2:1 |
| 比例式 CJK 排在前面（如 `Noto Sans SC`） | 13.23 | — | cellW 被撑到 13.2px（+85%），**全屏每个字形**的纠正量都被它决定 |

另一个反直觉实测：**`ui-monospace` 在 Windows/Chromium 上解析到的是比例式字体**
（实测 W=11.41 / M=10.56 / i=3.58，W≠M≠i，**根本不是等宽**）。
所以它只能当后期兜底（对 iOS/WebKit 有用，因为那种环境不暴露具名 Apple 字体），
**绝不能排在 `Consolas` 前面** —— 否则 Windows 上 cellW 会从 7.15 跳到 7.61~11.4，
汉字反而被撑得更开。

### 由此得到两条排序规则

- **规则 1（必须）：比例式 CJK 面排在所有「格子安全」面之后。**
  比例式 CJK 字体（PingFang、微软雅黑、Noto Sans CJK）自带拉丁字形且推进是比例值，
  排在前面就会接管拉丁文本，把 cellW 抬高 ~85%，从而决定全屏每个字形的纠正量。
- **规则 2（优化）：CJK 等宽面尽量靠前。**
  CJK **等宽**字体被设计成拉丁 0.5em、汉字 1em，即**汉字恰好 2 倍**，
  是唯一能把 padding 压到 0 的做法。混用「拉丁字体 + 另一个 CJK 字体」达不到。

> ⚠️ 两个被实测推翻的直觉，初版实现曾在此写错：
> 1. **「所有 CJK 面必须排在所有拉丁面之后」是错的**。CJK 等宽面本身是合法的格子字体，
>    且在没装拉丁字体的 Linux 上它同时解决两套字。所以只有**比例式**必须垫底。
> 2. **「`ui-monospace` 是等宽字体」在 Windows 上是错的**（见上），它只能放在后面。

## 三、最终栈的结构

`--font-mono` 分五组，顺序即规则：

1. **CJK 等宽面（最前）**：Sarasa Mono/Fixed/Term、Noto Sans Mono CJK、Source Han Mono、
   文泉驿等宽。装了就是最优解——两套字同一字体，汉字 = 2 × 拉丁。
   Debian 系 `fonts-noto-cjk` 里就带 `Noto Sans Mono CJK SC`，所以**Kali 装完这个包即命中**。
2. **各系统原生等宽面**：`SF Mono`/`Menlo`/`Monaco`（Apple）、
   `Consolas`/`Cascadia Mono`（Windows）、`Roboto Mono`（Android）、
   `DejaVu`/`Liberation`/`Ubuntu Mono`/`Droid Sans Mono`（Linux）。
   `Consolas` 有意排在 `Cascadia Mono` 前：Windows 原本就用它，且实测 padding 更低（0.30 vs 2.23）。
3. **`ui-monospace`**：后期兜底，主要给 iOS/WebKit。
4. **`NSimSun`**：Windows 简体汉字的格子安全面。它是 SimSun 家族的等宽面
   （汉字/拉丁 = 2.000），单列在比例式组之前；否则简体汉字会被比例式的
   `Microsoft YaHei`（推进 13.0px）接管，而 `Consolas` 的格子要求 14.3px，
   每个汉字被撑开 1.3px（旧栈为 0.3px）。不能写 `SimSun`（同样 2:1，但在栈中不让位）。
5. **比例式 CJK**：PingFang、雅黑、JhengHei、Yu Gothic、Noto Sans CJK、
   文泉驿、`Droid Sans Fallback`（Android 汉字兜底）等，最后 `monospace` 关键字收尾。

## 四、各平台表现

| 平台 | 命中字体 | 需配置 |
|---|---|---|
| **Windows** | Consolas / Cascadia Mono；汉字走 `NSimSun`（雅黑垫底，仅在前者缺席时接手） | 否 |
| **macOS** | `SF Mono` / `Menlo` / `Monaco` 等具名面（`ui-monospace` 只是后期兜底）；PingFang | 否 |
| **iOS / iPadOS** | 同 macOS（WebKit 自带 PingFang SC/TC） | 否 |
| **Android** | Roboto Mono / Droid Sans Mono；`Droid Sans Fallback` 供汉字 | 否 |
| **Debian/Ubuntu 桌面** | DejaVu Sans Mono / Ubuntu Mono；装了桌面则另有 Noto Sans CJK | 通常否 |
| **Kali Linux** | 见下节（实测：默认只间接带入 DejaVu 类拉丁字体） | **需要** |
| **Alpine / 最小容器** | 可能全无 CJK | **见下节** |

## 五、系统确实没装 CJK 字体时

### 为什么 Kali 会中招（实测数据）

Kali 的元包（`kali-linux-core`、`kali-linux-default`）、桌面元包
（`kali-desktop-xfce`、`kali-desktop-gnome`）以及终端本身
（`xfce4-terminal`、`gnome-terminal`、`xterm`）**都没有任何字体依赖**——
字体是经由桌面环境间接带入的。实测 Kali rolling 的 `main` 仓库，实际结果通常只有：

| 包 | installed | 有汉字？ |
|---|---|---|
| `fonts-dejavu-core` | 2.2 MB | ❌ 纯拉丁 |
| `fonts-liberation` | — | ❌ 纯拉丁 |

而 `monospace` 在 Debian 系上别名到 **DejaVu Sans Mono**，也没有汉字。
所以旧代码 `'Consolas, Monaco, monospace'` 在 Kali 上整条栈全落空，
浏览器把汉字交给最后的兜底字体（Debian 系那个通常是 **Unifont**，
32.4 MB 的 16×16 点阵位图）。点阵汉字的宽度与 DejaVu 的格子完全不成比例——
**这就是「终端直接变成超宽字体」的真正原因：不是字体丑，是字体不存在。**

Linux 上「通用的几种字体」因此要分两类：

**① 几乎所有发行版都有，但都没有汉字**：`DejaVu Sans Mono` / `DejaVu Sans`、
`Liberation Mono` / `Liberation Sans`、`Noto Sans Mono`（拉丁部分）。
字体栈里这些都在，但它们只能解决拉丁字形。

**② 有汉字，但要自己装**：

| 字体 | 包 | installed | 类型 |
|---|---|---|---|
| Noto Sans CJK | `fonts-noto-cjk` | **89.1 MB** | 比例式（含等宽子族） |
| 文泉驿微米黑 | `fonts-wqy-microhei` | **5.0 MB** | 比例式 |
| Unifont | `fonts-unifont` | 32.4 MB | 点阵，仅兜底用 |

**③ 真正适合终端的 CJK 等宽字体，发行版默认一个都不装**：
Sarasa Mono SC、Noto Sans Mono CJK、Source Han Mono——都要自己装。

### 装一个即可

```bash
# Debian / Ubuntu / Kali / Mint —— 最小代价，5 MB，立刻能看（比例式）
sudo apt install fonts-wqy-microhei && fc-cache -fv

# 最佳观感（89 MB，含 CJK 等宽子族，汉字正好 2 格）
sudo apt install fonts-noto-cjk && fc-cache -fv

# Fedora / RHEL / CentOS
sudo dnf install google-noto-sans-cjk-fonts

# Arch / Manjaro
sudo pacman -S noto-fonts-cjk

# Alpine
apk add font-noto-cjk
```

装完 `fc-cache -fv`，重开页面。**字体栈已把它们排在第 1 组，装好即自动优先命中，
不需要改任何代码。**

> **`fonts-noto-cjk` 就够（89 MB）**：该包里除了比例式的 `Noto Sans CJK SC`，
> 还带 **`Noto Sans Mono CJK SC`**（CJK 等宽，汉字恰好 2 倍拉丁）——
> 上游文档明确说明 OTC 的 Regular/Bold 两个字重内含 half-width (Monospace) 变体。
> 字体栈把它排在第 1 组，所以装上即自动命中，**不需要改代码**。
> 只想更省空间时用 `fonts-wqy-microhei`（5 MB，比例式，能显示但 padding 非零）。
> 追求最佳观感可装 [Sarasa Gothic](https://github.com/be5invis/Sarasa-Gothic)（OFL）。

## 六、实现要点

### 单一字体来源

终端格子走 JS 选项，而页面其余部分走 CSS；两条链一旦各持一份列表就会漂移（旧代码正是如此）。
现在 CSS 持有唯一的列表：

- `tokens.css` 的 `--font-mono` / `--font-sans` / `--font-sans-hant` 是**唯一真源**；
- `base.css` 的 `body.lang-hant` 不再复写一份 sans 列表，只绑定 `var(--font-sans-hant)`；
- 壳内页面的行内样式（`terminal-templates.js` 的文件面板）由 `ui-monospace, monospace`
  改为 `var(--font-mono)`。

`--font-sans-hant` 与 `--font-sans` 的关系是**同一份列表换序**（TC 面提前，因为字体名决定字形形式），
不是两份各自维护的列表：`TestHantSansStackIsSansReordered` 逐名比对两边，
只允许 Hant 侧多一个 `PingFang HK`（苹果的繁体对位名，无 SC 名字）。
这条断言是必要的，因为初版实现已经漂移过：Hant 侧漏了 `Source Han Sans SC`、
两个文泉驿和 JP/KR 名字，即「只是重排」变成了静默掉覆盖。

### 终端格子：唯一读不到 CSS 变量的地方

xterm 不走 CSS：它把 `fontFamily` 当 **JS 选项**接收，并注入自己的样式表
（`_injectCss` 写入 `${selector} .xterm-rows { font-family: <option> }`）。
所以终端格子是唯一无法继承 `--font-mono` 的表面。

这是原缺陷的成因：`terminal-view.js` 硬编码 `'Consolas, Monaco, monospace'`，
`ui-socket.js` 量列宽时又抄了同一串。在有 Consolas 的机器上看不出问题；
到裸 Linux 上整条栈落到 `monospace`，而**页面其他部分走 CSS 变量所以是正常的**，
两套标准在同一个界面上并存。

现在 `util.js` 的 `termcpMonoFontFamily()` 读 `--font-mono`，
`terminal-view.js` 与 `ui-socket.js` 共用。JS 保留一份等值常量作兜底，
由 `TestTerminalFontStackMatchesCSS` **逐名比对**（比较前剥离 CSS 注释并按空白归一，
即浏览器对声明本就施加的两种归一，所以换行位置变化不会误报，改字体名会报）。

### 列宽测量必须同字体

`fitShellTerminal()` 用隐藏 `<span>` 量单字符宽度再把像素换算成列数。
若该 span 与格子不同字体，列数就是错的（提前折行或右侧被切）。
它现在优先读 `term.fontFamily`，回退到 `termcpMonoFontFamily()`。

### 未采用的选项

`rescaleOverlappingGlyphs` 看似对症（把越界字形压回格内），但**这份 xterm 构建只声明、
不消费它**：它出现在默认值表与 `onMultipleOptionChange` 列表里，渲染器没有读取点
（实测全文只有 2 处出现）。设了是静默无效，故未启用。

### 实测脚本

`scripts/fontlab.py` 用真实 xterm.js 在 headless Chrome 里跑，输出
①各字体名在本机解析成什么、②每个栈的 cellW 与 Han padding。
**改栈之后建议跑一次**——本文的结论全部来自它，而不是推测。
