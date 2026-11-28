# Codex 网关验收

默认验收的客户端为 Codex `0.172.0`，对应 Codex 上游源码提交 `3d86e43b075adeba2193ec5c22ac92f55afd82e6`。先在被测入口运行 `codex --version`。本机执行的固定二进制、隔离 Docker 镜像内的客户端、真实部署中的远程 Host 是三种验证环境，分别记录。

## 固定客户端与受控上游

从 `apps/api` 运行下列测试。二进制应来自官方固定版发行。测试使用隔离的临时 home、合成授权和本地假上游。[固定客户端验收源码](../apps/api/cmd/server/gateway_acceptance_test.go) · [Codex 0.172.0 发行](https://github.com/openai/codex/releases/tag/rust-v0.172.0)

```sh
CLOUDEX_TEST_CODEX_BIN=/absolute/path/to/codex go test -count=1 -v ./cmd/server -run '^TestPinnedCodexGatewayMatrix$/native'
```

每格均执行两个有依赖的工具步骤。假上游发工具调用 1，收到结果后结合该结果发工具调用 2，再收到工具 2 结果并交付终态。记录实际目标路径、合成身份、模型、每步 `call_id`、终态和实际 usage，省略凭据值。一次登录、目录非空、`101` 或 `200` 分别只证明对应阶段。

| 渠道与传输 | 检查请求 | 完成判据 |
|---|---|---|
| Codex × WS | `/backend-api/codex/responses`，Codex 测试身份及其账号头与 beta 标识。专属路由单独检查目标 origin。 | 同一连接上两次配对的工具往返，最终 `response.completed`。 |
| Codex × SSE | 同路径同身份的 POST，完整工具历史。分别检查生成前拒绝 Upgrade 后回退和直接 HTTP。 | 同一模型两次配对的工具往返，最终 `response.completed`。 |
| ChatGPT × WS | `/v1/responses`，ChatGPT issued 注册的合成身份，连接内续写，公开 WS 请求形状。 | 同一连接上两次配对的工具往返，最终 `response.completed`。 |
| ChatGPT × SSE | `/v1/responses`，同一 issued 注册，`store:false`、`stream:true`、完整历史、无 `previous_response_id`。 | 同一模型两次配对的工具往返，最终 `response.completed`。 |

| 故障注入或状态变更 | 需要观察的结果 |
|---|---|
| 生成前拒绝 Upgrade | 假上游只收到一次握手，返回 `426` 或 ChatGPT 专用的 `403 subscription_sharing_route_not_supported` 后，收到同一授权同一模型的 SSE。WS 未收到生成。 |
| SSE 上游省略媒体类型 | 省略 `Content-Type` 的 CR 和 BOM 流以 `text/event-stream` 转发。固定客户端核对同一身份下的两次工具往返和终态。 |
| 生成已送达后断流、丢失终态和控制面退出 | 原运行的自动重连或 SSE 重试遭拒。假上游看到的生成次数仍为一次。 |
| 同一轮工具往返中触发自动压缩，两渠道各测 WS 与强制 SSE | 首次生成的 usage 超过客户端的自动压缩阈值。压缩请求带回工具结果并完成，下一上下文窗口的续写恰好送达假上游一次，最终 `response.completed`。WS 在新窗口重新握手一次。 |
| W1/W2 共享 A，另加 B | W1/W2 并发只兑换一次 A，B 独立可用，添加 B 不影响原绑定或流。 |
| 刷新落库前退出；重授权后旧刷新回包 | 未确认的兑换阻止再次使用旧 refresh token，迟到结果不能修改新授权。已明确临时失败与明确永久失效分别校验。 |
| A 重新授权误选 B；撤销 A | 误选保持两份记录和绑定。撤销只停止 A，并验证其渠道的撤权字段、请求次数及远端不确定时的本地状态。 |
| W1 换绑 B，W2 保持 A，两个方向都测 | 在拨号前、上游 `101` 后但下游 `101` 前、生成/工具写入边界、SSE 在途分别暂停。提交后 A 不收到 W1 新获准的写入，B 不收到 A 的旧会话，W2 继续 A。提交前已交给网络的数据单独核对。 |
| 限流和上游不可用 | 记录原渠道、请求实际次数、状态码、错误码和重试信息。另一个授权请求仍使用它自己的身份。 |

### Codex 启动配置

本机场景 `native/chatgpt-startup-settings` 需要 PATH 上的 Python 3.11 或更新版本。通过管理员 API 保存含 MCP 服务器的 `config.toml` 和全局 `AGENTS.md`，用仓库的启动器运行固定版 Codex。两份文件须按原文落盘，ChatGPT 假上游的首个生成请求须含全局指令及编码为 `additional_tools` 的 MCP 工具。清空设置后再次启动须写入空配置，并移除原指令文件。

### 换回原授权的受控回归

从 `apps/api` 运行以下测试，分别验证 WebSocket 和 SSE。切到 B 时网关拒绝 A 的历史引用。切回 A 后，客户端用新凭据提交带原工具调用及结果的续聊请求。受控上游应恰好收到一次该请求，客户端应收到 `response.completed`。使用首次绑定 A 时的凭据应收到 `401`。[换绑回归源码](../apps/api/internal/codexgateway/transport_test.go)

```sh
go test -count=1 -v ./internal/codexgateway -run '^TestGatewayRebindRestoresCompletedToolHistory$'
```

## 镜像与分机验证

使用独立、可丢弃的 Docker daemon。从仓库根目录构建工作镜像。把 `CLOUDEX_TEST_GATEWAY_DNS` 设为工作容器可解析且能够连接到测试进程的 DNS 名或 IPv4 地址，例如 Docker Desktop 提供的 `host.docker.internal`。测试进程使用临时端口和测试 CA，工作容器内通过正式启动器运行客户端。Docker 测试由可丢弃容器提供工具执行隔离，测试命令显式选择 Codex 的 `danger-full-access`。生产镜像的 Codex 沙箱默认值不受测试选项影响。

```sh
docker build -f docker/Dockerfile.workspace -t cloudex/workspace:codex-0.172.0 .
cd apps/api
CLOUDEX_TEST_DOCKER=1 CLOUDEX_TEST_GATEWAY_DNS=host.docker.internal \
  go test -count=1 -v ./cmd/server -run '^TestPinnedCodexGatewayMatrix$/docker'
```

验证待发布的镜像时，在同一 Docker daemon 加载候选镜像，并同时设置 `CLOUDEX_TEST_WORKSPACE_IMAGE` 和 `CLOUDEX_TEST_CODEX_VERSION`，分别指向候选镜像引用和预期版本。上面的 Docker 矩阵会通过镜像内的正式启动器校验版本并验证网关协议。受控验收使用合成授权和本地假上游。 [Workspace 镜像工作流](../.github/workflows/workspace-image.yml)

分机验收时保持浏览器、控制面和 Docker Host 的网络路径可区分。Agent mTLS 与 SSH 直连分别跑一次。在工作容器内验证 DNS、TLS、Upgrade、SSE flush、`codex --version`、`codex exec` 及 `codex app-server`。浏览器回环须到达控制面宿主。[Docker 验证入口](../CONTRIBUTING.md#docker-验收) · [网络配置](operations.md#配置两条网络路径)

受控 Host 接入测试使用真实 mTLS、SSH 和 Docker，在同一个隔离 daemon 上建立两条独立的连接。它验证凭据安装、确认重试、轮换、旧安装拒绝及 Stop / Start 后的 tmpfs。已构建上述镜像时，将 `DOCKER_HOST` 指向该 daemon 的 Unix socket，从 `apps/api` 运行。[Host 接入测试](../apps/api/internal/agentlink/gateway_credential_test.go)

```sh
DOCKER_HOST=unix:///absolute/path/to/test-docker.sock \
  CLOUDEX_TEST_DOCKER=1 CLOUDEX_TEST_SSH_NETWORK=cloudex-gateway-test \
  go test -count=1 -v ./internal/agentlink -run '^TestGatewayCredentialHostRoutes$'
```

覆盖 Create、Start、Stop、Rebuild、Reset、Delete、主动轮换、控制面重启、Host 离线和迟到安装确认。每个案例分别记录实际 Docker ID、启动标记、凭据状态及假上游收到的身份和生成次数。旧凭据和连接在相应边界失效。Host 恢复后仅当前实例与绑定获得可用凭据。

验收默认 Compose 时，从工作容器所在网络请求默认推理入口的 `/g/v1/models`。使用无效实例凭据时，应收到 `401 invalid_gateway_credential`，可据此确认 DNS、HTTP 路由和网关鉴权已接通。再检查同一容器无法连接控制台、共享 SSH Gateway、Agent 或 OAuth 回调的管理端口，同时确认这些入口仍可从原先配置的宿主机地址访问，且推理端口未发布到宿主机。需要确认推理时，使用绑定合成授权的实际 Workspace 完成工具往返并收到 `response.completed`。

## 真实账号验收

真实上游验收需要获授权的本人账号、应用注册和确切操作范围。每个渠道分别验证回调身份、scope、模型目录、同一模型 WS 和 SSE 两格的两次工具往返、终态，以及一次实际刷新。要求专属路由的 Codex workspace 分别验证目录与 Responses 的最终路由。记录上游请求 ID；服务端未回显 workspace 时标为未知。故障和竞态由受控上游提供确定性证据。OpenAI 对部署形态的正式支持独立于以上结果。[公开支持边界](codex-gateway.md#上游公开支持边界)

### 换回原授权的镜像复验

在[隔离 Docker 环境](#镜像与分机验证)使用待验收的工作镜像和两份获准用于验收的授权 A、B，分别跑 WebSocket 和 SSE。每轮保持 Workspace 容器运行，沿用两份授权的原代次。每次换绑都等新凭据就绪，再从正式启动器运行新的 Codex 进程。WebSocket 轮使用默认传输，SSE 轮在每次 `codex` 调用中传入 `-c model_providers.cloudex.supports_websockets=false`。

在 Workspace 到网关的链路上采集请求的传输、状态和终态事件类型。使用观测代理时须保留 WebSocket 升级和 SSE flush。[日志约定](operations.md#配置两条网络路径)

1. 首次绑定 A 时运行 `codex exec --json`，让工具生成一个随机 UUID，要求客户端只回复固定确认语，使 UUID 仅出现于工具结果。记录会话 ID 和该结果。在测试进程中保留首次绑定 A 的网关凭据，供本轮失效检查。
2. 换绑 B 并完成一轮请求，再切回 A。用 `codex exec resume --json` 和本轮原会话 ID 续聊，要求直接复述先前工具返回的 UUID，提示词里省略该值。成功时 UUID 一致，新增工具调用为零，上游收到续聊请求且客户端收到 `response.completed`。
3. 用本轮首次绑定 A 时的旧凭据请求 `/g/v1/models`，核对 `401 invalid_gateway_credential`。同时核对实际传输。WebSocket 轮须观测到 `101` 和经 WebSocket 送达的终态，且 `POST /g/v1/responses` 的 SSE 请求数为零。SSE 轮须观测到 SSE 送达的终态。证据只记录必要的请求与结果元数据，省略账号信息和凭据值。

## 当前验收记录

按[验证改动](../CONTRIBUTING.md#验证改动)从各行的通过提交选择场景，场景通过后更新对应行。未列出的场景（如[换回原授权的受控回归](#换回原授权的受控回归)）尚无通过记录。

换绑复验原记录的 `af4d97e` 在仓库历史重建后不可达。它与主线 `375a5720a57556ac3fb4401d19bf768ec8ce5277` 只差 `.md` 文件，因此改指该提交。真实账号验收原记录的合成版 `8427356` 同样不可达。主线上与它差异最小的 `7c2b288` 仍缺少 `apps/api/internal/codexgateway/stream_sse.go`、`stream_ws.go` 及相应测试中的传输改动，没有等价提交，按没有通过记录处理。受控验收的原记录没有写受验提交。

| 场景 | 层与环境 | 最近通过的提交 | 结果与证据 |
| --- | --- | --- | --- |
| [固定客户端与受控上游](#固定客户端与受控上游)、[镜像与分机验证](#镜像与分机验证)的受控矩阵 | 本机固定二进制与隔离 Colima Docker，`codex-cli 0.172.0`，合成授权与假上游 | 无，原记录未写受验提交 | 原生矩阵 9 项和 Docker 矩阵 10 项均通过，明细见[受控执行记录](#受控执行记录)。 |
| [Codex 启动配置](#codex-启动配置) | 本机 `codex-cli 0.172.0`、Python 3.14.6、合成授权与 ChatGPT WS 假上游 | `faccae222c6168013d0ba7315b771c48c3e6ed40` | 原生矩阵 16 项通过。首个生成请求含 `CLOUDEX_GLOBAL_AGENTS` 与 `additional_tools` 中的 `fixture_ping`。写入和清除后的文件符合预期。Docker 本轮未运行。 |
| [固定客户端与受控上游](#固定客户端与受控上游)的同轮自动压缩 | 本机 `codex-cli 0.172.0`、Python 3.14.6、合成授权与假上游 | `faccae222c6168013d0ba7315b771c48c3e6ed40` | 原生矩阵 16 项通过，其中两渠道 WS 与强制 SSE 的压缩场景各 1 项。每项的压缩请求带回 `call_fixture_1`，新窗口续写恰好送达一次并完成。Docker 本轮未运行。 |
| [真实账号验收](#真实账号验收)：两渠道 WS 与 SSE 的工具往返及撤权 | 独立 Colima Docker，`codex-cli 0.172.0`，`gpt-6.2-sol` | 无，原记录的提交不可达且没有等价提交 | 通过，明细见[真实账号执行记录](#真实账号执行记录)。该轮未覆盖正常到期刷新、Codex 专属 workspace 路由和实际分机 Host。 |
| [换回原授权的镜像复验](#换回原授权的镜像复验) | 隔离的本地 Colima Docker，`codex-cli 0.172.0`，`gpt-6.2-sol` | `375a5720a57556ac3fb4401d19bf768ec8ce5277` | 通过，WebSocket 与 SSE 两轮 A → B → A，明细见[真实账号执行记录](#真实账号执行记录)。 |
| [真实账号验收](#真实账号验收)：Sign in with Codex 的同轮自动压缩 | 本地 Colima Docker 的 Compose 部署，Workspace 镜像 `cloudex/workspace:codex-0.172.0`，`codex-cli 0.172.0`，`gpt-6.2-sol` | `44ae2fa85e1349da6cb237983bcc4d0904230af4` | WS 与强制 SSE 均通过，明细见[真实账号执行记录](#真实账号执行记录)。没有可用的 Sign in with ChatGPT 授权，该渠道未覆盖。 |

### 受控执行记录

固定版原生二进制与隔离 Colima Docker 内的工作镜像均运行 `codex-cli 0.172.0`。原生矩阵 9 项和 Docker 矩阵 10 项均通过。Docker 客户端经真实 tmpfs 安装和 `codex` 启动器，通过 TLS 连接宿主上的 Go 路由。假上游记录如下。[客户端验收源码](../apps/api/cmd/server/gateway_acceptance_test.go)

| 渠道 | 传输 | 实际目标路径 | 合成身份及模型 | 工具结果与终态 |
|---|---|---|---|---|
| Codex | WS | `/backend-api/codex/responses` | `fixture-codex-user` / `fixture-workspace-codex`，`gpt-6.1-sol` | 同一连接回传 `call_fixture_1`、`call_fixture_2`，最终 `response.completed` |
| Codex | SSE | `/backend-api/codex/responses` | `fixture-codex-user` / `fixture-workspace-codex`，`gpt-6.1-sol` | POST 完整历史配对 `call_fixture_1`、`call_fixture_2`，最终 `response.completed` |
| ChatGPT | WS | `/v1/responses` | `fixture-chatgpt-subject` / `fixture-issued-client`，`gpt-6.2-sol` | 同一连接回传 `call_fixture_1`、`call_fixture_2`，最终 `response.completed` |
| ChatGPT | SSE | `/v1/responses` | `fixture-chatgpt-subject` / `fixture-issued-client`，`gpt-6.2-sol` | POST 完整历史配对 `call_fixture_1`、`call_fixture_2`，最终 `response.completed` |

四格的三次生成分别返回输入和输出 usage `10/5`、`11/6`、`12/7`，`codex exec` 报告合计 `33/18`。两条 SSE 路径另行强制 HTTP 验证。生成前拒绝 Upgrade 时，客户端只向原授权发起 SSE。生成已接收后的断流和终态丢失各只向假上游提交一次，客户端自动发起的 HTTP 重试在网关收到 `409 submission_outcome_unknown`。

隔离 Docker 另以 PTY 启动默认交互式 `codex`，并以 stdio 启动 `codex app-server`。两个入口各完成两步工具链。交互入口使用其授权目录的默认模型。浏览器侧回环与 SSH 转发测试使用 `CLOUDEX_TEST_GATEWAY_LOOPBACK=1 go test -count=1 -v ./cmd/server -run '^TestGrantGatewayLoopbackCallback$'`。

- 换绑在两个方向验证了实际拨号前、上游 `101` 与下游确认之间、网络写入和在途 SSE。仅 W1 改绑，旧工具结果无法继续转发，W2 继续使用 A。刷新故障测试覆盖 W1/W2 共享一次兑换、已兑换但未落库后恢复、重授权后迟到结果，以及独立授权不受撤销影响。[转发竞态测试](../apps/api/internal/codexgateway/transport_test.go) · [刷新和撤销测试](../apps/api/internal/codexgateway/service_test.go)
- Agent mTLS 与固定主机密钥的 SSH 分别核验了实际 Docker 凭据安装路径。控制面生命周期测试覆盖了真实的容器操作与数据库重启。Host 离线用运行时检查故障注入。浏览器回环和 SSH 转发经独立的本地监听地址验证。[Host 接入测试](../apps/api/internal/agentlink/gateway_credential_test.go) · [生命周期测试](../apps/api/cmd/server/gateway_lifecycle_test.go) · [回调转发测试](../apps/api/cmd/server/gateway_callback_tunnel_test.go)
- Colima 内的 Codex 内嵌 `bwrap` 沙箱因无法创建用户命名空间而拒绝执行工具。Docker 矩阵使用上述测试选项验证客户端、启动器、TLS 和工具协议。部署 Host 的默认内嵌沙箱能力需要在其实际容器内单独验证。

### 真实账号执行记录

2026-11-27，合成版 `8427356` 在独立 Colima Docker 环境中使用 `codex-cli 0.172.0` 和 `gpt-6.2-sol` 完成真实账号验收。Sign in with Codex 和 Sign in with ChatGPT 分别通过 WebSocket 和强制 HTTP/SSE 完成两次有依赖的工具往返，四格均收到 `response.completed`。撤销测试中，两渠道上游均确认撤权，原实例凭据随后被网关拒绝。该轮未覆盖正常到期刷新、Codex 专属 workspace 路由和实际分机 Host。

2026-11-27，提交 `af4d97e` 在隔离的本地 Colima Docker 中以 `codex-cli 0.172.0` 和 `gpt-6.2-sol` 完成真实上游换绑复验。两轮在同一个 Workspace 容器内各执行一次 A → B → A。每轮切回 A 后，使用新凭据恢复本轮原会话，模型准确复述了仅出现在先前工具结果中的随机 UUID，新增工具调用为 0。切回后以本轮首次绑定 A 时签发的旧网关凭据请求 `/g/v1/models`，两轮均返回 `401 invalid_gateway_credential`。

| 传输 | 绑定版本 | 终态与回退观测 |
|---|---|---|
| WebSocket | `1 → 2 → 3` | 3 次握手均返回 `101`，4 个 `response.completed` 经 WebSocket 送达，观测到 0 次 SSE `POST /g/v1/responses`，全程无 SSE 回退。 |
| SSE | `3 → 4 → 5` | 4 个 `response.completed` 经 SSE 送达。 |

2026-11-28，提交 `44ae2fa` 的本地 Compose 部署以 Sign in with Codex 授权、`codex-cli 0.172.0` 和 `gpt-6.2-sol` 复验同轮自动压缩。在运行中的 Workspace 内经正式启动器执行 `codex -c model_auto_compact_token_limit=1000 exec --json -s danger-full-access`，SSE 轮另加 `-c model_providers.cloudex.supports_websockets=false`。提示要求工具读取一次 `/proc/sys/kernel/random/uuid`，再只回复该值。每种传输各跑两轮，其中一轮带 `--ephemeral`，另一轮保留会话记录。四轮均收到 `turn.completed`，最终回复与工具输出的 UUID 一致。网关没有返回 `409`，也没有记录警告或错误。每轮的提交记录推进到序号 6，即生成、压缩和续写三次提交各完成预留与完成两步，终态为 `completed`。上游未回显 `req_` 形式的请求 ID。保留会话记录的两轮明细如下。

| 传输 | 网关提交及 input tokens | 会话记录 |
|---|---|---|
| WebSocket | 3 次 `websocket` 提交均为 `response.completed`，依次 12153、12336、12293，无 SSE 回退。 | 工具调用和输出之后写入 `compacted`，随后新窗口续写，最终 `task_complete`。 |
| SSE | 3 次 `sse` 提交均为 `response.completed`，依次 12153、12341、12308。 | 同上，各请求的 input tokens 与网关一致。 |
