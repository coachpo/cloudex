# SSH 接入与 Web Terminal

## SSH 接入信息

使用 SSH 直连 Host 时，客户端仍按 Workspace 的配置连接共享 Gateway。Gateway 经远端宿主机的 SSH 连接转发到目标容器的专用 bridge 地址。添加 Host 时填写的宿主机登录凭据只供控制面使用。Workspace 用户的公钥依然在**设置 → SSH 公钥**管理。[Host 连接条件](operations.md#ssh-直连)

SSH 配置的 Host 名以 `cloudex-` 开头，名称仅含 ASCII 字母、数字、点、下划线或连字符且首尾为字母或数字时使用小写名称，否则使用工作区 ID 的前 8 位，Gateway 登录名和工作区的 `HostKeyAlias` 仍为 `cloudex-<完整 ID>`。

只有 `ready` 表示 Gateway 已核验当前容器的 sshd，并能提供两层主机公钥、指纹和 SSH 配置。客户端仍须自行验证网络与登录。SSH 客户端分别验证 Gateway 和容器的主机密钥。修改控制台的 SSH 主机名后，重新复制配置即可沿用原主机密钥别名。管理员在设置页统一替换公钥列表。Gateway 对新连接和新转发立即使用当前列表，容器内 sshd 读取下发的文件。离线 Host 重连后接收当前列表。撤销公钥不结束已建立的 SSH 会话。[状态与返回内容](../apps/api/internal/control/ssh.go) · [客户端连接步骤](operations.md#通过-ssh-连接-workspace)

## Web Terminal

终端要求有效的控制台会话、运行中的容器及可接纳交互连接的 Workspace。有排队或运行中的生命周期操作时拒绝新连接。[准入条件](../apps/api/internal/store/workspaces.go)

双向二进制消息传输 PTY 字节，调整终端尺寸和关闭 Shell 使用 JSON 文本消息。客户端须将较长输入分段发送，单条超限会关闭连接并结束 Shell。连接断开也会结束 Shell，控制面被强制停止时由终端租约收尾。重连会打开新 Shell。消息格式、单条上限和关闭行为见[服务端协议](../apps/api/internal/httpapi/terminal.go)、[超限测试](../apps/api/internal/httpapi/terminal_test.go)与[客户端分段示例](../apps/web/components/terminal.tsx)。
