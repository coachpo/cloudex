# 领域术语

| 术语 | 在 Cloudex 中的含义 |
|---|---|
| Host | 可承载 Workspace 的 Docker 主机，接入方式为 Agent 或 SSH 直连。连接状态表示控制面能否通过该接入方式访问 Docker，调度状态决定是否接受新放置和是否继续派发操作。 |
| Workspace | 具有独立身份、工作树和 SSH 身份的工作环境。创建时确定归属 Host，更换容器后仍使用原 Workspace ID。 |
| 代次（Generation） | 同一 Workspace 的某一代工作容器。Rebuild 和 Reset 会产生新代次。 |
| 运行实例 | 已核验的 Host、Workspace、容器代次、Docker ID 和本次启动标记。同一容器 Stop 后重新 Start 是新的运行实例。 |
| 上游 workspace | 授权渠道的服务端身份与策略边界。一个 Cloudex Workspace 引用的授权最多对应一个已知上游 workspace。 |
| 应用注册 | 上游授权服务器登记的应用身份。Codex 使用固定注册，ChatGPT 首次授权取得签发给该注册的 client ID。 |
| 托管授权（Grant） | 控制面持有的渠道、应用注册、已验证身份、权限和令牌组。多个 Workspace 可显式绑定同一授权。 |
| 绑定版本（Binding revision） | 一个 Cloudex Workspace 的授权选择版本。换绑或解绑提交时前进。 |
| 资源预留 | Host 承诺给 Workspace 的 CPU、内存和磁盘容量，与实时用量分别计算。 |
| Operation | 一次可独立查询结果的异步生命周期操作。操作结果与 Workspace 当前的观测状态分别保存。 |
| Cordon | 暂停 Host 的新放置。该 Host 上已有的 Workspace 可继续使用。 |
