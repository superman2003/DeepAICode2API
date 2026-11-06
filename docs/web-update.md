# 网页 Release 升级

从 `v0.2.0` 起，Windows x64 可运行包带网页升级能力。
首次从旧版迁入时，停止旧程序、备份服务器自己的 `.env` 和 `data`，
覆盖完整包中的程序与启动脚本，再运行 `run-windows.cmd`。完整包不包含真实 `.env` 或账号库。

## 使用

在管理网页填写管理密码，打开“更新”：

1. 检查更新：读取 `superman2003/DeepAICode2API` 最新稳定 Release。
2. 下载并校验：下载专用 Windows 升级 ZIP，校验 GitHub 资源摘要或同 Release 的 `SHA256SUMS.txt`，再核对包内程序摘要。
3. 安装并重启：有注册任务运行时会拒绝升级；旧程序先停止接收请求并保存状态，再退出。
   启动器在 exe 解锁后备份、替换两个程序，启动新版本并检查版本号、账号库可读性和账号数量。

更新会短暂中断接口。新版本启动或数据兼容性检查失败时，恢复旧程序。
升级只替换 `bin/deepaicode2api.exe`、`bin/register.exe`；页面内嵌在主程序中，会随程序一起更新。
不会覆盖 `.env`、`data`、账号、网关密钥、临时邮箱令牌、注册台账或日志。
程序备份位于 `data/updater/backups`，状态和错误在 `data/updater/state.json`。

更新来源在页面保存到 `data/updater/config.json`，不需要改 `.env`。
默认使用公开仓库，不需要 GitHub token。私有仓库的下载权限需要另行配置；本版本的浏览器下载资源面向公开 Release。
从 `v0.2.1` 起，GitHub API 限流或无法连接时会自动尝试公开 Release Feed；备用通道仍须通过 SHA256 和包内清单校验，不需要修改 `.env`。
直接运行裸 exe、`-Once` 模式或旧启动器不能执行网页重启，页面会明确提示使用新版启动器。

下载只接受 GitHub HTTPS Release 资源；不接受网页提交任意下载 URL。
ZIP 解压仅允许专用清单和两个程序路径，拒绝目录穿越、符号链接、配置文件、数据文件和摘要不匹配。
单包最多 64 MiB，展开最多 128 MiB。

## 后续发布

修改并提交代码，创建新版本 tag，例如 `v0.2.2` 后推送 tag。
仓库的 `Windows release` 工作流会测试、编译并发布：

- `DeepAICode2API_windows_amd64.zip`：首次安装或启动器协议变更时使用的完整无账号包。
- `DeepAICode2API_windows_amd64_update.zip`：网页升级专用，仅含两个程序与 `update-manifest.json`。
- `SHA256SUMS.txt`：两个 ZIP 的 SHA256。

也可手动执行工作流，填写版本号；或在 Windows 本地执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\build-release.ps1 -Version v0.2.2
```

保持 `update-manifest.json` 的 protocol=1，且程序继续支持 `-check-state`、`/health` 的版本号和更新退出码 75。
需要不兼容数据格式迁移或升级启动器协议时，应发布新的完整包并明确升级步骤，不能当作普通二进制热替换。
