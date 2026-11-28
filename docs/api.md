# HTTP API

业务接口位于 `/api/v1`。首次初始化和登录向未登录客户端开放。管理接口要求有效的管理员会话。写请求与控制台 WebSocket 握手须携带与 `CLOUDEX_ORIGIN` 一致的 `Origin`。OAuth 回调与容器推理使用各自的鉴权契约。进程探针另见[健康检查与排障](operations.md#健康检查与排障)。

## 路由与数据格式

完整的方法、路径、鉴权分组、JSON 成功与错误响应及请求标识见[路由定义](../apps/api/internal/httpapi/router.go)。WebSocket 的消息格式见[终端协议](workspace-access.md#web-terminal)。

| 调用方需要处理的业务 | 请求与响应的权威入口 |
|---|---|
| 初始化、登录、会话和修改账号 | [账号和 Cookie](../apps/api/internal/httpapi/auth.go) |
| 默认镜像、接入地址和 Codex 启动配置 | [控制面设置](../apps/api/internal/httpapi/settings.go) · [Codex 启动配置](#codex-启动配置) |
| Host 登记、删除、凭据更新、调度状态、盘点、容量和并发 | [Host 接口](../apps/api/internal/httpapi/hosts.go)、[删除 Host](#删除-host)、[SSH 直连接口](../apps/api/internal/httpapi/ssh_hosts.go)与[Host 状态](../apps/api/internal/store/hosts.go) |
| Codex 授权、Workspace 绑定和实例凭据 | [管理 API 源码](../apps/api/internal/httpapi/grants.go) · [授权和绑定](#codex-授权和绑定) · [推理请求](#workspace-推理网关) |
| Workspace 创建、放置、操作回执、历史、日志、指标与 Git | [Workspace 接口](../apps/api/internal/httpapi/workspaces.go)与[生命周期契约](workspace-lifecycle.md) |
| 管理员公钥与 Workspace 连接信息 | [公钥接口](../apps/api/internal/httpapi/ssh.go)与[SSH 接入契约](workspace-access.md#ssh-接入信息) |

首次初始化成功即建立会话。修改账号须验证当前密码，留空新密码可保留原密码。成功后旧会话失效，调用者须接受响应中的新 Cookie。[账号操作](operations.md#管理员账号)

SSH 公钥设置接受 `public_keys` 数组，替换全局列表。空数组撤销全部公钥。Workspace 的 Access 接口提供连接信息及是否配置公钥，其中 `alias` 是 SSH 配置的 [Host 名](workspace-access.md#ssh-接入信息)。在线 Host 下发失败时，返回 `SSH_KEY_SYNC_FAILED`，已保存的公钥列表仍用于 Gateway 鉴权。

SSH 主机名和 Agent 公开地址一起提交，空字符串清除对应设置。SSH 配置在主机名为空时使用绑定地址，新签发的注册材料使用当前 Agent 公开地址。默认工作镜像的修改立即适用于模板和新建的 Workspace。Host 可以报告多个镜像的放置就绪状态，实际 Docker 镜像清单另行查询。[默认工作镜像](operations.md#默认工作镜像)

Cordon 或禁用的 Host 也可调整磁盘预留容量和操作并发上限。容量变更保留已有预留，并发调整保留在途操作。新的派发受管理员设定值和最近一次生效值中较小者约束。[磁盘预留](operations.md#主机磁盘预留容量) · [操作并发](operations.md#主机操作并发上限)

## Codex 启动配置

`GET /api/v1/settings` 在设置响应中返回 `codex_config_toml` 和 `codex_agents_md`，两者都是字符串。`PUT /api/v1/settings/codex` 接受 `{"config_toml":"…","agents_md":"…"}`，两项必填，整体替换并在成功响应中回显该对象。空字符串清除对应文件的配置。

每项最多 32 KiB，且不得含 NUL。`config_toml` 须符合启动器支持的 TOML 1.0，日期年份从 `0001` 起。顶层保留键为 `model_provider`、`model_providers`、`model_catalog_json`、`cli_auth_credentials_store`、`openai_base_url` 和 `chatgpt_base_url`。字段校验失败返回 `400 INVALID_CODEX_SETTINGS`，消息指出出错文件、字段或保留键。[网关下发](#workspace-推理网关)

## 删除 Host

`DELETE /api/v1/hosts/{host}` 按 ID 移除远程 Agent 或 SSH 直连 Host 的登记，无需请求体。适用通常的 `Origin` 和管理员会话校验，不符时分别返回 `403` 和 `401`。

| 响应 | 含义 |
|---|---|
| `204` | 登记已删除。Host 从列表和 Workspace 放置选项消失，旧 Agent 令牌和证书失效，保存的 SSH 登录凭据被清除，管理连接关闭。 |
| `404 NOT_FOUND` | Host ID 不存在或登记已经移除。 |
| `409 HOST_LOCAL_PROTECTED` | 内置 `local` 不可移除。 |
| `409 HOST_HAS_PENDING_OPERATIONS` | Host 上仍有 `queued` 或 `running` 操作，包括已派发、结果待确认后重新排队的操作。 |
| `409 HOST_HAS_WORKSPACES` | Host 上仍有未删除的 Workspace，无论其运行状态如何。 |

连接和调度状态不影响删除资格。删除会清除该 Host 的登记令牌、容量和镜像就绪缓存，以及归属它的已删除 Workspace。清除范围包括这些 Workspace 的容器记录、操作历史、幂等记录、预留和专属凭据。其他 Host、全局设置和共享 Codex 授权均保留。远端机器、Agent 部署和 Docker 资源均保留。

原操作 ID 查询返回 `404`，原 Workspace 的操作列表返回空数组，原幂等键无法取回已删除的操作。名称可用于新的 Agent 或 SSH 登记，新登记获得新的 Host ID。登记名称必须唯一，比较时忽略 ASCII 字母的大小写。

依赖冲突或删除事务失败时，原登记、管理连接及关联记录均保留。响应丢失时，可通过 `GET /api/v1/hosts/{host}` 核对登记是否仍存在。

## SSH 直连 Host

下列路径均相对于 `/api/v1`。Host 的 `connection_type` 为 `agent` 或 `ssh`。SSH Host 的响应含地址、用户名和已固定的主机公钥，密码、私钥及私钥口令只写。

| 方法与路径 | 请求和成功结果 |
|---|---|
| `POST /hosts/ssh/probe` | 发送 `address`（含端口），在用户认证前读取主机公钥。返回 `200`、OpenSSH 格式的 `host_key` 和 SHA256 `fingerprint`。管理员须通过独立可信渠道核对指纹。 |
| `POST /hosts/ssh` | 发送 `name`、`address`、`username`、已核对的 `host_key`，以及 `password` 或 `private_key`（可附 `passphrase`）。验证登录、Docker 和专用 bridge 后，返回 `201`、Host 和 `Location`。验证过程可在远端创建缺失的工作网络。 |
| `PUT /hosts/{id}/ssh` | 对 SSH Host 提交原地址、原用户名、核对后的主机公钥和完整新凭据。验证成功返回 `200` 和原 ID 的 Host，并重建控制连接。 |
| `POST /hosts/{id}/ssh/reconnect` | 已在线时返回当前状态。先前在线的 Host 降级时，在原连接上重新检查 Docker，保留已有会话。离线或首次盘点尚未成功时，用已保存的凭据和主机公钥重新建连。 |

探测失败返回 `502 SSH_PROBE_FAILED`。验证时如远端密钥与请求中的公钥不同，返回 `409 SSH_HOST_KEY_CHANGED`；认证、Docker 或 bridge 检查失败返回 `422 SSH_CONNECTION_FAILED`。已登记的 SSH Host 在后续连接失败时通过 `connection_error` 报告原因。[SSH 直连接口](../apps/api/internal/httpapi/ssh_hosts.go)

## Workspace 仓库凭据

管理接口返回配置状态和用户名，令牌只写。[凭据接口](../apps/api/internal/httpapi/repository_credential.go)定义输入格式与删除响应。[仓库凭据](workspace-lifecycle.md#仓库凭据)说明创建重试、更新和撤销对已有容器的影响。

## Codex 授权和绑定

授权管理路径均相对于 `/api/v1`。Bearer、refresh token、ID token 和 OAuth 换码材料不通过管理 API 回读。账号显示字段用于辨识，授权身份以已验证的注册、subject 和适用的上游 workspace 为准。

| 方法与路径 | 外部契约 |
|---|---|
| `GET /grants` | 返回控制面已保存的授权渠道、注册、已验证身份、状态及引用它的 Workspace。读取列表不会发起上游查询、令牌刷新或试推理。Workspace 的运行实例就绪状态从绑定接口读取。 |
| `POST /grant-authorizations` | 提交 `channel`（`codex` 或 `chatgpt`）。重新授权时同时提交 `grant_id`，渠道及预期身份从现有授权确定。默认 `method: "browser"` 返回 `id` 和 `authorization_url`。仅 `codex` 可指定 `method: "device_code"`，返回 `id` 和 `device`，其中含 `verification_url` 与 `user_code`。 |
| `GET /grant-authorizations/{id}` | 发起授权的管理员会话查询尝试进度。换码和身份校验成功后返回待确认的身份。 |
| `POST /grant-authorizations/{id}/callback` | 发起浏览器授权的原会话可提交 `{"callback_url":"…"}`。回调地址与 `state` 必须匹配该尝试。与自动回调共享一次性 `state`。换码及身份校验成功返回 `204`，仍须查询待确认身份并确认保存。 |
| `DELETE /grant-authorizations/{id}` | 发起会话取消未过期的设备码尝试并停止轮询。成功返回 `204`。 |
| `POST /grant-authorizations/{id}/confirm` | 发起会话核对身份后确认。添加已有授权或为原授权误选另一身份会报错，现有授权与绑定保持原值。 |
| `POST /grants/{id}/disconnect` | 本地撤销该授权使用权，按原渠道尝试远端撤权。回读状态区分远端确认与结果未知。 |
| `DELETE /grants/{id}` | 删除无引用授权。有引用时返回 `409` 和受影响 Workspace。 |
| `GET /workspaces/{id}/grant-binding` | 返回保存的授权、`binding_revision` 及实例就绪状态。响应 `ETag` 可用于更新。 |
| `PUT /workspaces/{id}/grant-binding` | 以 `{"grant_id":"…"}` 绑定授权。提交前核对目标的目录与推理权限，要求 `If-Match: "<binding_revision>"`。 |
| `DELETE /workspaces/{id}/grant-binding` | 要求同样的 `If-Match`，解绑后保留版本。 |
| `POST /workspaces/{id}/gateway-credential/rotate` | 撤销旧实例凭据并准备新凭据，保持授权与绑定版本。 |

创建 Workspace 时可在 `POST /workspaces` 中提供 `grant_id`，由放置请求一起保存初始绑定。省略时可在 Workspace 创建后绑定。[创建接口](../apps/api/internal/httpapi/workspaces.go)

重试已生效的绑定目标会返回当前绑定。带有旧版本且会改变目标的请求返回版本冲突。换绑成功时旧实例凭据不能再使用。回环监听器接收 `GET /auth/callback`，按一次性 `state` 匹配渠道和授权尝试。设备码尝试在控制面轮询，最长有效 15 分钟。两种方式的待确认结果都只能由最初的管理员会话读取和保存。[回环网络](operations.md#配置两条网络路径) · [身份与刷新契约](codex-gateway.md#授权身份和刷新)

## Workspace 推理网关

| 方法与路径 | 外部契约 |
|---|---|
| `GET /g/v1/models` | 使用实例 Bearer 返回当前授权下适用于固定版 Codex 的目录。 |
| `GET /g/v1/codex-settings` | 使用同一实例 Bearer 返回当前全局启动配置，响应为 `application/json`，对象含 `config_toml` 和 `agents_md`，响应头为 `Cache-Control: no-store`。文件未配置时对应值为空字符串。此请求只读取控制面设置。 |
| `GET /g/v1/responses` | 使用同一 Bearer 请求 WebSocket Upgrade。成功建立上游连接且仍符合准入条件后才能收到 `101`。上游在提交生成前明确拒绝 Upgrade 时返回 `426`。 |
| `POST /g/v1/responses` | 使用同一 Bearer 提交 HTTP Responses，正常响应以 SSE 流交付。需要继续工具调用时，客户端按适用协议回传结果和历史。 |

实例凭据只对当前绑定和运行实例有效。客户端应保持本次启动的运行标识，明确的续轮才能越过可能已提交的记录。出现 `submission_outcome_unknown` 时，同一运行或 turn 可能已向上游发出生成。仅 `response.completed` 确认推理完成。模型能力错误应检查请求中的模型与字段，认证和限流错误按当前授权处理。[传输、回退和错误契约](codex-gateway.md#流式转发)
