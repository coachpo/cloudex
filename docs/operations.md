# 运维

## 安装与入口

首次安装按 [README](../README.md#启动) 构建工作镜像并启动 Compose。默认 Web 入口只绑定宿主机回环地址。远程访问优先使用 SSH 隧道，避免直接公开 Web、Gateway 或 Codex App Server。

使用 HTTPS 反向代理或更改 Web 端口时，把 `CLOUDEX_ORIGIN` 设为浏览器实际使用的 Origin，并允许 WebSocket 升级。Gateway 绑定地址须为宿主机具体的回环、局域网或 Mesh VPN IPv4 地址。各 Host 使用专用 Docker bridge。端口、环境变量和挂载默认值见[配置示例](../.env.example)、[控制面 Compose](../compose.yaml) 和[远程 Agent Compose](../compose.agent.yaml)。

### 投入使用

向外开放入口前，先从本机完成[管理员初始化](#管理员账号)。项目投入使用时更新[环境判定](../AGENTS.md#环境与数据操作)和[迁移基线](../CONTRIBUTING.md#兼容与迁移)。接入真实账号前，核对[上游公开支持边界](codex-gateway.md#上游公开支持边界)与实际部署方式。按 [ADR 0002](adr/0002-workspace-repository-credential.md#复核条件)复核仓库凭据的存储与下发方式。

### 预构建镜像

`cloudex-api`、`cloudex-agent`、`cloudex-web` 和 `cloudex-workspace` 在 `ghcr.io/coachpo` 下使用相同的 Cloudex 发布标签，每个镜像均支持 `linux/amd64` 和 `linux/arm64`。推送 `v*` 标签时发布原标签、语义化版本 `<主>.<次>.<修订>` 和 `<主>.<次>`、`sha-<完整提交>` 及 `latest`。手动构建使用 `manual-<完整提交>`。[镜像发布工作流](../.github/workflows/docker-images.yml)

Workspace 镜像在两种架构的原生 runner 上分别构建，核对镜像内 `codex --version` 与控制面固定的 `codexupstream.CodexClientVersion` 一致，再运行网关受控验收。两种架构均通过且 APP 镜像均构建成功后，工作流才发布多架构标签。升级 Codex 时同步修改 `docker/Dockerfile.workspace` 的 `CODEX_VERSION` 和 `CodexClientVersion`，只改一处会使发布失败。[候选镜像验收](codex-gateway-e2e.md#镜像与分机验证) · [Codex 构建版本](../docker/Dockerfile.workspace) · [控制面客户端版本](../apps/api/internal/codexupstream/client.go)

### 默认工作镜像

预构建的 `cloudex-api` 默认引用同一次发布的 `cloudex-workspace`。例如 `cloudex-api:v0.0.4` 内置 `ghcr.io/coachpo/cloudex-workspace:0.0.4`，手动构建内置对应的 `manual-<完整提交>` 标签。在各 Host 拉取控制面实际使用的镜像引用，并在控制台确认镜像就绪。控制面只检查 Host 上已有的镜像。

从源码使用 Compose 构建时，`CLOUDEX_WORKSPACE_IMAGE` 同时指定工作镜像的构建标签和 `api` 内置的默认引用，默认值为 `cloudex/workspace:local`。修改后重新构建 `workspace-image` 和 `api`。[Compose 配置](../compose.yaml) · [环境变量示例](../.env.example)

管理员在 **设置** 保存的镜像引用优先于内置默认值。切换前在各 Host 准备目标镜像，并确认就绪。已有 Workspace 继续使用创建时的镜像引用，在所在 Host 保留对应镜像。

## 管理员账号

- **首次初始化**　在本机创建唯一的管理员，成功后自动登录。空库的首位访问者即可创建账号，应在向其他人开放入口前完成初始化。
- **修改**　在 **设置 → 账号** 输入当前密码，再修改用户名或密码。新密码留空可保留原密码。保存后当前浏览器继续登录，其他会话失效。
- **找回**　先限制入口只允许自己访问，再在 Compose 宿主机运行下列命令。它清除现有管理员及所有会话。重新打开控制台创建管理员后，再开放入口。

```sh
docker compose exec api cloudex-server reset-admin
```

## Codex 网关

### 配置两条网络路径

- **默认 Compose 的本机 Host。** 使用 HTTP 回环控制台且 `CLOUDEX_GATEWAY_ORIGIN` 留空时，Workspace 在 Docker 私网通过 `http://cloudex-gateway:8081` 直连控制面。该端口只在 Workspace 网络监听，服务 `/g/v1`，不发布到宿主机。控制台、SSH Gateway、Agent 和 OAuth 回调在管理网络监听，沿用各自的宿主机端口绑定。若旧 Compose 的 `gateway` 容器仍在运行，移除该容器以释放 `cloudex-gateway` 网络别名。
- **自定义入口与远程 Host。** 将 `CLOUDEX_GATEWAY_ORIGIN` 设为所有需要使用托管授权的容器可解析、可通过受信任 TLS 连接的 HTTPS Origin，例如 `https://gateway.example.net`。留空时优先使用 HTTPS 控制台 Origin。自定义入口将 `/g/v1/` 原路径转给 Go 控制面，允许 WebSocket 升级和长连接，关闭 SSE 缓冲和代理自动重试。容器须信任该入口的证书链。远程 Host 需要可达的 HTTPS 入口，不能使用 Compose 的本机网络别名。从工作容器验证 DNS、TLS 和响应流。[配置示例](../.env.example) · [网关接口](api.md#workspace-推理网关)

若只启用了默认 Compose 的本机 HTTP 网关，远程 Host 上的 Workspace 仍可创建并使用 SSH、Git 和 Web Terminal，但新增或更改授权绑定会返回 `gateway_not_configured`。先配置该容器可达且信任的 HTTPS 推理入口，再重试绑定。

控制面需要能直接连接所选上游的 HTTPS 端点。上游 SSE 使用 HTTP/1.1，换绑准入覆盖请求写入连接的时点。入口反向代理只负责容器到控制面的链路。
- **浏览器到授权回调。** 浏览器回调方式默认使用 `http://127.0.0.1:1455/auth/callback`，Compose 只在控制面宿主回环发布。默认端口已占用时，用 `CLOUDEX_OAUTH_CALLBACK_PORT=1457` 并匹配本地隧道。若浏览器运行在另一台机器上且要自动完成回调，请先在浏览器所在机器运行下列命令。SSH 目的地是控制面宿主，工作容器所在的 Docker Host 无需访问回调端口。[手动提交回调 URL](#手动提交回调-url) · [回环回调转发](https://learn.chatgpt.com/docs/auth#fallback-forward-the-localhost-callback-over-ssh)

```sh
ssh -N -L 127.0.0.1:1455:127.0.0.1:1455 user@control-plane-host
```

两条路径中的代理、接入日志和追踪均应排除凭据。自动回调不得记录查询字符串，手动提交回调的 API 不得采集请求体。网关不得记录 Authorization、Cookie、提示词、流正文或工具参数。网关用内部 Workspace、授权、传输、阶段、请求 ID、终态和实际返回的用量排查问题。

### 添加授权和绑定 Workspace

1. 在 **设置 → Codex 授权** 选择 **Sign in with Codex** 或 **Sign in with ChatGPT**。浏览器回调不可达时，Codex 可选 **Device Code**，前提是个人账号或所在的 ChatGPT workspace 已启用设备码登录。在 Cloudex 发起后打开 OpenAI 验证链接并输入一次性用户码。控制面通过 HTTPS 轮询与换码，管理员电脑无需回环监听或 SSH 隧道。完成任一方式后，回到发起授权的控制台会话，核对并确认账号及适用的 workspace 或应用注册。ChatGPT 若未授予套餐推理权限，按授权行提示对同一注册重新授权。[Codex 设备码启用条件](https://learn.chatgpt.com/docs/auth#preferred-device-code-authentication-beta) · [上游公开支持边界](codex-gateway.md#上游公开支持边界)
2. 在 **Workspace → 设置** 选择一份授权。打开 Codex 前等待实例凭据就绪。换绑或轮换后按[恢复授权与凭据](#恢复授权与凭据)重新打开客户端。换绑后已发给旧上游的请求可能仍在执行。
3. 从工作容器的 Shell 执行 `codex --version` 核对实际运行的版本。交互 `codex`、`codex exec` 和 `codex app-server` 使用工作镜像的启动入口读取当前绑定的模型配置。使用 `-m` 时选择当前目录中的模型 slug。固定版本和镜像构建入口见[工作镜像](../docker/Dockerfile.workspace)。

```sh
codex exec --skip-git-repo-check --json 'Return CLOUDEX_OK.'
```

在固定客户端与受控假上游中验收协议和转发，再分别按两种渠道的[公开支持边界](codex-gateway.md#上游公开支持边界)核实真实环境。登录、模型目录、建立连接、完成工具往返和最终 `response.completed` 各自对应不同验证阶段。[验收矩阵](codex-gateway-e2e.md)

#### 手动提交回调 URL

两种浏览器登录均可使用。先在 Cloudex 发起授权并在 OpenAI 完成登录。若跳转到 `127.0.0.1` 后浏览器显示无法连接，从地址栏复制完整 URL，回到发起授权的控制台，选择 **手动提交回调 URL**，粘贴并提交，再核对身份并确认保存。管理员电脑无需监听回环端口或建立 SSH 隧道。此入口使用原授权的 `redirect_uri`，OpenAI 对两条渠道的手动提交方式的正式支持均未确认。

### 配置 Codex 启动文件

在 **设置 → Codex 配置** 编辑 `config.toml` 和 `AGENTS.md` 并保存。所有绑定授权且使用支持启动配置下发的工作镜像的 Workspace 共用这两份文件。保存后，新启动的 Codex 会话使用最新配置，Codex Desktop 重新连接远端 App Server 后生效。后续保存无需重建 Workspace。

Codex 或 Codex Desktop 写入 `CODEX_HOME/config.toml` 的内容会在下一次启动时被控制台保存的内容覆盖。需要信任项目时，把信任配置保存到控制台的 `config.toml`。

```toml
[projects."/workspace/project"]
trust_level = "trusted"
```

### 恢复授权与凭据

- **需重新授权。** 对受影响授权选择重新授权，使用原注册和同一已验证身份。刷新结果未知时需重新授权，以免再次使用可能已轮换的旧 refresh token。
- **绑定成功，容器待准备。** 在 Host 详情核对连接，在 Workspace 查看容器的实际状态。Host 恢复后控制面会重新检查容器和凭据安装。配置过 HTTPS Origin 后，仍无推理响应时，先从工作容器排查网关 DNS、服务器证书和反代。
- **换绑或轮换后恢复会话。** 等实例凭据就绪后，重启 Codex CLI 或让 Codex Desktop 重新连接远端 App Server。在同一容器中切回同一份授权的同一代次后，打开原会话即可用新凭据续聊。重授权会开启新的会话空间，容器重建会丢弃原会话文件。[换绑和上下文](codex-gateway.md#换绑和上下文)
- **推理提交结果未知。** 原上游可能仍在执行，也可能已完成。网关会拒绝原运行中的自动重发。检查本地工具是否已产生副作用，再决定是否从新的客户端运行发起请求。
- **断开授权。** 先查看受影响的 Workspace。断开立即停止本地转发，保留 Workspace 的绑定供识别影响范围。远端撤权结果未知时，控制台显示本地已断开、远端未确认。删除授权前先解除所有绑定。
- **套餐限流。** `subscription_sharing_usage_limit_exceeded` 表示本次授权的用量请求被拒，`subscription_sharing_usage_unavailable` 表示用量服务暂不可用。按当前授权恢复，保留服务报告的重试信息，其他授权仍按自己的状态工作。[错误与恢复契约](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery)

## 加入远程 Host

零保留规则下 SQLite 迁移基线可以重写，已执行的迁移版本不会重跑。控制面数据库来自旧基线时，先按[零保留约定](../CONTRIBUTING.md#兼容与迁移)重新初始化，再登记 Host。

同一 Docker Engine 只登记一次。容量和资源预留按 Host ID 统计，通过不同地址或接入方式重复登记会重复计算可用容量。在目标机[准备工作镜像](#默认工作镜像)后，按网络连接方向选择接入方式。

控制面能 SSH 登录目标 Host 并通过登录账号访问 Docker 时，SSH 直连可以管理 Workspace 和提供 Web Terminal。若目标 Host 的 sshd 禁止向容器转发 TCP，而使用者需要原生 SSH 或 Codex Desktop，可改用 Host Agent。此时要求 Agent 能向控制面建立出站连接，并能访问目标容器的 SSH 端口。

### SSH 直连

适用于控制面能访问目标机 SSH 服务的场景。远端需要 Linux Docker Engine 及 Docker CLI，供登录账号在非交互 SSH 会话中执行 `docker --host=unix:///var/run/docker.sock system dial-stdio`。该账号须能直接访问此 socket。原生 SSH 和 Codex Desktop 接入 Workspace 还要求目标机的 sshd 允许向专用 Docker bridge 的容器地址转发 TCP，并能访问其 `2222` 端口。

1. 在 **Hosts → Add host** 选择 **SSH 直连**，填写名称和控制面可达的 SSH 地址（例如 `build.example.com:22`）。选择 **获取主机指纹**。
2. 通过目标机控制台或主机管理员核对显示的 SHA256 指纹。主机控制台可用 `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` 查看 ED25519 密钥指纹；若协商的是其他密钥，应检查相应的公钥文件。
3. 确认指纹后填写远端用户名，选择密码或私钥认证，加密私钥还需填写口令。提交时控制面验证登录和 Docker 权限，检查 `CLOUDEX_SSH_NETWORK` 指定的 bridge，缺失时创建。等 **Hosts** 显示在线、所选镜像就绪，在 **New workspace** 中选择该主机。

控制面将登录凭据用 `CLOUDEX_MASTER_KEY` 加密后存入数据库。轮换凭据时，在**原 Host** 详情页选择 **更新 SSH 凭据**，保留登记的地址和用户名，重新核验指纹后输入新凭据。保存会重建控制连接，进行中的终端和 SSH 会话需重新连接。

Docker 盘点失败而 SSH 仍可达时，Host 显示为 `degraded`，暂停新放置和操作派发。选择 **重新连接** 可立即重试。若先前已在线，已有终端和 Gateway 转发继续使用原连接；若首次盘点尚未成功，控制面会重新建立 SSH 连接。

如果目标机出示的主机公钥发生变化，控制面会停止自动重连。目标机恢复原密钥后，可在 Host 详情页手动重连，沿用已确认的公钥。如果主机确已换钥，通过可信渠道核对新指纹，再选择 **更新 SSH 凭据** 保存新密钥。

### Host Agent

1. 将控制面的 `CLOUDEX_AGENT_BIND` 设为目标 Host 可达的私网地址并用 Compose 应用。在 **设置 → 接入地址** 填写 **Agent 公开地址**（`host:port`）。目标 Host 只需能向该地址建立出站连接，也可通过隧道连接。
2. 在目标 Host [准备控制面使用的工作镜像](#默认工作镜像)。从源码构建时，用与控制面相同的仓库提交和 `CLOUDEX_WORKSPACE_IMAGE`，执行 `docker compose -f compose.agent.yaml --profile build build workspace-image`。`CLOUDEX_SSH_NETWORK` 须与控制面一致。
3. 在 **Hosts → Add host** 领取一次性注册令牌，有效期 15 分钟。下载 `agent-ca.pem`，放在目标机的 `compose.agent.yaml` 旁，或用 `CLOUDEX_AGENT_CA_PATH` 指向已有文件。
4. 在目标 Host 设置 `CLOUDEX_AGENT_ADDRESS` 和 `CLOUDEX_AGENT_ENROLLMENT_TOKEN`，启动 Agent。**Hosts** 显示 **Online** 后，从环境移除注册令牌并重新创建 Agent 容器，保留其状态卷。

   ```sh
   docker compose -f compose.agent.yaml up -d --build
   ```

Agent 会在证书到期前自动续期。新证书用于重连，已有连接继续使用。证书有效期受签发 CA 到期日限制。[签发期限](../apps/api/internal/agentauth/authority.go) · [续期窗口](../apps/api/internal/agentauth/client.go)

证书过期或私钥遗失时，在**原 Host** 详情页重发令牌，再用它重启该 Host 的 Agent。这样可保留原 Host ID 和 Workspace 归属。**Disabled** 会断开连接并暂停派发。

替换凭据时保留 Agent 的 `commands.sqlite`，其中记录已完成的清卷命令，确保操作重试时不会再次清理已重建的卷。

## 主机磁盘预留容量

在 Host 详情页的 **工作区容量** 中按 GiB 设置。保存后立即用于新放置，调低容量保留原有预留。设为 `0` 时，仅能放置磁盘预留为零的 Workspace。

## 主机操作并发上限

在 Host 详情页调整上限并查看实际生效值。调低立即限制新派发，正在执行的操作继续。调高后，Agent Host 等待 Agent 上报新值，SSH 直连 Host 等待控制面完成下一次状态采集。

## Host 调度

**Cordon host** 暂停新放置，已有 Workspace 和连接可继续使用。**Disable host** 断开该 Host 的控制连接及其服务的 SSH 和 Web Terminal 连接，并暂停操作派发。已有容器继续运行。选择 **Resume scheduling** 恢复调度。

## 移除远程 Host

先删除归属该 Host 的 Workspace，等待未完成操作结束或取消可取消的操作。随后在远程 Host 的详情页选择 **删除主机**，核对名称和影响后确认。内置 `local` 受保护。删除会一并清理该登记的 Workspace 操作历史，远端机器、Agent 部署及 Docker 容器、卷、镜像和网络继续保留。[删除范围](api.md#删除-host)

若页面显示删除结果待确认，选择 **重新检查**。页面确认主机仍在登记后即可重试。移除成功后，可按[加入远程 Host](#加入远程-host)用原名称登记，新登记会获得新的 Host ID。

## 通过 SSH 连接 Workspace

跨机器访问时，可把共享 SSH Gateway 绑定到控制面宿主机的私网或 VPN 地址。使用默认回环绑定时，在客户端运行 `ssh -N -L 127.0.0.1:2222:127.0.0.1:2222 user@control-plane-host` 并保持隧道开启。直接访问私网或 VPN 入口时，在 **设置 → 接入地址** 填写客户端可达的 **SSH 主机名**，再从 Workspace 复制连接配置。

1. 在 **设置 → SSH 公钥** 填入本机 OpenSSH 公钥，每行一把。保存后，公钥立即用于新连接。可在同一处移除公钥。
2. 启动并等待 **Gateway ready**。将 **Copy known_hosts entries** 的两条公钥加入本机 `~/.ssh/known_hosts`，再把 **Copy SSH config** 的两个 Host 块放在 `~/.ssh/config` 的通用 `Host *` 规则之前。使用非默认私钥时，为两个 Host 块填写同一 `IdentityFile`，或使用 SSH agent。
3. 运行 `ssh <alias>` 验证完整链路。使用 Codex 时，在远端执行 `codex --version` 核对客户端，再按[授权绑定步骤](#添加授权和绑定-workspace)运行。
4. 选择 **Add host in Codex Desktop**，或在桌面应用的 **Settings → Connections** 添加 SSH 别名，打开 `/workspace/project`。Desktop 启动远端 App Server，并使用该容器的 Codex 配置。SSH、终端和 Desktop 共用工作树，避免同时编辑同一文件。[Codex Remote SSH 指南](https://learn.chatgpt.com/docs/remote-connections#connect-to-an-ssh-host)

使用名称作为 SSH 别名的 Workspace 删除后以同名重建，会沿用同一个 Host 名。用新配置替换 `~/.ssh/config` 中对应的两个旧 Host 块。

## 健康检查与排障

- **控制面与 Host**　`/healthz` 只检查 HTTP 进程是否响应。在 **Hosts** 查看各 Host 的连接、最近观测和镜像就绪情况。SSH 直连失败时，按详情页错误检查地址连通性、主机公钥、登录凭据、远程 Docker CLI、socket 权限或工作网络。失联时已有容器继续运行，控制台保留最后观测。
- **SSH**　**Gateway ready** 表示服务端已核实运行中的容器和内层主机公钥。仍须用客户端别名实际登录。Gateway 不会自动启动已停止的容器。重建期间暂停新转发，旧连接随旧容器退出。
- **操作失败**　先看[操作详情](workspace-lifecycle.md#生命周期操作)，再用 `docker compose logs api` 查看启动、生命周期和 SSH 转发记录。Gateway 可记录转发起止，无法查看 Desktop 会话内容。

Workspace 冷启动报 `workspace SSH daemon did not start` 时，先查容器日志核对 sshd 开始监听的时间，再检查 Host 同期是否在构建或解包镜像。保持工作镜像与容器配额相同，在 Host 空闲时复测，分别记录首次创建与重试的结果。

## 数据保留

| 操作 | 项目卷 | 容器 home | SSH 主机密钥卷 | Git 凭据文件 |
|---|---|---|---|---|
| Stop / Start | 保留 | 同一容器保留 home | 保留 | 同一容器保留原文件。 |
| Rebuild | 保留 | 新容器从镜像初始化 | 保留 | 新容器使用当前设置。 |
| Reset project | 删除并重新初始化 | 新容器从镜像初始化 | 保留 | 新容器使用当前设置。 |
| Delete workspace | 默认保留，可显式清除 | 随容器删除 | 默认保留，可显式清除 | 随容器删除。 |

表中的 home 位于容器可写层。

停止、重建和 Host 失联时预留继续有效。删除并保留卷时只释放 CPU 和内存。要释放磁盘预留，应在删除 Workspace 时选择清除卷。直接在 Docker 清卷不会更新预留，保留的卷也不会自动挂到新 Workspace。

需要保留数据时，分别保管控制面数据库、原 `CLOUDEX_MASTER_KEY`、使用 Agent 的 Host 的 Agent 状态卷，以及各 Host 的 Workspace 持久卷。控制台不管理备份。数据库及备份含明文仓库令牌、加密的托管授权和 ChatGPT 注册使用的安装标识。恢复 SSH Gateway、Agent CA 或解密 SSH 直连凭据和上游令牌都需要原主密钥。实例网关凭据仅随当前容器的运行时内存文件存续。项目数据、登录文件和私钥须远离仓库和日志。[控制面卷](../compose.yaml) · [远程 Agent 卷](../compose.agent.yaml) · [删除后的操作历史](workspace-lifecycle.md#删除与历史)
