# 领域术语

| 术语 | 在 Cloudex 中的含义 |
|---|---|
| Host | 可承载 Workspace 的 Docker 主机，接入方式为 Agent 或 SSH 直连。连接状态表示控制面能否通过该接入方式访问 Docker，调度状态决定是否接受新放置和是否继续派发操作。 |
| Workspace | 具有独立身份、工作树和 SSH 身份的工作环境。创建时确定归属 Host，更换容器后仍使用原 Workspace ID。 |
| 代次（Generation） | 同一 Workspace 的某一代工作容器。Rebuild 和 Reset 会产生新代次。 |
| 资源预留 | Host 承诺给 Workspace 的 CPU、内存和磁盘容量，与实时用量分别计算。 |
| Operation | 一次可独立查询结果的异步生命周期操作。操作结果与 Workspace 当前的观测状态分别保存。 |
| Cordon | 暂停 Host 的新放置。该 Host 上已有的 Workspace 可继续使用。 |
