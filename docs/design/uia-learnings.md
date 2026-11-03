# UI Automation 踩坑与性能记录

来自 issue #201 的实测总结（针对 Windows UI Automation 适配）。

## 1. 元数据税（Metadata Overhead）

`uia_children` 返回一个元素大约 **1.2 KB**，其中 95% 是冗余字段：
- `providerDescription` 约 450 字节，且同一进程的元素逐字相同；
- `legacyAccessiblePattern` 约 400 字节，绝大多数是空字段；
- `ariaProperties`、`ariaRole`、`culture` 等大量恒为空的字段。

实测数据：
- 任务栏的 10 个子元素 = **37 KB**
- 一个记事本窗口的子元素 = **10 KB**
- 不带参数的 `uia_find {}` 扫全桌面 = **117 KB**

**注**：此前 termcp 默认分页较小（512 B），导致大帧需要多次读取；在 issue #209 将 `bytes_limit` 确立为真正契约（默认 8192 B，内联最大 8 MiB）后，该传输层阻碍已消除。

## 2. 窗口子元素结构陷阱

- Win32 记事本的菜单栏不是标准 UIA 子元素，`uia_find_name {"name":"文件"}` 返回的列表为空；
- 真实菜单只在 `LegacyIAccessible` 的 `roleName: MenuPopup` 出现；
- 想走菜单实现操作（如存盘），不能假定所有 Win32 控件都挂在 UIA 树上。

## 3. 「动作成功」与「状态改变」解耦

- 对 UI 发送操作（如 `default-action` 点击「开始」按钮）可能返回 `"success": true`，但底层控件的 `toggleState` 仍为 `Off`（UI 并没有真正打开）；
- **动作执行成功不等于世界状态发生改变**；
- 自动化断言不能信动作本身的返回信息（如刚写进去的 `valuePattern.value`），必须在操作后通过**另一条读取模式**重新获取真实值做二次确认。
