# TempMail.lol 外部临时邮箱

注册页选择 **TempMail.lol（外部临时邮箱）**，数量先填 1，开启自动收信验证，再开始注册。
此后端自动创建邮箱，无需 QQ 邮箱、IMAP 授权码或自建邮箱服务。

API Key 可在服务器 `.env` 配置；页面留空时复用服务器的密钥。
页面中 TempMail.lol 专用域名留空时随机分配；仅当自有域名已绑定到 TempMail.lol 账号时才填写。
此前 QQ IMAP 表单的域名和指定邮箱不会传给这个后端。

```dotenv
TEMPMAIL_LOL_BASE=https://api.tempmail.lol/v2
TEMPMAIL_LOL_API_KEY=你的API密钥
TEMPMAIL_LOL_TOKEN_DIR=私有目录/tempmail-lol-inboxes
```

网关默认把邮箱收信令牌保存在状态文件旁的 `exports/tempmail-lol-inboxes` 中，
CLI 默认使用当前目录下同名路径。每个邮箱一个私有 JSON 文件；令牌不出现在页面、注册台账或日志中。
服务重启或补跑时会读取这些令牌；邮箱仍须处于 TempMail.lol 的有效期内。
不要将令牌目录加入 Git 或公开分享。此部署使用已有的私有 `data` 目录。

CLI：

```powershell
.\bin\register.exe -mail-backend tempmail_lol -count 1
```

临时邮箱 API 沿用 HTTP/HTTPS 环境代理访问；注册和推理继续保留代理池及 7890 连接出口。
同一邮箱至少间隔 5 秒检查一次，跨任务共享节奏。收到 429 时遵守 `Retry-After`，
服务未提供该响应头时暂停 15 秒；冷却期间同一 API Key 的其他收件箱也暂停请求。
支持过期识别、取消、超时；验证超时会在任务中显示警告，不会一直停留在 pending。

接口依据：[官方 API 文档](https://tempmail.lol/api)、[官方 Python SDK](https://github.com/tempmail-lol/api-python)。
使用 `POST /v2/inbox/create` 创建邮箱和 `GET /v2/inbox?token=...` 收信，付费密钥通过 Bearer 请求头提供。
