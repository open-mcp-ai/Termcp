# Termcp 身份与接入

这些词汇区分管理实例的人、登录申请者、执行操作的 Agent，以及它们之间的授权关系。

## Language

**Admin（管理员）**：管理 Termcp 实例、权限组和接入权限的人；管理操作的身份与 Agent 的执行身份分别记录。
_Avoid_: 超级 Agent

**登录用户（Login User）**：申请接入 Termcp 的自然人身份，可以拥有多个 Agent；完成登录本身不授予实例管理权限。
_Avoid_: Agent 账号、客户端应用

**Agent**：在 Termcp 中执行操作、接受权限组约束的主体；其所属用户与执行身份是不同概念。
_Avoid_: OAuth 用户、MCP 连接

**接入授权（Access Grant）**：把一次获准的客户端接入与所属用户、Agent、允许的访问范围和期限关联起来的授权关系，可以独立结束。
_Avoid_: 登录会话、Token

**缺省组（Default Group）**：管理员为新 Agent 指定的初始权限组；组的名称和主机范围本身不代表拥有业务操作权限。
_Avoid_: 默认管理员组
