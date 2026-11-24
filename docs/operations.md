# 运维

## 安装与入口

首次安装按 [README](../README.md#启动) 构建工作镜像并启动 Compose。默认 Web 入口只绑定宿主机回环地址。远程访问优先使用 SSH 隧道，避免直接公开 Web、Gateway 或 Codex App Server。

使用 HTTPS 反向代理或更改 Web 端口时，把 `CLOUDEX_ORIGIN` 设为浏览器实际使用的 Origin，并允许 WebSocket 升级。Gateway 绑定地址须为宿主机具体的回环、局域网或 Mesh VPN IPv4 地址。各 Host 使用专用 Docker bridge。端口、环境变量和挂载默认值见[配置示例](../.env.example)、[控制面 Compose](../compose.yaml) 和[远程 Agent Compose](../compose.agent.yaml)。

### 投入使用

向外开放入口前，先从本机完成[管理员初始化](#管理员账号)。项目投入使用时更新[环境判定](../AGENTS.md#环境与数据操作)和[迁移基线](../CONTRIBUTING.md#兼容与迁移)。若已有须保护的真实账号凭据，按 [ADR 0003](adr/0003-workspace-home-volume.md#复核条件) 和 [ADR 0002](adr/0002-workspace-repository-credential.md#复核条件)复核存储与下发方式。

### 预构建镜像

使用 GHCR 的 `linux/arm64` 镜像时，为 `ghcr.io/coachpo/cloudex-api`、`ghcr.io/coachpo/cloudex-agent`、`ghcr.io/coachpo/cloudex-web` 和 `ghcr.io/coachpo/cloudex-workspace` 选择同一发布标签。

原手册记载，推送 `v*` 标签会发布原标签、语义化版本标签、`sha-<完整提交>` 和 `latest`，手动构建只发布 `manual-<完整提交>`。当前标签约定未经核实。[镜像工作流](../.github/workflows/docker-images.yml)

### 默认工作镜像

先在各 Host 构建或拉取工作镜像，在控制台检查各 Host 对该镜像的就绪状态，再到 **设置** 切换默认镜像。`CLOUDEX_WORKSPACE_IMAGE` 只指定 Compose 构建标签。已有 Workspace 继续使用创建时的镜像引用，在所在 Host 保留对应镜像。

## 管理员账号

- **首次初始化**　在本机创建唯一的管理员，成功后自动登录。空库的首位访问者即可创建账号，应在向其他人开放入口前完成初始化。
- **修改**　在 **设置 → 账号** 输入当前密码，再修改用户名或密码。新密码留空可保留原密码。保存后当前浏览器继续登录，其他会话失效。
- **找回**　先限制入口只允许自己访问，再在 Compose 宿主机运行下列命令。它清除现有管理员及所有会话。重新打开控制台创建管理员后，再开放入口。

```sh
docker compose exec api cloudex-server reset-admin
```

## 初始 Codex 文件与登录

在 **设置 → 新工作区的初始 Codex 文件** 填写 `AGENTS.md` 和 `config.toml`。后续新建的 Workspace 在首次启动前收到这些文件。要修改已有 Workspace，请在其 home 目录中直接编辑。

在每个 Workspace 运行一次 `codex login --device-auth`。登录状态、Codex 会话和 home 中的用户工具会随 home 命名卷保留。[持久化决定](adr/0003-workspace-home-volume.md)

## 加入远程 Host

此版本更新了 SQLite 基线。已有旧基线的开发环境须按项目的[零保留约定](../CONTRIBUTING.md#兼容与迁移)重新初始化，再登记 Host。

同一 Docker Engine 只登记一次。容量和资源预留按 Host ID 统计，通过不同地址或接入方式重复登记会重复计算可用容量。在目标机[准备工作镜像](#默认工作镜像)后，按网络连接方向选择接入方式。

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
2. 在目标 Host 准备默认工作镜像。可拉取同一镜像，或在同版本仓库执行 `docker compose -f compose.agent.yaml --profile build build workspace-image`。`CLOUDEX_SSH_NETWORK` 须与控制面一致。
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

## 通过 SSH 连接 Workspace

跨机器访问时，Gateway 可绑定私网或 VPN 地址。使用默认回环地址和端口时，在客户端运行 `ssh -N -L 2222:127.0.0.1:2222 user@docker-host`，保持隧道开启。客户端需要单独的 Gateway 地址时，在 **设置 → 接入地址** 填写 **SSH 主机名**。

1. 在 **设置 → SSH 公钥** 填入本机 OpenSSH 公钥，每行一把。保存后，公钥立即用于新连接。可在同一处移除公钥。
2. 启动并等待 **Gateway ready**。将 **Copy known_hosts entries** 的两条公钥加入本机 `~/.ssh/known_hosts`，再把 **Copy SSH config** 的两个 Host 块放在 `~/.ssh/config` 的通用 `Host *` 规则之前。使用非默认私钥时，为两个 Host 块填写同一 `IdentityFile`，或使用 SSH agent。
3. 运行 `ssh <alias>` 验证完整链路。需要 Codex 时在远端执行 `command -v codex`。尚未登录可运行 `codex login --device-auth`，设备码登录须由账户或组织允许。[Codex 认证说明](https://learn.chatgpt.com/docs/auth#login-on-headless-devices)
4. 选择 **Add host in Codex Desktop**，或在桌面应用的 **Settings → Connections** 添加 SSH 别名，打开 `/workspace/project`。Desktop 使用远端 CLI 的登录并启动自己的 App Server。SSH、终端和 Desktop 共用工作树，避免同时编辑同一文件。[Codex Remote SSH 指南](https://learn.chatgpt.com/docs/remote-connections#connect-to-an-ssh-host)

## 健康检查与排障

- **控制面与 Host**　`/healthz` 只检查 HTTP 进程是否响应。在 **Hosts** 查看各 Host 的连接、最近观测和镜像就绪情况。SSH 直连失败时，按详情页错误检查地址连通性、主机公钥、登录凭据、远程 Docker CLI、socket 权限或工作网络。失联时已有容器继续运行，控制台保留最后观测。
- **SSH**　**Gateway ready** 表示服务端已核实运行中的容器和内层主机公钥。仍须用客户端别名实际登录。Gateway 不会自动启动已停止的容器。重建期间暂停新转发，旧连接随旧容器退出。
- **操作失败**　先看[操作详情](workspace-lifecycle.md#生命周期操作)，再用 `docker compose logs api` 查看启动、生命周期和 SSH 转发记录。Gateway 可记录转发起止，无法查看 Desktop 会话内容。

## 数据保留

| 操作 | 项目卷 | home 卷 | SSH 主机密钥卷 | Git 凭据文件 |
|---|---|---|---|---|
| Stop / Start | 保留 | 保留 | 保留 | 同一容器保留原文件。 |
| Rebuild | 保留 | 保留 | 保留 | 新容器使用当前设置。 |
| Reset project | 删除并重新初始化 | 保留 | 保留 | 新容器使用当前设置。 |
| Delete workspace | 默认保留，可显式清除 | 默认保留，可显式清除 | 默认保留，可显式清除 | 随容器删除。 |

停止、重建和 Host 失联时预留继续有效。删除并保留卷时只释放 CPU 和内存。要释放磁盘预留，应在删除 Workspace 时选择清除卷。直接在 Docker 清卷不会更新预留，保留的卷也不会自动挂到新 Workspace。

需要保留数据时，分别保管控制面数据库、原 `CLOUDEX_MASTER_KEY`、使用 Agent 的 Host 的 Agent 状态卷，以及各 Host 的 Workspace 持久卷。控制台不管理备份。数据库及备份含明文仓库令牌，home 卷及其备份含 Workspace 内的登录凭据。恢复 Gateway 和 Agent CA 私钥、解密 SSH 直连凭据都需要原主密钥。项目数据、登录文件和私钥不得进入仓库或日志。[控制面卷](../compose.yaml) · [远程 Agent 卷](../compose.agent.yaml) · [删除后的操作历史](workspace-lifecycle.md#删除与历史)
