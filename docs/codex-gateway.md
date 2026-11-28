# 双渠道 Codex 网关

Cloudex 由同一个 Go 控制面持有上游授权，并向工作容器提供 Responses 网关。一份授权可供多个 Workspace 显式绑定。同一部署内的两个 Workspace 可以使用不同渠道。添加授权只建立该身份的记录。限流、权限失败和路由不可用只影响对应授权或请求，管理员可明确选择换绑。[配置与使用](operations.md#codex-网关) · [管理 API](api.md#codex-授权和绑定)

## 渠道协议

| 环节 | Sign in with Codex | Sign in with ChatGPT |
|---|---|---|
| 应用注册 | 固定版本的 Codex 第一方 client ID。Cloudex 在授权参数中标识自身。 | 公开 plan usage 动态注册，以安装级 `ext_agent_host_id` 标识长期持有凭据的一方。首次回调提供 issued client ID，重授权复用它。 |
| 授权和换码 | `/oauth/authorize` 与 `/oauth/token`。PKCE S256、一次性 state、精确回环 URI、固定版 scope。 | `/api/accounts/authorize` 与 `/api/accounts/oauth/token`。同时核验 OIDC nonce、issued client ID、`resource=https://api.openai.com/v1` 和实际授予的推理权限。 |
| 身份 | 验证 ID token 的签名和声明，并核对 access token 的上游用户与 workspace。请求使用已核验的 `ChatGPT-Account-ID`。 | 验证 issued client 对应的 ID token。以注册和 verified `sub` 识别授权。未回显的上游 workspace ID 保持未知。 |
| 刷新和撤销 | 向固定版 `/oauth/token`、`/oauth/revoke` 发送 JSON，按原注册处理令牌轮换。 | 向公开流程的 token、revocation 端点发送 form。刷新使用 issued client ID、原 resource，并遵守 `earliest_refresh_at`。 |
| 模型目录 | 通常为 `GET https://chatgpt.com/backend-api/codex/models?client_version=0.172.0`，由服务端可信 workspace 路由决定实际目的地。 | `GET https://api.openai.com/v1/models`，使用该注册的 access token。 |
| Responses | 通常为 `https://chatgpt.com/backend-api/codex/responses`（WS 使用 `wss`），带账号头及固定版 WS beta 头。 | `https://api.openai.com/v1/responses`（WS 使用 `wss`），使用该注册的 access token。HTTP 使用 `store:false`、`stream:true` 和完整输入历史。 |

两渠道的 OAuth、身份、令牌、头和目录各自解释。邮箱只用于显示。ChatGPT 缺少 `chatgpt.tokens.use.direct` 时，可识别登录身份，推理标为未授权。Codex 专属 workspace 分别确认模型目录和 Responses 路由，返回未经可信发现的目标或缺失专属路由时停止对应环节。[固定版 Codex 源码](https://github.com/openai/codex/tree/3d86e43b075adeba2193ec5c22ac92f55afd82e6) · [ChatGPT 注册与登录](https://developers.openai.com/siwc/token-sharing-open-source/sign-in) · [ChatGPT 模型与推理](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)

启动器从当前授权的目录生成 Codex `0.172.0` 的模型快照。可见 slug 和客户端可编码能力共同决定可用模型。ChatGPT 的预览针对 function/custom 工具使用 namespace 或 `additional_tools`。若一个模型和客户端组合会产生不受支持的工具请求，返回模型或字段错误。每次提交继续按当前授权和能力检查。[ChatGPT 预览限制](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)

## 授权身份和刷新

授权创建后，渠道与身份固定。重授权确认同一注册、同一 subject，以及适用的上游用户和 workspace 后替换整份授权。选择了其他身份时，本次结果作废。成功替换递增授权代次 `grant_epoch`，已有绑定保留，旧会话上下文失去续写资格。

同一授权的并发请求共享刷新。正常刷新在事务内保存轮换令牌与元数据，推进 `token_version`。原身份的流可继续到原连接截止，新握手和新 POST 使用有效新 token。明确临时失败会保留授权，明确失效或疑似已兑换却未完成持久化时要求重新授权。刷新的版本与尝试标识阻止迟到回包修改已被替换或断开的授权。[ChatGPT 会话时效](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions) · [ChatGPT 刷新错误](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery#refresh-errors)

## 换绑和上下文

`binding_revision` 的事务提交是切换生效点。控制面协调待拨号登记、下游 `101`、实际上游写入和撤销。换绑提交后，旧凭据、旧 WebSocket、在途 SSE 与已排队的生成或工具结果均失去转发权。提交前已交给网络的字节仍可能抵达旧上游，远端计算结果可能未知。

例如 `W1` 和 `W2` 都绑定 A 时，只把 `W1` 改绑到另一渠道的 B。B 的模型、客户端会话空间与上游上下文从它自己的绑定开始。A 发给 `W1` 的工具调用和响应 ID 不能续写到 B，`W2` 继续使用 A。若 A 已失效，`W2` 仍显示 A 的真实状态。两种渠道换绑方向共用此边界。

Codex 启动器按授权 ID 和授权代次选择容器内的会话空间。同一容器切回同一份授权的同一代次后，重启客户端或重新连接 Desktop 可以打开该授权下原来的会话。重授权使用新的会话空间，容器重建会丢弃原容器的会话文件。

启用会话恢复需要控制面与 Workspace 镜像都支持在凭据安装请求及确认中传输授权 ID。以 Agent 接入的 Host 还需要支持该协议的 Host Agent。升级此协议时，应使用支持该协议的新 Workspace 镜像重建旧容器后再启用推理。

运行实例凭据仅存于容器的临时挂载，控制面持久化其摘要与安装状态。持有实例凭据的进程能在撤销前使用该绑定的推理能力；访问位置无法单凭 Bearer 证明。[实例生命周期](workspace-lifecycle.md#codex-授权和运行实例)

## 流式转发

网关对每条下游 Responses WS 建立对应的上游连接。WS 只在发出它的有效连接内接收 `previous_response_id`，工具结果按 `call_id` 及逻辑会话核对。HTTP/SSE 续轮用输入历史交回工具结果。切回原授权后，同一容器、授权代次和会话下已获上游完成确认的完整工具调用和结果可作为新一轮的历史输入。旧绑定的未完成工具调用、单独回传的旧工具结果和其他授权或 Workspace 的引用会在转发前遭拒。客户端中断停止其转发，对上游已接受的工作需要以实际中断确认或结果未知表示。

生成尚未提交时，上游明确拒绝 Upgrade、返回无效升级响应，或 ChatGPT 握手返回 `403 subscription_sharing_route_not_supported`，网关会用 `426` 让固定版客户端以同一授权和同一模型回退 HTTP/SSE。其他权限失败和限流保留上游语义。已写出生成，或因断线、进程退出而无法确定是否写出时，网关记录该次提交，防止客户端自动改用另一传输重复发出请求。客户端明确引用上一轮交付的响应或工具调用才能推进相关续写。

上游成功响应缺少 `Content-Type` 时，网关先在有限字节和时限内确认首个 Responses SSE 事件，支持 LF、CRLF、CR 换行和流首的 UTF-8 BOM，再以 `text/event-stream` 向客户端转发事件。仅用 SSE 事件名标识的 Responses 事件会补齐客户端所需的 JSON `type`。无法确认流格式时保留提交结果未知的状态。HTTP 和流内错误保留上游错误种类、适用字段和重试信息。只有 `response.completed` 表示完成。流内失败、未完成、中断或提前断流分别保留实际状态，缺少上游 usage 时将用量视为未知。[固定版 Codex 回退逻辑](https://github.com/openai/codex/blob/3d86e43b075adeba2193ec5c22ac92f55afd82e6/codex-rs/core/src/responses_retry.rs) · [上游错误与恢复](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery)

## 上游公开支持边界

- **Sign in with Codex。** 官方文档覆盖在本人远端机器运行官方客户端的登录方式。App Server 文档说明已有本地或开源应用可继续使用其认证，建议迁移到 SIWC，商业或托管服务不在其许可范围内。公开资料对新第三方网关直接复用第一方 Codex 注册、长期持有多个授权并直连 Codex 后端的正式支持未确认。[官方远端登录](https://learn.chatgpt.com/docs/auth#login-on-headless-devices) · [App Server 认证说明](https://learn.chatgpt.com/docs/app-server#auth-endpoints)
- **Sign in with ChatGPT。** SIWC Terms §3.1 覆盖本人控制的远端 runtime，要求本人授权及只用于已连接应用，并限制套餐池化和规避限额。§1 则要求持久化认证令牌保存在本人控制的本地环境，排除远端或托管环境。自托管 VM 指南描述了在浏览器机器完成登录，经 SSH 转移受保护凭据，由 VM 负责刷新。Cloudex 的浏览器回环转发和远端控制面长期保存方式仍需 OpenAI 确认这些规则在该部署形态下的适用范围。[SIWC Terms §1、§3.1](https://openai.com/policies/sign-in-with-chatgpt-terms/) · [自托管 VM 指南](https://developers.openai.com/siwc/token-sharing-open-source/self-hosted-vms)

[工程验收](codex-gateway-e2e.md)记录客户端、假上游及受控拓扑的行为。正式支持需要独立的上游依据。
