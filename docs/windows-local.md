# 本机 Windows 部署

项目目录：`C:\Users\Administrator\Desktop\deepaicode`

- 控制台：<http://127.0.0.1:10087/>
- OpenAI Base URL：`http://127.0.0.1:10087/v1`
- Anthropic Base URL：`http://127.0.0.1:10087`
- 管理密码、网关 API Key：项目目录下的 `data/access.txt`。
- 当前仅监听本机回环地址。

打开控制台，输入管理密码，在账号页登录 DeepAI 或手动添加已有 API Key。
客户端使用 `data/access.txt` 中的网关 API Key；接入上游账号后才可以推理。

## 运行管理

在项目目录打开 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Status
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Restart
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Stop
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Start
```

登录当前 Windows 用户后，启动目录中的 `DeepAICode2API.lnk` 会启动隐藏的守护进程。
网关异常退出后，守护进程等待 10 秒再启动网关。`Stop` 同时停止守护进程和网关；
下次登录仍会自启。要禁用自启，运行 `shell:startup` 并删除这个快捷方式。

## 配置与日志

- `.env`：管理密码、AES-256-GCM 加密主密钥、监听地址、数据文件路径和代理配置。
- `data/deepaicode2api-data.json`：加密的账号池、网关密钥及状态。
- `data/gateway.stderr.log`：网关启动与错误日志，每次启动覆盖。
- `data/gateway.stdout.log`：网关标准输出，每次启动覆盖。
- `data/supervisor.log`：守护进程启动和异常重启记录。

上游访问使用本机已有代理 `http://127.0.0.1:7890`。请保持代理运行；
修改 `.env` 中的 `HTTP_PROXY`、`HTTPS_PROXY` 后运行 `Restart` 生效。
配置 `DEEPAI2API_EGRESS_PROXY=http://127.0.0.1:7890` 后，注册和推理指定的远程代理
也会先经过本机 7890 连接，代理池和远程出口 IP 保留。直接指定本机 7890 时不会重复套代理。

`.env` 和 `data` 已限制为当前用户及 SYSTEM 访问，并被 Git 忽略。
备份时同时保存 `.env` 和 `data`；丢失加密主密钥将无法读取账号池。
移动整个项目后，更新 `.env` 中的数据路径，再运行 `-Action Install` 更新自启快捷方式。

## 更新与重新编译

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Stop
git pull --ff-only
go test ./...
go build -trimpath -ldflags "-s -w" -o bin\deepaicode2api.exe .
go build -trimpath -ldflags "-s -w" -o bin\register.exe ./cmd/register
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Start
```

首次部署版本：`f06acbe`。
