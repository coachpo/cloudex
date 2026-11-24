# 变更记录

尚无版本标签。下列提交用于定位历史变更。

## Workspace 工作流（未发布）

- 控制台和 API 已移除 Cloud Task、全局模型服务和 Codex 沙箱设置。通过 SSH、Web Terminal 或 Codex Desktop 使用 Workspace，Git 页可查看工作树快照。
- 可为每个 Workspace 保存 HTTPS 仓库凭据。首次 clone 和后续 fetch、push 可复用令牌，控制台只回显配置状态和用户名。[仓库凭据](docs/workspace-lifecycle.md#仓库凭据)
- 可在控制台编辑 Codex 托管文件。新容器可沿用保存的登录与配置，旧容器内的本地改动随容器替换而清除。[Codex 文件](docs/operations.md#管理-workspace-的-codex-文件)

## 管理员账号与界面语言（未发布）

- 首次从控制台创建管理员，重启后沿用原账号。更改用户名或密码后当前浏览器继续登录，其他会话失效。丢失凭据可用 `reset-admin` 重新初始化。[账号找回](docs/operations.md#管理员账号)
- 控制台按浏览器语言显示简体中文或英文，包括错误、日期和数字格式。主题选择可跨访问保留。
- 可在控制台设置 SSH 主机名、Agent 公开地址、各 Host 的磁盘预留容量和操作并发上限。这些更改无需重启，磁盘容量改由主机详情页设置，原 `CCC_AGENT_DISK_BYTES` 配置已移除。[Host 容量](docs/operations.md#主机磁盘预留容量)
- 默认工作镜像可在设置页切换，已有 Workspace 沿用创建时的镜像。
- 当时的后台任务支持设置 Codex 沙箱及使用 API 密钥的自定义 Responses API 服务，可选模型和请求头，凭据不回显。未指定服务时沿用 OpenAI。这些能力随后随 Cloud Task 移除。
- 产品更名为 Cloudex，镜像、配置变量前缀（`CCC_` → `CLOUDEX_`）和 SSH 别名随之更名。旧数据、旧版 Agent 和逐 Workspace 发布的 SSH 端口停止受支持。

## 多 Host 管理 — `e05e54e`、`8cb5bee`、`1b97801`、`af7b3df`

- 可统一注册、盘点本地与远程 Docker Host，创建 Workspace 时手动选址并预留 CPU、内存和磁盘。既有 Workspace 归入本机 Host，保留原代次、卷、操作历史和 SSH 身份。
- 生命周期操作和当时的后台任务在归属 Host 执行。慢 Host 不会阻塞其他 Host 的状态更新。Agent 证书自动续期，已建立的 SSH、终端及任务连接继续使用。
- Agent 已连接但 Docker 不可用时，Host 显示为 `degraded`，实时视图和新任务按离线处理。

## 生命周期与 Cloud Task 恢复 — `d6f3587`、`f6ed40c`、`dee3550`

- 后台任务可显示收尾状态，中断后可恢复已记录的结果，确认启动被拒绝的任务可重新排队。
- Start 在发出前失败或 Stop 失败时，操作历史显示失败，Workspace 继续显示实际状态。
- Web Terminal 支持粘贴大段文字。概览在生命周期按钮旁提示是否需要重建或验证 SSH。

## SSH Gateway 与 Cloud Task 观测 — `2e5ad61`

- Workspace 改用共享 SSH Gateway 并保留独立主机密钥。既有公钥与历史保留。V1.1 直连容器须重建并更新客户端 SSH 配置后切换。
- 当时的 Cloud Task 支持按 Workspace 和状态筛选、查看计数及线程、Turn、事件和用量快照，观测范围仅限后台任务。

## V1.1：Codex Desktop 直连 SSH — `998c29b`

- Workspace 支持公钥管理、独立 SSH 端口和 Codex Desktop 导入，重建后保留 SSH 主机身份。Access 页可复制 OpenSSH 配置和主机公钥。

## 初始单机版 — `650cfa5`

- 支持通过 Docker Compose 运行，提供 Workspace 生命周期、Web Terminal、Codex 后台任务、日志回放和 Git 快照。
- 支持 OpenAI API Key 和 Codex CLI 账号认证。
