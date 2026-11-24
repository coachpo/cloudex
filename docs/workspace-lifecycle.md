# Workspace 生命周期

## 创建与放置

创建时选择 Host，并提供 `Idempotency-Key`。可以初始化本地 Git 仓库，或按指定分支克隆 HTTPS 仓库，默认分支为 `main`。远程地址须与凭据分开，不能包含查询参数或片段。可选择初始化后是否保持运行。输入字段、资源单位、默认值及按 Host 查询的方法见[创建接口](../apps/api/internal/httpapi/workspaces.go)。

容量预览反映最近一次观测。创建时 Host 须健康在线、允许放置、已备妥所选镜像，且有足够的可预留容量。容量不足时，响应给出短缺的资源、请求量和可用量。[放置条件](../apps/api/internal/store/reservations.go) · [错误响应](../apps/api/internal/httpapi/router.go)

## 生命周期操作

- 提交成功返回 `202`。通过 `Location` 查询操作结果。创建和生命周期请求支持幂等重试。同一键可取回原操作，复用键时请求参数须一致，创建时的[仓库令牌](#仓库凭据)除外。
- Reset 和 Delete 都须确认 Workspace ID。只有 Delete 可显式清除保留卷。各操作对数据的影响见[数据保留](operations.md#数据保留)。
- Docker 暂停的 Workspace 可以 Stop，须解除暂停才能 Start。Start 在发往 Docker 前失败时，操作记为失败，期望和观测状态保留。若 Docker 可能已启动容器，控制面会重新核验当前代次与 SSH 就绪状态，已记录的失败仍可查询。
- Stop 失败时，期望状态记为停止，操作记为失败。控制面根据 Docker 的实际状态更新观测，Workspace 仍可继续操作。
- 只有尚未尝试的排队操作可以取消。取消尚未派发的 Create 会释放预留。Agent 失联或执行结果待确认时，控制面可以重新派发同一操作，保留目标代次和预留。

## 仓库凭据

私有仓库可在创建时单独提交用户名和令牌。用相同幂等键重试，若仅令牌发生变化，则返回原操作并保留原令牌。首次 clone 和之后的 fetch、push 都可使用与 HTTPS 主机及端口匹配的凭据，匹配范围包括该主机上的其他仓库。[凭据范围与理由](adr/0002-workspace-repository-credential.md)

保存或清除凭据适用于此后 Create、Rebuild或 Reset生成的新容器。同一容器 Stop / Start 继续使用原凭据。需要立即失效时，到代码托管平台吊销令牌。删除 Workspace 或取消未派发的 Create 会清除控制面中的凭据。[凭据接口](api.md#workspace-仓库凭据)

## 用户级 Codex 配置

默认关闭托管，首次读取时返回标准工作镜像预置的 TOML，其他文件内容为空。保存须提交当前修订及完整配置，修订冲突时先重新读取。已保存的修订与已应用的修订可分别查询。[配置接口](api.md#workspace-codex-文件)

启用托管后，新容器在首次启动前取得配置。关闭托管时使用镜像内容。对现有容器的本地修改只在该容器中保留，控制台不会回读。同一容器的停启或重启保留内容。需要应用新配置并保留项目时使用 Rebuild。[托管决定](adr/0001-codex-home-managed-files.md)

有排队或运行中的生命周期操作时，Workspace 拒绝更改 Codex 配置和仓库凭据。

## Git 快照

运行中 Workspace 的快照显示当前提交、工作树状态和已跟踪文件相对 `HEAD` 的补丁。未跟踪文件仅列出路径。快照会标记输出截断，完整内容可在终端用 Git 查看。[快照格式与上限](../docker/workspace-control.py)

## 实时指标

只有运行中的容器且 Docker 已提供样本时才有实时指标。调用方可从错误区分样本暂不可用、容器已不存在和 Agent 失联。[指标接口](../apps/api/internal/httpapi/workspaces.go) · [运行时错误](../apps/api/internal/httpapi/router.go)

## 删除与历史

删除后仍可按 Workspace 或操作 ID 查询操作历史。Workspace 详情和容器历史随删除关闭。选择清除卷时，删除操作等待归属 Host 确认卷已清理。
