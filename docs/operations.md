# 运维

## 安装与入口

首次安装按 [README](../README.md#启动) 构建工作镜像并启动 Compose。默认 Web 入口只绑定宿主机回环地址。远程访问优先使用 SSH 隧道，避免直接公开 Web、Gateway 或 Codex App Server。

使用 HTTPS 反向代理或更改 Web 端口时，把 `CLOUDEX_ORIGIN` 设为浏览器实际使用的 Origin，并允许 WebSocket 升级。Gateway 绑定地址须为宿主机具体的回环、局域网或 Mesh VPN IPv4 地址。各 Host 使用专用 Docker bridge。端口、环境变量和挂载默认值见[配置示例](../.env.example)、[控制面 Compose](../compose.yaml) 和[远程 Agent Compose](../compose.agent.yaml)。

### 投入使用

向外开放入口前，先从本机完成[管理员初始化](#管理员账号)。项目投入使用时更新[环境判定](../AGENTS.md#环境与数据操作)和[迁移基线](../CONTRIBUTING.md#兼容与迁移)。若已有须保护的真实账号凭据，按 [ADR 0001](adr/0001-codex-home-managed-files.md#复核条件) 和 [ADR 0002](adr/0002-workspace-repository-credential.md#复核条件)复核存储与下发方式。

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

## 管理 Workspace 的 Codex 文件

在 **Workspace → 设置** 启用托管并编辑文件全文。保留项目并应用新配置时选择 **Rebuild**。Reset 会重新初始化项目，详见[数据保留](#数据保留)。

需要保留登录时，粘贴当前 `auth.json`。同一份 ChatGPT 登录只用于一个 Workspace，刷新后旧副本可能失效。新容器无法登录时，重新粘贴有效的文件。凭据的可见性、适用形态和复核条件见 [ADR 0001](adr/0001-codex-home-managed-files.md)。

## 加入远程 Host

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

在 Host 详情页调整上限，同时查看 Agent 实际生效值。调低立即限制新派发，正在执行的操作继续。调高后要等 Agent 上报新值才会增加并发，无需重启。

## Host 调度

**Cordon host** 暂停新放置，已有 Workspace 和连接可继续使用。**Disable host** 断开 Agent 及其服务的 SSH 和 Web Terminal 连接，并暂停操作派发。已有容器继续运行。选择 **Resume scheduling** 恢复调度。

## 通过 SSH 连接 Workspace

跨机器访问时，Gateway 可绑定私网或 VPN 地址。使用默认回环地址和端口时，在客户端运行 `ssh -N -L 2222:127.0.0.1:2222 user@docker-host`，保持隧道开启。客户端需要单独的 Gateway 地址时，在 **设置 → 接入地址** 填写 **SSH 主机名**。

1. 在 **设置 → SSH 公钥** 填入本机 OpenSSH 公钥，每行一把。保存后，公钥立即用于新连接。可在同一处移除公钥。
2. 启动并等待 **Gateway ready**。将 **Copy known_hosts entries** 的两条公钥加入本机 `~/.ssh/known_hosts`，再把 **Copy SSH config** 的两个 Host 块放在 `~/.ssh/config` 的通用 `Host *` 规则之前。使用非默认私钥时，为两个 Host 块填写同一 `IdentityFile`，或使用 SSH agent。
3. 运行 `ssh <alias>` 验证完整链路。需要 Codex 时在远端执行 `command -v codex`。尚未登录可运行 `codex login --device-auth`，设备码登录须由账户或组织允许。[Codex 认证说明](https://learn.chatgpt.com/docs/auth#login-on-headless-devices)
4. 选择 **Add host in Codex Desktop**，或在桌面应用的 **Settings → Connections** 添加 SSH 别名，打开 `/workspace/project`。Desktop 使用远端 CLI 的登录并启动自己的 App Server。SSH、终端和 Desktop 共用工作树，避免同时编辑同一文件。[Codex Remote SSH 指南](https://learn.chatgpt.com/docs/remote-connections#connect-to-an-ssh-host)

## 健康检查与排障

- **控制面与 Host**　`/healthz` 只检查 HTTP 进程是否响应。在 **Hosts** 查看各 Host 的连接、最后心跳和镜像就绪情况。失联时已有容器继续运行，控制台保留最后观测。
- **SSH**　**Gateway ready** 表示服务端已核实运行中的容器和内层主机公钥。仍须用客户端别名实际登录。Gateway 不会自动启动已停止的容器。重建期间暂停新转发，旧连接随旧容器退出。
- **操作失败**　先看[操作详情](workspace-lifecycle.md#生命周期操作)，再用 `docker compose logs api` 查看启动、生命周期和 SSH 转发记录。Gateway 可记录转发起止，无法查看 Desktop 会话内容。

## 数据保留

| 操作 | 项目卷 | 已有的 Workspace SSH 主机密钥 | 容器内的 Codex 目录与 Git 凭据 |
|---|---|---|---|
| Stop / Start | 保留 | 保留 | 同一容器保留原内容。 |
| Rebuild | 保留 | 保留 | 新容器按镜像和当前托管设置重新生成。 |
| Reset project | 删除并重新初始化 | 保留 | 与 Rebuild 相同。 |
| Delete workspace | 默认保留，可显式清除 | 默认保留，可显式清除 | 随容器删除。 |

停止、重建和 Host 失联时预留继续有效。删除并保留卷时只释放 CPU 和内存。要释放磁盘预留，应在删除 Workspace 时选择清除卷。直接在 Docker 清卷不会更新预留，保留的卷也不会自动挂到新 Workspace。

需要保留数据时，分别保管控制面数据库、原 `CLOUDEX_MASTER_KEY`、各 Host 的 Agent 状态卷和 Workspace 持久卷。控制台不管理备份。数据库及备份含明文托管的 Codex 登录文件和仓库令牌，Gateway 和 Agent CA 私钥的恢复还需要原主密钥。项目数据、登录文件和私钥不得进入仓库或日志。[控制面卷](../compose.yaml) · [远程 Host 卷](../compose.agent.yaml) · [删除后的操作历史](workspace-lifecycle.md#删除与历史)
