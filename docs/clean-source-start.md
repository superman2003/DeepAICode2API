# TempMail.lol 干净源码版

本压缩包只包含当前程序源码、测试、示例配置和简单启动脚本。
不包含本地账号库、账号密码、网关 API Key、加密主密钥、邮箱收信令牌或本机 `.env`。

## Windows 美国服务器运行

1. 解压到新的普通目录，例如 `C:\DeepAI-Source`。
2. 安装 Go 1.25 或更新版本，重新打开 PowerShell。可先运行 `go version` 确认。
3. 在项目目录运行 `run-windows.cmd`，或执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\run-clean-windows.ps1
```

首次运行会从 `.env.example` 新建 `.env`，生成全新的加密主密钥和管理密码，
编译程序，然后在当前窗口启动。进程退出时等待 5 秒再启动；关闭窗口或 Ctrl+C 可停止。
不依赖计划任务、SYSTEM 账户或个人“启动”目录。

若是在之前安装失败的那台服务器上运行，先以管理员身份停止旧任务，避免它继续尝试占用端口：

```powershell
Get-ScheduledTask -TaskName DeepAICode2API-Server -ErrorAction SilentlyContinue | Stop-ScheduledTask
```

控制台地址：`http://服务器IP:10087/`。管理密码保存在新 `.env` 的
`DEEPAI2API_ADMIN_PASSWORD`。首次启动会显示新生成的网关 API Key，也可在控制台创建。
服务器 Windows 防火墙及云平台安全组需按你的访问范围允许 TCP 10087。

## TempMail.lol 注册

在注册页选择 **TempMail.lol（外部临时邮箱）**，填入自己的 TempMail.lol API Key，
邮箱域名留空随机分配。先注册 1 个账号确认流程，再增加数量。
也可以把密钥写入新 `.env` 的 `TEMPMAIL_LOL_API_KEY`，重启后页面留空复用。

默认不使用 7890。账号代理连接失败且请求尚未发出时，允许服务器直连回退。
接口支持 OpenAI `/v1/chat/completions` 和 Anthropic `/v1/messages`。

## 可选手动编译

```powershell
go test ./...
go build -trimpath -o bin\deepaicode2api.exe .
go build -trimpath -o bin\register.exe ./cmd/register
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\run-clean-windows.ps1 -NoBuild
```

后续备份只需妥善保存服务器自己生成的 `.env` 和 `data`。已有加密账号库时，
启动脚本不会重新生成缺失的主密钥；需要恢复那份账号库对应的原配置。
