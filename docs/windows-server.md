# Windows 美国服务器迁移

迁移包适用于 Windows x64，包含本次修改后的程序、源码和账号快照。
不需要在服务器安装 Go 或 Docker。将整个包解压到固定目录，例如 `D:\Apps\DeepAICode2API`。

## 携带的数据

- `data/deepaicode2api-data.json`：加密的账号池、原网关密钥、分组、代理及注册信息。
- `.env`：与账号库对应的原加密主密钥、管理密码、TempMail.lol 密钥和服务器运行设置。
- `data/exports`：注册台账、私有凭据旁路文件、临时邮箱收信令牌。
- `data/result`：已有注册交付物。
- `data/access.txt`：管理密码及原网关 API Key。
- `migration-manifest.json`：快照时间、账号数量、程序版本及文件校验信息。

主密钥和账号库必须成套迁移，不能把 `.env` 换成示例文件或重新生成主密钥。
压缩包包含私有凭据，请通过自己的远程桌面或受控文件传输上传，解压目录放在网站目录之外。
迁移包是生成时的快照；之后本机新增的账号不会自动同步到服务器。

## 安装及验证

在服务器上以管理员身份打开 **Windows PowerShell**：

```powershell
cd D:\Apps\DeepAICode2API
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Validate
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Install -OpenFirewall
```

`Validate` 不访问外网，只核对原主密钥能否解密账号库，并显示账号/网关密钥数量。
`Install` 会设置数据目录权限、创建 SYSTEM 开机任务并启动服务，不需要保持远程桌面登录。
权限初始化在读取账号库之前执行，目录和普通文件分别处理，避免文件读取被拒绝。
若已有同名服务器任务，安装会在新账号库校验通过后停止旧实例，再更新任务路径，支持解压到新目录后安装。
云厂商的安全组也需要允许 TCP 10087。若只允许你的电脑访问，可在安装时加
`-AllowedRemoteAddress 你的公网IP`。

查看状态：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Status
Invoke-RestMethod http://127.0.0.1:10087/health
```

若旧版安装脚本已造成 `Access is denied`，用修复包覆盖 `scripts` 后，在管理员 PowerShell 中重新运行
`-Action Install -OpenFirewall`；也可先运行 `-Action RepairPermissions` 单独修复 NTFS 权限。
这项操作不更改账号库内容或加密主密钥。

控制台为 `http://服务器IP:10087/`，继续使用原管理密码。
Claude/CC Switch 地址改成 `http://服务器IP:10087`，保留原网关 API Key；
OpenAI 兼容客户端使用 `http://服务器IP:10087/v1`。公网长期使用时可在入口配置 HTTPS。

## 网络与故障恢复

服务器包已将 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 和 `DEEPAI2API_EGRESS_PROXY` 清空，
不依赖本机 7890。账号原来的远程代理绑定仍保留。

`DEEPAI2API_PROXY_FALLBACK_DIRECT=1` 开启以下行为：

- 默认使用账号绑定的远程代理；连接失败且上游请求尚未发出时，尝试服务器直接出网。
- 失败代理进入约 60 秒的冷却期，随后再尝试恢复原代理。
- 请求已经发出或流式响应已经开始时，不由出口回退层重放请求。
- 账号侧继续使用现有失败换号、容量退避和请求重试逻辑。

开机任务运行守护进程，程序退出后约 10 秒重新启动；
开启的健康检查发现持续约一分钟无法探活时会重新启动程序。
守护进程自身异常退出时，计划任务每分钟尝试重新启动。

## 备份与恢复

安装时创建一次备份，之后每天服务器本地时间 03:00 再备份，默认保留最近 14 份。
备份包含原主密钥和加密账号库，创建压缩包前会用程序实际验证快照可以解密。

手动备份：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Backup
```

备份位于 `data\server-backups\server-backup-*.zip`。需要防范整机或磁盘故障时，
请定期把备份复制到另一台机器或私有备份存储；单机备份不提供第二台机器的自动接管。

恢复时先将备份解压到一个新的私有目录，再执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Stop
# 将备份中的 .env 和 data 复制回项目目录，然后重新验证、启动。
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Validate
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows-server.ps1 -Action Start
```

迁移完成后以服务器为主要写入端，避免继续在两台机器独立注册后误以为账号会自动合并。
旧 Windows 绝对数据路径和收信令牌路径已在迁移包中改成项目相对路径。
