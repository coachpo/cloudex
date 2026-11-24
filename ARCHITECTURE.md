# 架构

Cloudex 由一个控制面管理多台 Docker Host。Agent 模式由 Host 主动连接控制面，SSH 直连模式由控制面登录 Host，通过远端 Docker CLI 访问该机的 Docker socket。两种模式都使用 Host 的本地 socket，工作容器和控制台共享单个可信管理员的信任边界。[领域术语](docs/glossary.md)

## 模块与数据流

| 模块 | 职责与边界 |
|---|---|
| [Web 控制台](apps/web) | 提供 Host 管理、Workspace 放置与生命周期、SSH 接入、终端和 Git 快照。浏览器向同源控制面发起请求。 |
| [控制面入口](apps/api/cmd/server)与 [HTTP API](apps/api/internal/httpapi) | 接收 REST 和终端 WebSocket 请求，将页面请求转给 Next.js。同一进程提供共享 SSH Gateway。 |
| [调度](apps/api/internal/control)与[存储](apps/api/internal/store) | SQLite 保存 Workspace 的归属、资源预留、待执行操作和最后观测。控制面向归属 Host 派发操作，并按 Host 独立核对 Docker 实况。 |
| [Host Agent](apps/api/cmd/host-agent)、[Agent 通信](apps/api/internal/agentlink)与[容器运行时](apps/api/internal/runtime) | 以每台 Host 专属的证书建立 mTLS 连接，与控制面交换命令、心跳和数据流。Agent 核验受管容器归属后操作本机 Docker。 |
| [Host 连接管理](apps/api/internal/hostmanager)与[SSH 传输](apps/api/internal/hostssh) | 按归属 Host 选择接入方式。对 SSH 直连 Host，控制面校验登记时确认的主机公钥，通过 SSH 访问 Docker API，定期采集容量、镜像和连接状态。 |
| [SSH Gateway](apps/api/internal/remote)与[工作镜像](docker/Dockerfile.workspace) | Gateway 验证外层公钥，只允许向目标 Workspace 的 SSH 跳转。经归属 Host 的 Agent 或 SSH 通道转发到已核验的容器地址。容器内 OpenSSH 负责 `coder` 的公钥登录、文件传输和 Desktop App Server。root 和密码登录均禁用。 |

## Workspace 的归属与连接

创建时由管理员选择 Host。控制面接受放置后预留资源，并异步执行操作。同一 Workspace 的变更串行。控制面保留未确认操作和最后观测，待归属 Host 恢复连接后核对。[Workspace 生命周期](docs/workspace-lifecycle.md)

原生 SSH 与 Codex Desktop 的连接彼此独立。浏览器终端经控制台会话鉴权后连接容器 PTY。三种入口共用项目文件和 home 目录，两个目录各有命名卷。[SSH 接入与 Web Terminal](docs/workspace-access.md)

## 接口与决定

- [HTTP API](docs/api.md)给出路由、请求格式和领域契约的入口。
- [ADR 0003](docs/adr/0003-workspace-home-volume.md)记录 home 持久卷、初始 Codex 文件和登录凭据的存放边界。
- [ADR 0002](docs/adr/0002-workspace-repository-credential.md)记录 HTTPS 仓库令牌的使用范围、可见性与撤销边界。
- [运维](docs/operations.md)说明部署、Host 注册、故障恢复和数据保留。
