# 认证与权限页面的共享主题

状态：2026-10-30 本分支已实现，对应 #232 的 U01/U02，尚未合并。

## 页面与主题来源

`/setup`、`/login`、`/auth`、`/permissions` 共用认证主题 shell。页面使用工作区已有的主题语义颜色、字体、主题启动脚本与切换引擎；保留各自的表单/权限布局，不启动工作区终端、WebSocket 或业务脚本。

四个内置主题及 auto 使用同一份 `termcp.theme` 浏览器偏好。auto 继续跟随系统，首屏使用既有 theme-boot 加载及回退机制。主题缺失时回退到 default-light，原偏好保留，用户可在菜单中选择其他主题。窄屏不会套用工作区的固定 32px 主题按钮宽度。

启动层调用 `Managed.SetPageAssets(webui.Assets())`，在 `Wrap` 之前注入同一个文件系统。auth 不 import webui，沿用外部 assets、外部 themes 与内嵌资源的覆盖/回退顺序。没有可用的主题资源时（包括 no_webui 构建）使用认证 shell 的本地后备配色，不显示失效的主题选择器，不把页面隐藏在加载状态。

## 登录前静态入口

`GET/HEAD /auth/ui/...` 由 auth 自己处理，不进入应用路由。可公开读取：

- 两个固定脚本：`static/theme-boot.js` 与 `static/js/theme.js`。
- `static/css/` 的 CSS、公共字体与图标。
- 主题内的 CSS、本地字体和图片，以及固定的 `theme.json`、`strings.json`。
- `themes/` 的最小发现清单，仅列出实际有 theme.css 的有效主题 ID。

主题及其展示资源属于公开静态材料，不能放入配置秘密。任意其他 JavaScript、HTML、数据文件和目录列表均不公开；缺失/拒绝路径返回 404，写入方法返回 405。路径经过有效性检查，文件打开后再次拒绝目录，即使外部文件在检查之后被换成目录也不能列出内容。资源 no-store，并忽略条件缓存请求，避免同秒修改或旧版本资源被错误复用。

未登录者仍然不能读取 `/auth/groups`、`/auth/principals`、`/auth/tokens` 或业务 API；Agent 没有因此获得管理面权限。静态响应不签发 cookie。

## CSP

认证页面默认拒绝资源，仅开放同源连接、样式、字体与图片；图片另允许内嵌 data。每一个内联及外部脚本都需要本次响应的 nonce，没有加入 script-src self 或 unsafe-inline。保留 base-uri none、frame-ancestors none 和 form-action self。页面自身仍有既有内联样式，因此 style-src 保留 unsafe-inline。

静态资源响应另有禁止脚本和 sandbox 的 CSP，使 SVG 或清单作为顶层文档打开时不成为活动页面。它们不会放行匿名管理请求。

## 验证

- auth HTTP 回归覆盖公开资源、HEAD、写方法拒绝、路径遍历、未知脚本/数据、目录替换、缓存、nonce，以及缺少主题资源时的后备页面。
- webui 集成回归递归请求四个真实内置主题的 CSS imports、字体、图片和元数据，经过 Managed admission，不依赖已有登录。
- 共享 JS 回归覆盖认证资源挂载、工作区/文档原路径、代理部署前缀、自动跟随系统与缺失主题回退。
- macOS 全仓 make test（race/shuffle）通过；主题相关回归六轮并发验证通过，文件替换回归也通过六轮；no_webui 构建及对应回归通过。
- 内置浏览器使用临时实例和合成账号检查四主题、auto、首次设置、账号/权限页、退出后资源、390px 窄屏和外部主题消失回退。正常页面无 CSP/控制台错误；移除主题文件时出现预期的资源 404，页面成功恢复。
- Win11 PD 上 auth 完整包及真实主题 imports 回归通过，使用 Go 1.27.3 交叉编译 windows/amd64、CGO=0。未运行 Windows 全仓/race 或 Linux CI。
