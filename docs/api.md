# HTTP API

业务接口位于 `/api/v1`。首次初始化和登录向未登录客户端开放，其余业务接口要求有效的管理员会话。写请求与 WebSocket 握手须携带与 `CLOUDEX_ORIGIN` 一致的 `Origin`。进程探针另见[健康检查与排障](operations.md#健康检查与排障)。

## 路由与数据格式

完整的方法、路径、鉴权分组、JSON 成功与错误响应及请求标识见[路由定义](../apps/api/internal/httpapi/router.go)。WebSocket 的消息格式见[终端协议](workspace-access.md#web-terminal)。

| 调用方需要处理的业务 | 请求与响应的权威入口 |
|---|---|
| 初始化、登录、会话和修改账号 | [账号和 Cookie](../apps/api/internal/httpapi/auth.go) |
| 默认镜像和接入地址 | [控制面设置](../apps/api/internal/httpapi/settings.go) |
| Host 登记、凭据续发、调度状态、盘点、容量和并发 | [Host 接口](../apps/api/internal/httpapi/hosts.go)与[Host 状态](../apps/api/internal/store/hosts.go) |
| Workspace 创建、放置、操作回执、历史、日志、指标与 Git | [Workspace 接口](../apps/api/internal/httpapi/workspaces.go)与[生命周期契约](workspace-lifecycle.md) |
| 公钥替换与连接信息 | [公钥接口](../apps/api/internal/httpapi/ssh.go)与[SSH 接入契约](workspace-access.md#ssh-接入信息) |

首次初始化成功即建立会话。修改账号须验证当前密码，留空新密码可保留原密码。成功后旧会话失效，调用者须接受响应中的新 Cookie。[账号操作](operations.md#管理员账号)

SSH 主机名和 Agent 公开地址一起提交，空字符串清除对应设置。SSH 配置在主机名为空时使用绑定地址，新签发的注册材料使用当前 Agent 公开地址。默认工作镜像的修改立即适用于模板和新建的 Workspace。Host 可以报告多个镜像的放置就绪状态，实际 Docker 镜像清单另行查询。[默认工作镜像](operations.md#默认工作镜像)

Cordon 或禁用的 Host 也可调整磁盘预留容量和操作并发上限。容量变更保留已有预留，并发调整保留在途操作。新的派发受管理员设定值和 Agent 最近上报值中较小者约束。[磁盘预留](operations.md#主机磁盘预留容量) · [操作并发](operations.md#主机操作并发上限)

## Workspace Codex 文件

接口接受并返回托管文件的全文。输入格式与大小限制见[配置接口](../apps/api/internal/httpapi/codex_config.go)，修订冲突和生效时机见[用户级 Codex 配置](workspace-lifecycle.md#用户级-codex-配置)。

## Workspace 仓库凭据

管理接口返回配置状态和用户名，令牌只写。[凭据接口](../apps/api/internal/httpapi/repository_credential.go)定义输入格式与删除响应。[仓库凭据](workspace-lifecycle.md#仓库凭据)说明创建重试、更新和撤销对已有容器的影响。
