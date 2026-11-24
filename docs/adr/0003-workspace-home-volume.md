# 0003 持久化 Workspace 的 home 目录

- 状态：已接受，取代 [0001](0001-codex-home-managed-files.md)
- 日期：2026-11-23

## 背景

管理员通过 SSH、Codex Desktop 和 Web Terminal 使用 Workspace。容器替换后仍需使用原来的 Codex 登录、会话、本地配置和用户工具。

## 决定

- 每个 Workspace 使用独立的 `/home/coder` 命名卷。首次挂载空卷时从工作镜像复制 home 内容。Rebuild 和 Reset 都保留 home 和 SSH 主机密钥。Reset 只重建项目卷。删除时选择清除卷会删除三个卷。
- 管理员可在设置页保存初始 `AGENTS.md` 和 `config.toml`。Create 在首次启动前写入 home 中的 `~/.codex`。未设置的文件沿用镜像内容。
- 管理员在各 Workspace 中用 `codex login --device-auth` 登录。Codex 凭据保存在该 Workspace 的 home 中，控制台不收集或下发 `auth.json`。
- Git 仓库凭据存放在容器可写层。新容器取得当前设置，已清除设置时文件缺省。[ADR 0002](0002-workspace-repository-credential.md)

## 后果

容器替换会断开现有连接。重新连接后可继续使用 home 中的文件和 Codex 会话记录。备份或清除 home 卷时，应将其中的登录凭据与用户文件一并纳入考虑。[数据保留](../operations.md#数据保留)

## 复核条件

项目投入使用且有真实账号凭据需要保护，或部署引入多个管理员时，复核 home 卷的访问、备份及清除策略。
