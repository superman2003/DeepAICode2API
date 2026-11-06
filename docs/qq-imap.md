# 域名邮箱 + QQ IMAP

此后端使用域名随机地址注册，通过 QQ 邮箱收取转发后的验证邮件。
不需要 CF Temp Email Worker；域名转发可以使用已有的邮件路由服务。

## 准备收信

1. 将注册域名的 Catch-all（所有未单独设置的邮箱地址）转发到自己的 QQ 邮箱。
   例如：`任意前缀@example.com` → `123456@qq.com`。
2. 在 QQ 邮箱设置中启用 IMAP，并生成 IMAP 授权码。
3. 确认转发保留原始 `To` 或 `X-Original-To` 等收件头。
   程序按原始域名地址匹配邮件；若转发把原始地址全部移除，就无法可靠匹配账号。

## 注册页面

邮箱后端选择 **域名邮箱 + QQ IMAP**，填写注册域名、QQ 收信邮箱与授权码。
默认使用 `imap.qq.com:993`、TLS 和 `INBOX` 文件夹。
先点 **测试邮箱连接**，通过后再开始注册。

连接测试只检查 TLS、IMAP 登录和收件文件夹，不会注册账号或发送邮件，
也不代表域名转发已生效。可先自行向一个域名地址发送邮件，确认 QQ 邮箱能收到。
开启自动验证时，开始注册前也会检查 IMAP 登录，检查失败就不会创建注册任务。

页面填写的授权码仅用于当前请求/任务，不会进入注册台账、账号状态或浏览器存储。
如果希望后续任务直接复用，请使用下面的服务端配置；修改后重启网关。

## 服务端配置

在 `.env` 中填写：

```dotenv
DEEPAI_REGISTER_MAIL_BACKEND=qqimap
DEEPAI_REGISTER_DOMAIN=example.com
QQ_IMAP_USER=123456@qq.com
QQ_IMAP_AUTH_CODE=QQ邮箱生成的授权码
QQ_IMAP_HOST=imap.qq.com
QQ_IMAP_PORT=993
QQ_IMAP_FOLDER=INBOX
```

使用 IMAP 授权码，不是 QQ 账号登录密码。配置后，页面中的 QQ 参数可以留空。
注册地址默认生成 `dai<随机字符串>@example.com`；也可以指定已有的同域地址（数量为 1）。

Windows 本机部署重启：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\windows.ps1 -Action Restart
```

CLI 同样支持：

```powershell
.\bin\register.exe -mail-backend qqimap -doctor
.\bin\register.exe -mail-backend qqimap -count 1
```

## 收信行为

- IMAP 使用 TLS，并检查服务器证书。
- 只读打开文件夹，通过 `BODY.PEEK` 读取，不删除邮件或标记已读。
- 搜索 DeepAI 发件人或主题，只检查最近 100 封候选邮件，每封最多读取 1 MiB。
- 解码 MIME、Base64、Quoted-Printable 和 HTML 后提取 DeepAI 验证链接。
- 同一注册任务共享扫描结果，最短每 3 秒扫描一次，按域名收件地址隔离账号。
- 过滤本次注册之前的旧邮件，支持取消与收信超时。
- 网络直连 IMAP 服务，不使用注册代理池；现有 HTTP/HTTPS 代理设置继续用于上游 HTTP 请求。

实际收信仍需正确的域名转发规则、QQ 邮箱和授权码。

## 开发验证

已使用本地 TLS IMAP 服务器测试收信、邮件解码、账号隔离、证书校验、取消和超时。
全量 `go test ./...`、`go vet ./...` 和页面 JavaScript 语法检查已通过。
本机没有 C 编译器，未能运行 `go test -race`；真实 QQ 收信需配置后测试。
