# Cloudex

Cloudex 是自托管的多主机 AI Coding Workspace 平台。管理员选择 Docker Host 创建 Workspace，再通过 Codex Desktop、原生 SSH 或 Web Terminal 使用同一工作树。项目和 home 目录分别保存在持久卷中。

## 项目现状

项目尚未投入使用，面向单个可信管理员。控制台可以查看各 Host 的容量、镜像和 Workspace 状态。磁盘预留用于放置，容器没有磁盘硬配额。控制面无法判断 Codex Desktop 的当前 Turn 是否空闲。数据保留政策见[贡献指南](CONTRIBUTING.md#兼容与迁移)。

## 启动

准备 Linux Docker Engine 和 Docker Compose v2。macOS 可使用运行 Linux 容器的独立 Docker 环境。Remote SSH 已在标准 rootful Docker 上验证。建议使用 Docker Engine 28 或更新版本，旧版本发布到回环的端口可能被同一二层网络上的其他机器访问。[Docker 端口发布说明](https://docs.docker.com/engine/network/port-publishing/)

首次安装且尚无配置时运行以下命令。

```sh
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

将生成的值填入 `.env` 的 `CLOUDEX_MASTER_KEY`，按[数据保留说明](docs/operations.md#数据保留)保管。然后构建工作镜像并启动控制面。

```sh
docker compose --profile build build workspace-image
docker compose up -d --build
```

1. 从本机打开[控制台](http://localhost:3000)。首次访问时创建管理员账号，之后用已有账号登录。
2. 确认 **Hosts** 中目标 Host 在线。在 **New workspace** 选择 Host 和资源，创建空仓库或填写 HTTPS 仓库地址与分支。私有仓库可单独填写凭据。
3. 在 **设置 → SSH 公钥** 添加客户端公钥，再按[连接步骤](docs/operations.md#通过-ssh-连接-workspace)验证两层主机密钥并打开 `/workspace/project`。也可以在 **Web terminal** 打开 Shell。

## 文档

- [架构与契约](ARCHITECTURE.md)涵盖模块边界、术语、接口和决策记录。
- [运维](docs/operations.md)涵盖管理员账号、远程 Host 注册、调度、数据保留和恢复。
- [贡献指南](CONTRIBUTING.md)说明开发、文档、验证和提交约定。
- [变更记录](CHANGELOG.md)按历史记录用户可见的变化。
