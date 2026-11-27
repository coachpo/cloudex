# 控制台端到端验收

使用完整本地控制面、浏览器和可丢弃的 Docker Host。先按[验证入口](../CONTRIBUTING.md#项目验证入口)构建，再按[安装步骤](operations.md#安装与入口)启动。推理接入见 [Codex 网关验收](codex-gateway-acceptance.md)。

## 创建回执丢失后重试

从新建表单提交带首尾空白的唯一名称。当代理收到真实 API 的 `202` 时，记下 `Location`、`workspace_id` 和 `operation_id`，仅切断向浏览器返回的响应。在第二个标签页确认 Workspace 已创建。在原页面等报错后以去除空白的同一名称重试，第二次请求直达 API。可用 Playwright 在首次拦截时执行 `route.fetch()` 取得后端回执，再执行 `route.abort()` 丢弃下行。

两次 POST 须使用相同的非空 `Idempotency-Key`、规范化名称和相同创建参数，第二次返回相同的 Workspace 和操作 ID。操作结束后，Workspace 列表中该名称只出现一次，操作历史只有一次 Create。检查新容器可以实际访问。[创建接口](../apps/api/internal/httpapi/workspaces.go) · [幂等事务](../apps/api/internal/store/workspaces.go)

## 键盘、反馈与工作区访问

以下使用英文 UI 标签定位控件。除明确注入的异常或展示数据外，使用真实 API。用浏览器无障碍树、活动元素、网络事件和页面尺寸记录判定证据。验证键盘可达时，从前一可聚焦元素按 Tab 进入。

| 场景与前置数据 | 操作 | 预期结果与判定证据 |
|---|---|---|
| U1 浏览列表与小屏布局。准备 64 字符连续 Host/Workspace 名和 128 字符分支，查看非空列表、Host 详情、新建页、设置、Logs 和 Git | 在 320×720、1100×720 下逐页检查 `document.documentElement.scrollWidth`。从全新加载的 Workspaces 按 Tab、Enter；再用 Tab 进入 Filter by state，操作筛选。320×400 打开长名 Host 的 Disable host 对话框并按 Escape；在 Settings 从 Save account 按 Tab 到 Default workspace image，先将后者滚到靠近底栏。 | 页面宽度不超过视口；首个 Tab 显示 Skip to main content，Enter 后焦点为唯一 main。筛选组有可访问名称、改变选中状态，焦点环未被滚动边缘裁掉。对话框长标题在框内换行，初始焦点 Keep host，Esc 后回 Disable host，Host 状态不变；底栏上方可见完整的聚焦输入框。 |
| U2 接入 Host。准备仅供测试的 SSH Host 地址与已核对的指纹，另备未完成注册的非本机 Agent Host | 从 Hosts 用 Enter 打开 Add host，Tab 进入连接方式，左右键切 Agent/Direct SSH。填测试地址，Get host fingerprint，点 Check fingerprint again，再 Cancel。另打开 Agent 表单签发注册令牌、完成面板；从可重发令牌的 Host 详情及 SSH Host 的更新凭据入口分别打开并关闭。 | 打开时焦点进面板标题；方向键改变单选且焦点留在选项；重新检查回到获取指纹按钮，取消返回 Add host。签发后焦点到 Connect host agent 标题，证据避开令牌值。详情页关闭后回触发按钮；若登记状态已使触发项消失，则回 Host 标题。核对每步活动元素与当前展开状态。 |
| U3 从 Access 页复制 SSH 信息。运行中、已有公钥，Gateway ready | 分别复制 known_hosts 和 SSH config，比对剪贴板与页面文本。禁止写剪贴板（或仅令浏览器的 writeText 拒绝），再试两按钮并手动选择文本。保持页面挂载，仅在下一次 access GET 的响应中改变相应文本；再让下一次刷新返回带唯一测试消息的 409，随后放行真实 GET 并重试。 | 成功能读回完整内容；写入失败时出现“请选择下方文本手动复制”对应提示，文本仍可复制。文本改变后旧结果消失；已有数据时刷新失败仅有一个对应 alert，恢复后显示当前状态。记录四次复制结果和刷新前后的状态区域。 |
| U4 在真实终端重连。Workspace 运行，WebSocket 可用 | 进入 Terminal 并输出一条唯一标记。从 Terminal input 按 Shift+Tab，再用 Enter 激活 Reconnect；观察新连接，返回输入区执行 `pwd` 和第二条标记。 | Shift+Tab 恰好到 Reconnect。真实 WS 重新建立、状态回 Connected，第二次命令确实回显结果（例如用 `printf '%s%s\n' e2e- reconnected`），确认新 PTY 可交互。 |
| U5 查看并离开长输出。准备可滚动的日志、工作树状态和已跟踪文件差异 | 在测试工作树提交一个多行样本文件后修改全部行，再创建足量未跟踪样本使 status 可滚动；日志用多次 SSH 连接生成，长度不足时可单独以 120 行合成 logs GET 验证呈现，并注明替身。用 Tab 分别进入 Container logs、Working tree、Git patch，再 PageDown。随后在浏览器网络层暂挂一个已发出的 Git GET，点击 Logs，观察请求取消再解除挂起。 | 三个有名称的 region 都可键盘进入；各自 scrollTop 增加、内容可读。离页触发 Git 请求 aborted/requestfailed，Logs 显示自己的数据，迟到结果不覆盖当前视图。保存焦点、滚动前后值和请求时间线。 |
| U6 访问错误地址。已有有效 Workspace ID | 在地址栏分别直接进入 /unknown 和 /workspaces/{有效ID}/unknown，查看文档请求状态，在两页各用键盘激活返回 Workspaces 的链接。 | 两页均有一个 main、可读的 Page not found，返回链接实际打开 /workspaces 并显示列表。整页未知路径 HTTP 404；Workspace 的未知子页因流式路由已发头，文档 HTTP 200。 |
| U7 保存 SSH 公钥。准备两把有效测试公钥 | 保存第一把公钥，继续编辑成另一把，暂不保存。 | 首次保存显示成功，继续编辑后旧成功提示消失。独立 GET 仍为最后一次保存的公钥。 |
| U8 选择主题并离开菜单。登录页、新浏览器上下文 | 分别模拟系统浅/深并选择 System，再在系统深/浅下显式选择 Light/Dark，每次重载。打开主题菜单分别以 Tab、Shift+Tab、Esc 退出。 | 四组 body 背景依次为 `#f4f4f2`、`#111110`、`#f4f4f2`、`#111110`。Tab 关闭并进 Username，Shift+Tab 离开菜单，Esc 关闭后焦点回主题按钮；记录计算样式和焦点。 |
| U9 控制面异常回包。新开已登录的设置页 | 仅对真实页面读取 /settings 的那次 GET 用代理返回 HTTP 200、JSON 字符串 `"unexpected response"`；观察错误后放行正确响应并重试。 | 页面显示控制面未返回有效响应的错误，可恢复显示设置值。保留响应类型、状态和页面错误；若出现未捕获 TypeError、空白页或把异常值当保存结果均失败。 |

## 删除与授权保存的故障反馈

以下 Playwright 用例用模拟 API 注入故障。核对请求顺序和浏览器可见反馈。

| 场景 | 操作 | 预期结果 |
|---|---|---|
| [Host 删除回执丢失](../apps/web/tests/host-removal.browser.js) | 在 Host 详情确认删除，丢弃 DELETE 回执。分别让后续详情读取返回 `404`、返回原登记，或持续失败。 | `404` 时显示已移除，Hosts 列表和 Workspace 放置选项均移除该 Host。查到原登记时允许重试。读取失败时显示结果待确认，重新检查仅发读取请求。 |
| [Codex 授权保存失败](../apps/web/tests/grants.browser.js) | 到达授权确认页，令保存返回带具体错误的 `409`。暂挂随后的状态查询，再以 `404` 结束查询。 | 暂挂期间立即显示原错误，允许取消，保存按钮停用。收到 `404` 后撤下保存按钮，原错误仍可见。 |
