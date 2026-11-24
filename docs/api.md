# HTTP API

业务接口位于 `/api/v1`。首次初始化和登录向未登录客户端开放，其余业务接口要求有效的管理员会话。写请求与 WebSocket 握手须携带与 `CLOUDEX_ORIGIN` 一致的 `Origin`。进程探针另见[健康检查与排障](operations.md#健康检查与排障)。

## 路由与数据格式

完整的方法、路径、鉴权分组、JSON 成功与错误响应及请求标识见[路由定义](../apps/api/internal/httpapi/router.go)。WebSocket 的消息格式见[终端协议](workspace-access.md#web-terminal)。

| 调用方需要处理的业务 | 请求与响应的权威入口 |
|---|---|
| 初始化、登录、会话和修改账号 | [账号和 Cookie](../apps/api/internal/httpapi/auth.go) |
| 默认镜像和接入地址 | [控制面设置](../apps/api/internal/httpapi/settings.go) |
| Host 登记、凭据更新、调度状态、盘点、容量和并发 | [Host 接口](../apps/api/internal/httpapi/hosts.go)、[SSH 直连接口](../apps/api/internal/httpapi/ssh_hosts.go)与[Host 状态](../apps/api/internal/store/hosts.go) |
| 新 Workspace 的初始 Codex 文件 | [初始 Codex 文件](../apps/api/internal/httpapi/codex_files.go)与[生效时机](workspace-lifecycle.md#初始-codex-文件) |
| Workspace 创建、放置、操作回执、历史、日志、指标与 Git | [Workspace 接口](../apps/api/internal/httpapi/workspaces.go)与[生命周期契约](workspace-lifecycle.md) |
| 管理员公钥与 Workspace 连接信息 | [公钥接口](../apps/api/internal/httpapi/ssh.go)与[SSH 接入契约](workspace-access.md#ssh-接入信息) |

首次初始化成功即建立会话。修改账号须验证当前密码，留空新密码可保留原密码。成功后旧会话失效，调用者须接受响应中的新 Cookie。[账号操作](operations.md#管理员账号)

SSH 公钥设置接受 `public_keys` 数组，替换全局列表。空数组撤销全部公钥。Workspace 的 Access 接口提供连接信息及是否配置公钥。在线 Host 下发失败时，返回 `SSH_KEY_SYNC_FAILED`，已保存的公钥列表仍用于 Gateway 鉴权。

初始 Codex 文件设置接收 `agents_md` 和 `config_toml` 两个字符串。空字符串使用镜像对该文件的默认内容。

SSH 主机名和 Agent 公开地址一起提交，空字符串清除对应设置。SSH 配置在主机名为空时使用绑定地址，新签发的注册材料使用当前 Agent 公开地址。默认工作镜像的修改立即适用于模板和新建的 Workspace。Host 可以报告多个镜像的放置就绪状态，实际 Docker 镜像清单另行查询。[默认工作镜像](operations.md#默认工作镜像)

Cordon 或禁用的 Host 也可调整磁盘预留容量和操作并发上限。容量变更保留已有预留，并发调整保留在途操作。新的派发受管理员设定值和最近一次生效值中较小者约束。[磁盘预留](operations.md#主机磁盘预留容量) · [操作并发](operations.md#主机操作并发上限)

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
