# DeepAICode2API

本仓库在 [原项目](https://github.com/2951461586/DeepAICode2API) 基础上增加 TempMail.lol、QQ IMAP、Claude Code/Codex 缓存兼容、会话账号绑定及网页 Release 更新。
Windows 可运行包不带任何账号或真实密钥，无需安装 Go。首次运行 `run-windows.cmd`，后续在管理网页“更新”中下载、校验并重启升级。
详见 [网页更新说明](docs/web-update.md) 和 [最新 Release](https://github.com/superman2003/DeepAICode2API/releases/latest)。

[![ci](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml/badge.svg)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

把本机 **DeepAI Code 桌面端**的上游模型服务，逆向复刻成一个
**OpenAI / Anthropic 双协议兼容的 Go 网关**。

复用桌面端登录后写入的 API Key，无需另申请密钥；Go 实现，
编译后为单文件程序。QQ IMAP 收信使用 go-imap / go-message，运行时无需额外服务进程。

| 能力 | 说明 |
| --- | --- |
| 双协议 | OpenAI `/v1/chat/completions`、`/v1/responses`；Anthropic `/v1/messages` |
| 号池治理 | 分组轮询、失败换号、容量退避、按账号配额调度 |
| 账号注册 | 邮箱注册 → 登录 → 取 Key → 验活 → 入池，CF 临时邮箱 + 代理池 |
| 控制台 | 单页内嵌 UI：账号 / 密钥 / 日志 / 统计 / 注册 / 设置 |
| 安全 | 密钥鉴权、管理口令、同源写保护、状态文件 AES-256-GCM 可选加密 |
| 可回归 | 3.8k 行测试，`go test -race` + CI 覆盖 5 个包 |

```text
┌──────────────┐  /v1/chat/completions   ┌────────────────┐   Api-Key + Theos 头   ┌─────────────────────────────┐
│ OpenAI SDK   │  /v1/responses          │ DeepAICode2API │  X-DeepAI-Code-Machine │ https://api.deepai.org      │
│ Claude Code  │  /v1/messages           │   （本网关）    │ ─────────────────────▶ │ /deepai_code/chat/completions│
│ pi / Cline   │  /v1/models             │                │  ◀───────────────────── │ /deepai_code/responses      │
└──────────────┘  SSE 流式 / 非流式       └───────┬────────┘      OpenAI 兼容 SSE    └─────────────────────────────┘
                                                  │ 自动读取
                                                  ▼
                          ~/.deepai-code/userdata/secrets/deepai-account.bin
                          {"apiKey":"…","username":"…","isPro":false}
```

---

## 一、逆向结论

DeepAI Code 是 **Electron + 本地 t3code server** 架构（应用版本 0.3.17）。
真正干活的不是桌面壳，而是 `resources/server.asar` 里的 t3code server；
它把上游能力封装成本地回环 HTTP 服务，再交给 Pi（编码 agent）调用。

### 1.1 关键事实

| 环节 | 逆向结果 |
| --- | --- |
| 应用形态 | Electron 壳（`app.asar`）+ 独立 server（`server.asar`，约 200 MB） |
| 数据目录 | `T3CODE_HOME` 或 `~/.deepai-code`，状态目录为 `<base>/userdata` |
| **凭据落盘** | `<stateDir>/secrets/deepai-account.bin`，**明文 JSON**（文件权限 0600） |
| 凭据内容 | `{apiKey, username, isPro}`，`apiKey` 为 UUID 格式 |
| **推理入口** | `POST https://api.deepai.org/deepai_code/chat/completions` |
| Responses 入口 | `POST https://api.deepai.org/deepai_code/responses`（仅 `api=openai-responses` 模型） |
| 模型目录 | `GET https://api.deepai.org/deepai_code/models` |
| 配额 | `GET https://api.deepai.org/deepai_code/theos` |
| 用量 | `GET https://api.deepai.org/get_usage_summary` |
| 语音转写 | `POST https://api.deepai.org/speech_to_text`（字段 `voiceRecording`） |
| 联网搜索 | `POST https://api.deepai.org/deepai_code/web_search` |
| **鉴权** | 请求头 `Api-Key: <apiKey>`（不是 `Authorization`） |
| 协议头 | `X-DeepAI-Code-Theos: 2`（计费车道版本） |
| **机器指纹** | `X-DeepAI-Code-Machine: sha256(原始机器 UUID)`，用于按天发放免费 Theos |
| 会话粘性 | `X-Session-Affinity`（body 里的 `prompt_cache_key` 也会被上游使用） |
| 容量拒绝 | `409 {"error":{"code":"session_busy"}}` + `Retry-After` |
| 浏览器登录 | `GET /deepai_code/desktop_auth/?state=…&port=…` → 本机 `/auth-callback?code=…` → `POST /deepai_code/desktop_auth/exchange/` |

### 1.2 上游本身就是 OpenAI 兼容的

`chat/completions` 的请求 / 响应与 OpenAI 完全一致，响应还额外带了
OpenRouter 风格的 `provider`、`reasoning`、`reasoning_details`、`cost` 等字段。
因此本网关不需要做复杂的协议还原，重点在：

1. **凭据导入**：从桌面端明文凭据文件读取并去重；
2. **账号池**：多 Key 轮询、失败换号、`session_busy` 等待重试；
3. **模型治理**：动态目录缓存、别名、兜底模型；
4. **协议适配**：为 Anthropic 客户端和 Responses 客户端做翻译；
5. **可观测性**：请求日志、token / 成本统计、配额探测、控制台。

### 1.3 机器指纹怎么来的

桌面端逻辑（`src/deepai/machineId.ts`）：

- Windows：`(Get-CimInstance Win32_ComputerSystemProduct).UUID`，固件占位 UUID 时回落
  到注册表 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`；
- macOS：`ioreg` 的 `IOPlatformUUID`；
- Linux：`/sys/class/dmi/id/product_uuid`，其次 `/etc/machine-id`；
- 最终取 `sha256(小写原始值)`，原始值绝不上传。

本网关复刻了同一套逻辑。容器 / 无 SMBIOS 环境可用
`DEEPAI_MACHINE_ID=<原始UUID>` 显式覆盖。

---

## 二、支持的模型

模型目录**完全由上游下发**（当前 14 个，下表是 2026-11-06 的快照），
网关只做缓存与别名，上游新增模型无需改代码。默认免费模型：

| ID | 名称 | 上下文 | 最大输出 | 说明 |
| --- | --- | ---: | ---: | --- |
| `glm-5.3-flash` | GLM 5.3 Flash | 1,048,576 | 131,072 | 默认模型，免费 |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash | 1,048,576 | 131,072 | 免费 |
| `mimo-2.6-flash` | MiMo 2.6 Flash | 1,048,576 | 131,072 | 免费 |
| `solar-mini-4` | Solar Mini 4 | 524,288 | 131,072 | 免费，纯文本 |
| `gpt-6-luna` | GPT-6 Luna | 1,048,576 | 128,000 | 免费 |
| `mimo-2.6-pro` | MiMo 2.6 Pro | 1,048,576 | 131,072 | 免费 |
| `crouton-alpha` | Crouton Alpha | 1,048,576 | 131,072 | 免费 |
| `deepai-cyber` | DeepAI Cyber | 65,536 | 8,192 | 免费，纯文本 |

付费（premier）模型包括 `gpt-6.1-sol`、`glm-5.3`、`kimi-k3`、
`claude-opus-5-5`、`claude-fable-5-1`、`gpt-6-astra`（Responses 端点）。

内置别名（可在控制台覆盖）：

- `gpt-4o` / `gpt-4` / `gpt-3.5-turbo` / `claude-3-5-sonnet` / `deepseek-chat` … → `glm-5.3-flash`
- `deepai/<id>`、`openai/<id>` 前缀会被自动剥离

无法识别的模型名**不会报错**，而是回落到「兜底模型」→ 目录默认模型，
保证任意客户端开箱即用。

---

## 三、接口清单

### OpenAI 兼容

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/models` | 模型目录（含 `deepai` 元信息） |
| POST | `/v1/chat/completions` | 流式 / 非流式；支持 tools、图片、`prompt_cache_key` |
| POST | `/v1/responses` | `openai-responses` 模型透传；其余模型自动翻译 |
| POST | `/v1/audio/transcriptions` | 转发上游语音转写（webm / mp4） |
| POST | `/v1/audio/translations` | 同上（别名） |

### Anthropic 兼容

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/messages` | 流式 / 非流式，支持 `tool_use`、`thinking`、图片 |
| POST | `/v1/messages/count_tokens` | 本地估算（上游不提供） |

Claude Code 只需：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:10087
export ANTHROPIC_API_KEY=sk-...        # 网关密钥；未签发时可留空
export ANTHROPIC_MODEL=glm-5.3-flash
```

### DeepAI 原生能力扩展

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/deepai/theos` | 当前账号免费额度 / 余额 |
| GET | `/v1/deepai/usage` | 订阅与用量摘要 |
| POST | `/v1/deepai/web_search` | 上游联网搜索（`{query, num_results}`） |
| POST | `/v1/deepai/find_service` | 上游服务推荐 |

### 管理接口（注册 / 入池）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/api/register` | 启动批量注册任务（`count` / `concurrency` / `group` / `mail_backend` / `email` / `password` / `verify`） |
| GET | `/admin/api/register?id=…` | 查询任务进度与逐账号结果 |
| POST | `/admin/api/accounts` | `action=enroll`：验活并写入号池 |

### 控制台

| 路径 | 说明 |
| --- | --- |
| `/` | 内嵌单页控制台（无外部依赖） |
| `/ping` | `pong` |
| `/health` | 账号 / 模型 / 机器指纹摘要 |
| `/admin/api/*` | 账号、密钥、日志、统计、设置、配额、登录 |

---

## 四、快速开始

```bash
# 1) 编译（产物在 bin/；也可直接 go build -o deepaicode2api.exe .）
make build

# 2) 直接启动（会自动导入桌面端凭据 + 拉取模型目录）
./bin/deepaicode2api

# 3) 打开控制台
#    http://127.0.0.1:10087/

# 4) 注册链路（可选）
cp .env.example .env             # 填 TEMPMAIL_* 或 DEEPAI_REGISTER_DOMAIN
go build -o register.exe ./cmd/register
./register.exe -doctor          # 检查邮箱后端 / 网关 / 机器指纹
./register.exe -count 1         # 注册 1 个账号 → 验证 → 验活 → 入池
./register.exe -count 3 -concurrency 2 -group batch-a
./bin/register -check-proxies   # 检查代理池 → 上游连通性
./bin/register -reverify -group <分组>  # 给未验证账号补点验证链接（限流）
./bin/register -resume          # 补跑台账里未入池的账号
```

> 首次部署建议先设置 `DEEPAI2API_MASTER_KEY`（`openssl rand -hex 32`）：
> 状态文件里是上游 API Key 与账号密码，Windows 上 `0600` 不设 ACL。

启动日志示例：

```text
状态文件: …/deepaicode2api-data.json
机器指纹: a1b2c3d4e5f6…（用于上游按天发放免费 Theos）
已导入账号 desktop-…（~/.deepai-code/userdata/secrets/deepai-account.bin）
模型目录已更新：14 个模型，默认 glm-5.3-flash
DeepAICode2API dev 已启动: http://127.0.0.1:10087/
```

### 调用示例

```bash
# OpenAI 风格
curl http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}'

# 流式
curl -N http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Python OpenAI SDK：

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:10087/v1", api_key="sk-...")
print(client.chat.completions.create(
    model="glm-5.3-flash",
    messages=[{"role": "user", "content": "hello"}],
).choices[0].message.content)
```

---

## 五、命令行参数与环境变量

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-host` | `DEEPAI2API_HOST` | `127.0.0.1` | 监听地址 |
| `-port` | `DEEPAI2API_PORT` | `10087` | 监听端口 |
| `-data` | `DEEPAI2API_DATA_PATH` | 用户配置目录 | 状态文件 |
| `-deepai-dir` | `DEEPAI2API_DATA_DIR` | 自动探测 | 桌面端数据目录 |
| `-admin-password` | `DEEPAI2API_ADMIN_PASSWORD` | 空 | 管理密码 |
| `-allow-insecure` | `DEEPAI2API_ALLOW_INSECURE` | `false` | 允许非回环监听但不设管理密码 |
| `-import` | — | — | 仅导入凭据后退出 |
| `-no-import` | — | — | 启动时不自动导入 |
| `-version` | — | — | 打印版本 |

其他环境变量：

| 变量 | 说明 |
| --- | --- |
| `DEEPAI_API_KEY` | 直接添加一个上游账号（可选） |
| `DEEPAI_MACHINE_ID` | 覆盖机器指纹原始 UUID（容器部署必填） |
| `DEEPAI2API_MASTER_KEY` | 状态文件加密主密钥（64 位 hex / base64，32 字节），留空则明文 |
| `DEEPAI_API_ORIGIN` | 覆盖上游地址（自建测试） |
| `TEMPMAIL_BASE` / `TEMPMAIL_ADMIN_KEY` / `TEMPMAIL_DOMAIN` | CF Temp Email Worker（注册链路收信） |
| `DEEPAI_REGISTER_DOMAIN` | 静态邮箱域名（无收件箱） |
| `DEEPAI_REGISTER_MAIL_BACKEND` | 强制邮箱后端：`cf` / `static` / `none` |
| `DEEPAI2API_GATEWAY` | `register.exe` 默认入池地址 |
| `DEEPAI2API_ADMIN_PASSWORD` | 管理密码（CLI 入池 / 控制台） |
| `DEEPAI_REGISTER_GROUP` | 默认号池分组 |
| `PROXY_POOL` | 注册 / 推理代理池（逗号或换行分隔完整 URL） |
| `PROXY_POOL_FILE` | 代理池文件（每行一个） |
| `DEEPAI_REGISTER_PROXY` | 单代理（覆盖代理池） |
| `DEEPAI2API_ENV_FILE` | 指定 `.env` 路径 |
| `T3CODE_HOME` / `DEEPAI_CODE_HOME` | 桌面端数据目录 |

### 诊断工具

```bash
go run ./cmd/probe -usage                 # 凭据 + 模型目录 + 配额
go run ./cmd/probe -prompt "hello"        # 真实调用一次
go run ./cmd/probe -prompt "hi" -stream   # 流式调用
go run ./cmd/probe -key <api-key>         # 直接验证某个 Key
```

---

## 六、注册 → 登录 → 取 Session → 验活 → 入池

本项目把 `deepai.org.har` 里的网页注册链路复刻成了纯 HTTP 的自动化流水线，
产物直接写入 DeepAICode2API 的轮询号池。

### 6.1 HAR 逆向结论

| 阶段 | 请求 | 说明 |
| --- | --- | --- |
| 预检 | `POST /get_user_login_type`（multipart） | 字段 `email`；返回 `user_exists` / `login_with_password` |
| 注册来源 | `POST /save_custom_signup_data`（JSON） | 对齐网页端，失败可忽略 |
| 注册 | `POST /daily-time-sync/registration/`（multipart） | `email` / `username` / `password1` / `password2`；201 后下发 `sessionid` |
| 登录 | `POST /daily-time-sync/login/`（multipart） | `username`（邮箱）/ `password`；400 = 凭据错误 |
| 取 Session | `GET /daily-time-sync/user/` | 返回 `pk`、`username`、`userprofile.api_key`、`plan` |
| 邮箱验证 | `GET /verify_email?key=…` | 链接来自验证邮件；验证后每日 Theos 才会发放 |
| 验活 | `GET /deepai_code/theos` | 带 `Api-Key` + `X-DeepAI-Code-Machine`，200 即可用 |

关键点：

- `userprofile.api_key` **就是** DeepAICode2API 使用的上游 Key；
- 注册 / 登录都是普通 Cookie 会话，不需要浏览器、不需要 CSRF 头；
- 邮箱验证后 `/theos` 才会返回 `granted > 0`；
- 每日 Theos 按“机器指纹 + 账号”发放：同一台机器当天已领过额度时，
  新账号的 `granted` 可能为 `0`，但免费模型仍然可用。

### 6.2 链路与状态

```text
建临时邮箱 → 预检邮箱 → 注册 → 登录 → 取 Session/API Key
           → 收信验证（可选）→ /theos 验活 → 写入 2API 号池 → 台账/交付物
```

| 状态 | 含义 |
| --- | --- |
| `registered` | 会话已建立，已取到 API Key |
| `verified` | 邮箱验证链接已访问 |
| `keyed` | `/theos` 验活通过（可入池但尚未入池） |
| `enrolled` | 已写入号池 |
| `failed` | 关键阶段失败 |

台账按邮箱并集合并：高等级记录胜出，已赚到的 `api_key` / `account_id` 不会被后续失败覆盖。

### 6.3 CLI

```bash
./register.exe -doctor                     # 环境体检（不出网注册）
./register.exe -count 1                    # 注册 → 验证 → 验活 → 入池
./register.exe -count 5 -concurrency 2 -group batch-a
./register.exe -email a@b.com -password '...'   # 复用已有邮箱（count=1）
./register.exe -resume                     # 补跑台账里未入池的账号
./register.exe -no-enroll                  # 只注册/验活，不写号池
./register.exe -mail-backend static -domain example.com   # 无收件箱，跳过验证
```

常用参数：`-gateway`、`-admin-password`、`-ledger`、`-result-dir`、
`-verify=false`、`-probe-chat`、`-timeout`、`-delay`。

### 6.4 控制台

控制台新增「注册」页：

- 数量 / 并发 / 分组 / 邮箱域名 / 邮箱后端 / 自动验证；
- 启动后台任务，实时轮询每个账号的阶段、Key、Theos、错误；
- 完成后自动刷新账号池。

### 6.5 邮箱后端

| 后端 | 能力 | 配置 |
| --- | --- | --- |
| `cf` | 建邮箱 + 收信 + 自动点验证链接 | `TEMPMAIL_BASE` / `TEMPMAIL_ADMIN_KEY` / `TEMPMAIL_DOMAIN` |
| `static` | 随机前缀 `@域名`，无收件箱（跳过验证） | `DEEPAI_REGISTER_DOMAIN` |
| `none` | 使用 `-email` 指定的固定邮箱 | 显式 `-email` |
| `qqimap` | 域名随机地址 + QQ IMAP 收信验证 | `DEEPAI_REGISTER_DOMAIN` / `QQ_IMAP_USER` / `QQ_IMAP_AUTH_CODE` |
| `tempmail_lol` | 外部临时邮箱创建 + 收信验证 | `TEMPMAIL_LOL_API_KEY`（可选） |

留空自动选择：`cf` → `qqimap` → `tempmail_lol` → `static` → `none`，使用第一个已配置的后端。

域名邮箱转发到 QQ 收信的设置步骤、页面配置和 CLI 示例见 [QQ IMAP 配置](docs/qq-imap.md)。
外部临时邮箱接入见 [TempMail.lol 配置](docs/tempmail-lol.md)。

### 6.6 台账与交付物

- `exports/register-ledger.jsonl`：全部尝试（含失败），按邮箱合并；
- `exports/register-ledger-credentials.jsonl`：**仅**密码 / Session / CSRF，供 `-resume` 使用；
- `result/keys.txt`：`email----api_key----account_id`；
- `result/apikeys.txt`：纯 API Key，一行一个；
- `result/success.jsonl`：机器可读成功记录（已脱敏，不含密码）。

台账与交付物写入前会过一遍 `redactRecord`：`password` / `session_key` /
`csrf_token` 不会出现在主台账与 `success.jsonl` 里，因此台账可以直接贴进
issue 或发给别人排查。凭据单独落在 `*-credentials.jsonl`（`0600`，读取时按
邮箱并回），不需要补跑时可直接删掉这一个文件。

这些都已在 `.gitignore` / `.dockerignore` 中排除。

### 6.7 号池分组与轮询

- 账号有 `group`（默认 `default`）；
- API Key 也有 `group`，请求进来后只会在对应分组内轮询；
- 未绑定分组的 Key 使用全部分组；
- 管理员在控制台创建密钥时可指定分组。

这样可以用一个网关同时服务多个批次 / 租户，而不会串号。

### 6.8 代理池与指纹库

- 注册与推理共用同一代理池：`PROXY_POOL`（逗号 / 换行分隔的完整 URL）或
  `PROXY_POOL_FILE`（每行一个）；`DEEPAI_REGISTER_PROXY` 可指定单代理；
- 每个账号轮询分配一个代理并写入台账，补跑时复用历史代理；
- 推理侧按账号绑定的代理调用上游（`Server.clientFor`），管理接口只展示
  `host:port`，不泄露代理凭据；
- 指纹库内置 6 套自洽的 `User-Agent` / `Sec-CH-UA` / 平台 / `Accept-Language`，
  按邮箱稳定分配（同一账号补跑得到同一套请求头）；
- CLI：`-check-proxies`、`-proxy-pool`、`-proxy-file`、`-proxy`、`-no-fingerprint`、
  `-reverify`（补点验证链接，见 §6.20）；
- 控制台「注册」页可直接粘贴代理池并关闭指纹库。

### 6.9 机器指纹（为什么每号一个）

上游按 `X-DeepAI-Code-Machine` **每天发放一次**免费 Theos，因此：

- 每个账号在注册时生成一个独立的机器 UUID（`deepai.NewMachineUUID`），
  存入台账与号池，之后 `/theos` 与推理全程使用它（`Account.MachineID`）；
- 没有账号级指纹时才回退到进程级 `MachineID()`（桌面端导入 / 历史账号）；
- 共用指纹时，同一台机器上的多个账号会互相抵消额度 —— 这是旧版本
  池内 `granted=0` 的根因；
- 存量账号可一键补齐（只补空值，不覆盖已有指纹）：

```bash
curl -s -X POST http://127.0.0.1:10087/admin/api/accounts \
  -H "X-Admin-Password: $DEEPAI2API_ADMIN_PASSWORD" \
  -d '{"action":"assign-machine"}'
# {"ok":true,"assigned":20}
```

补齐后会自动异步重探配额。调度侧也会优先挑有额度的账号（`hasQuota`），
已知 `balance=0` 的账号只在全组都没额度时才兜底。

### 6.10 实测：补齐机器指纹前后的额度对比

同一份号池（21 个账号：1 个桌面端导入 + 20 个批量注册，11 个代理），
用 `assign-machine` 补齐前后各跑一次 `/admin/api/checkin`：

| 指标 | 补齐前 | 补齐后 |
| --- | --- | --- |
| 有独立机器指纹的账号 | 0 / 21 | **21 / 21**（21 个互不相同） |
| `balance > 0` 的账号 | **0 / 21** | **20 / 21** |
| 典型配额 | `balance=0.00000 granted=0.00000` | `balance=200.00000 granted=200.00000` |
| 配额刷新 | — | `refresh_at=2026-11-07T04:03:03Z`（次日 UTC 04:03） |
| 连续 8 次真实推理 | — | **8/8 HTTP 200**，落在 **8 个不同账号** |

唯一 `balance=0` 的是桌面端导入的账号（`source=desktop`，单独在
`default` 分组）——它当天在原机器指纹下的额度已被先前使用消耗。

这说明此前的 `granted=0` 不是上游策略收紧，就是**共用机器指纹**导致的：
同一指纹每天只发一次额度。补齐后每个账号各自领取 200 单位。

> 注意：`/theos` 的 `balance` 精度是 5 位小数，单次请求成本约 `1.8e-6`，
> 所以小额调用看不出余额下降；要看消耗请查 `/v1/deepai/usage`。

### 6.11 50 账号全链路压测（注册 → 登录 → 取 Key → 验活 → 入池 → 推理）

配置：CF 临时邮箱后端、代理池 10 条、并发 5、分组 `stress-50-20261106`。

| 阶段 | 结果 |
| --- | --- |
| 注册（创建账号） | **50 / 50** |
| 登录并取到 API Key | **50 / 50** |
| 邮箱验证 | **50 / 50** |
| Key 验活（`/theos`） | 48 / 50 |
| 入池 | 47 / 50，`-resume` 后 **49 / 50** |
| 耗时 | **4m40s**（并发 5） |
| 失败原因 | 全部是代理问题：`No exit node` ×2、`unexpected EOF` ×1 |

入池后 49 个账号**机器指纹互不相同**，最终 `balance` 全部为 `200.00000`，
号池总额度 **13,800.00**。

数据面压测（同一分组，临时密钥）：

| 指标 | 值 |
| --- | --- |
| 请求数 / 并发 | 60 / 6 |
| 成功率 | **60 / 60（100%）** |
| 吞吐 | 4.54 rps |
| 延迟 | p50 **1114ms**、p90 1999ms、p95 2220ms、max 3480ms |
| 账号覆盖 | **47 / 47**（轮询把整批都用上了） |
| 用量 | 1190 in / 704 out token，成本 `0.00034728` |

**压测暴露并修复的缺口**：新账号在入池那一刻 `/theos` 常返回 `balance=0`
（额度尚未下发），几分钟后复探才有 200。原实现把这个 0 当成事实缓存，
账号会被调度侧长期跳过。修复：

- 入池路径发现 0 额度时安排延迟复探（3 / 6 / 12 分钟退避，拿到即停）；
- `store.hasQuota` 把**超过 15 分钟**的 0 额度视为“未知”，重新允许调度。

复探日志示例：

```text
[quota] oai-1a2b3c4d5e6f7890 复探拿到额度: balance=200.00000 granted=200.00000
```

### 6.12 第二批：更换出口后复跑（验证延迟复探与出口容错）

同一套代码，换了代理出口（移除了持续报 `No exit node` 的那条，补入 4 条新的
`https://…bringyour.com` 出口，池内共 12 条）后再跑一批 50：

| 阶段 | 结果 |
| --- | --- |
| 注册 / 取 Key | 50 / 50 |
| 邮箱验证 | 48 / 50 |
| 入池 | 49 / 50，`-resume` 后 **51 / 51** |
| 耗时 | 8m23s（并发 5、12 出口） |
| 唯一失败 | `TLS handshake timeout`（出口瞬时抖动） |
| 自动复探 | **51 次全部命中 200**（3/6/12 分钟退避，无需人工介入） |

数据面（同分组）：

| 场景 | 成功率 | 吞吐 | p50 | p90 | max |
| --- | --- | --- | ---: | ---: | ---: |
| 60 请求 / 并发 6 | **100%** | 3.47 rps | 1221ms | 3659ms | 5252ms |
| 40 请求 / 并发 10 | **100%** | 7.87 rps | 1011ms | 1662ms | 1891ms |

100 条请求日志全部 200，覆盖 49 个不同账号。

**出口容错结论**：`No exit node` 是可复现的单出口故障 —— 把该出口上的
17 个账号改绑到新出口后，全部恢复 `balance=200`。这也说明「换出口」不需要
重新注册账号，改绑即可。

### 6.13 累计号池规模

| 指标 | 值 |
| --- | --- |
| 账号总数 | 121（4 个批次） |
| 有额度账号 | 120（唯一 0 是桌面端账号） |
| 总额度 | **24,000.00** |
| 机器指纹 | 121 个互不相同 |
| 出口 | 12 条 |

### 6.14 单号端到端速率（第三批，含逐号埋点）

`Record.FinishedAt` 起开始有逐号计时，`E2E = FinishedAt - CreatedAt`（不含排队）：

| 指标 | 值 |
| --- | --- |
| 单号平均 E2E | **27.0s**（中位 25.3s，p90 33.3s，p95 35.0s） |
| 平均速率 | **5.58s/号 = 10.75 号/分钟 = 645 号/小时** |
| 并发利用率 | 4.83 / 5 = **96.6%**（worker 几乎不空转） |
| 墙钟 | 279s（4m39s） |

阶段耗时（均值）：`verify` **14.8s（54.8%）** > `exists` 3.3s > `validate` 3.1s
> `enroll` 2.0s > `register` 1.2s > `login` 1.1s。

单号时间线显示**取 Key 早于验证完成**，所以验证等待可与后续阶段并行。

按 `docs/performance.md` 的 P0/P1/P2 已全部落地：入池不再二次探测 `/theos`
（2.0s → 0.00s）、新邮箱跳过存在性预检（3.3s → 0）、验证等待与 `/theos` 验活
并行、`/theos` 瞬时错误退避重试、并发自动压到出口数、收信自适应轮询
（起始 300ms）、台账批量 fsync、控制台直接显示单号 E2E 与阶段耗时。

实测（10 号，并发 5）：单号均值 **25.8s**，`exists`/`enroll` 开销清零，
**入池 0 失败**。E2E 剩余部分几乎全是邮件投递等待（外部服务，14~43s 波动）。

### 6.15 第四批：优化后（并发 10）

P0/P1/P2 全部落地后复跑 50 号，出口 12 条、并发 10：

| 阶段 | 结果 |
| --- | --- |
| 注册 / 取 Key | 50 / 50 |
| 邮箱验证 | 49 / 50 |
| 入池 | **50 / 50**（零失败，无需 `-resume`） |
| 墙钟 | **233s（3m53s）** |
| 吞吐 | **12.88 号/分 = 773 号/小时** |
| 并发利用率 | 6.73 / 10 = 67% |

单号端到端：均值 **31.4s**（中位 27.1s、p90 33.5s、p95 43.3s），
去掉 1 个 198s 的邮件投递离群后约 **28.0s**。

| 阶段 | 优化前（第三批） | 优化后（第四批） |
| --- | ---: | ---: |
| `exists` | 3.3s | **0（`skipped (fresh)`）** |
| `enroll` | 2.0s | **0.00s** |
| `verify` | 14.8s | 20.3s（64.6%） |
| `validate` | 3.1s | 4.3s |
| 失败账号 | 4 / 50 | **0 / 50** |

**结论**：两处可去除开销清零、失败率归零、吞吐 +20%。单号耗时已完全由
邮件投递决定（`verify` 占 64.6%），且并发 10 下利用率只有 67% —— 说明
继续加出口/并发仍有空间，瓶颈不在网关。

自动复探在这批完全生效：批次结束时 28/50 有额度，**无需任何人工干预**，
10 分钟内自愈到 49/50。

### 6.16 第五批：同一配置复跑（暴露邮件侧波动）

同配置（并发 10 / 12 出口）再跑一批 50，用来观察批次间稳定性：

| 指标 | 第四批 | 第五批 |
| --- | ---: | ---: |
| 入池 | 50 / 50 | **50 / 50** |
| 失败 | 0 | **0** |
| 邮箱已验证 | 49 / 50 | 36 / 50（14 个 120s 超时） |
| 墙钟 | 233s | 388s |
| 吞吐 | 12.88 号/分 | 7.73 号/分 |
| E2E 均值 | 31.4s | 67.2s |
| `verify` | 20.3s | **56.1s（83.6%）** |
| `validate` | 4.3s | 3.9s |
| `exists` / `enroll` | 0 / 0.00s | 0 / 0.00s |
| 并发利用率 | 67% | 87% |

**网关侧完全稳定**：`register` 1.4~1.6s、`login` 1.2~1.3s、`validate` 3.9~4.3s、
`enroll` 0.00s、`exists` 0 —— 两批没有差别。差异 100% 来自 `verify`（邮件投递）：
20.3s → 56.1s，并且 14 个账号等满 120s 超时。

**关键发现：邮箱验证超时不影响额度。** 14 个未验证账号里，7 个当时就有 200，
其余靠延迟复探在 5 分钟内补上；本批最终 **49/50 有额度**，全部零人工干预。

这说明 `verify` 目前虽然占了 83.6% 的 E2E，却不在任何必要路径上 ——
下一步应把它彻底移出关键路径（见 `docs/performance.md` §10）。

### 6.17 第六批：验证移出关键路径后（P0-4）

入池不再等邮箱验证，改为后台补验证 + 回填 `email_verified`。

| 指标 | 第五批（阻塞验证） | 第六批（P0-4） |
| --- | ---: | ---: |
| 单号 E2E 中位 | 52.5s | **12.3s** |
| 单号 E2E 均值 | 67.2s | **12.6s**（仅统计入池成功的） |
| p90 / 最小 | 107.6s / 36.4s | 15.6s / **9.1s** |
| 后台验证回填 | — | **33 / 34** 成功，0 失败 |

按中位 12.3s、并发 10 推算：50 号墙钟 **≈ 61s、约 49 号/分**，
与 `docs/performance.md` 的预测（~11s / ~45 号/分）基本一致。

**这一批同时暴露了新的上游故障**：16/50 卡在
`POST https://api.deepai.org/daily-time-sync/registration/` 返回 **504 Gateway
Timeout**，单个账号白等 188~219s，把墙钟拖到 7m10s。随后 20 号确认批次
（并发 10）仍是 19/20 成功、1 个同样 504，中位 E2E **11.9s**，说明 P0-4 本身
稳定，504 是上游侧抖动。该失败模式目前**没有重试**，是下一项待办。

### 6.18 第七批：注册 POST 重试 + 后台验证完善

**修复的两处**（都在第六批暴露）：

1. **注册 POST 不再被 504 拖死**：给 `/daily-time-sync/registration/` 加 45s
   总预算、重试退避改为可被 ctx 打断（原来超时后还要睡完 700ms+1400ms），
   并且失败后**用登录兜底** —— 504 时账号可能已经建成，登录成功即继续。
2. **CLI 退出前等待后台验证**（`-verify-grace`，默认 60s）：后台验证跑在 CLI
   进程里，批次一结束进程退出就被带走；现在会等一会儿并从台账刷新汇总，
   交付物与汇总里的 `email_verified` 才是真的。

| 指标 | 第六批 | **第七批** |
| --- | ---: | ---: |
| 入池 | 34 / 50 | **50 / 50** |
| 失败 | 16（全 504） | **0** |
| 墙钟 | 430s | **82s（1m22s）** |
| 吞吐 | — | **36.6 号/分 = 2,195 号/小时** |
| 单号 E2E 中位 | 12.3s | **11.7s**（均值 12.5s） |
| p90 / 最小 | 15.6s / 9.1s | 19.1s / **7.9s** |
| 并发利用率 | — | 7.62 / 10 = 76% |

对照计划（`docs/performance.md` §10 预测 ~11s / ~45 号/分）：单号 11.7s ✔、
吞吐 36.6 号/分（略低于 45，因为 76% 利用率还有余量）。

grace 等待的独立验证（5 号小批，并发 5）：`等待 5 个后台邮箱验证（最多 1m0s）…`
→ 完成后汇总 `邮箱已验证 5`，用时 27s。

### 6.19 第八批：上游抖动下的稳定性

同一配置（并发 10 / 12 出口）再跑一批 50：

| 指标 | 第七批 | 第八批 |
| --- | ---: | ---: |
| 入池 | 50 / 50 | **50 / 50** |
| 失败 | 0 | **0** |
| 墙钟 | 82s | 228s（含 60s grace 等待） |
| 单号 E2E **中位** | 11.7s | **11.9s** |
| E2E 均值 / p90 / 最大 | 12.5s / 19.1s / 33.0s | 25.4s / **89.2s** / 121s |
| 邮箱验证 | 33/34（该批未含 grace） | **0 / 50**（见下） |

**单号中位 11.9s 几乎没变**，说明 P0-4 的架构在上游变慢时依然稳定；
变的是长尾与阶段耗时：

| 阶段 | 第七批 | 第八批 |
| --- | ---: | ---: |
| `register` | 1.4s | **6.6s** |
| `login` | 1.3s | **4.2s** |
| `session` | 0.6s | **3.0s** |
| `validate` | 4.3s | **6.7s** |
| `enroll` | 0.00s | 0.00s |

**验证 0/50 的原因不在网关**：这批 33 个账号拿到了验证链接，但
`GET https://api.deepai.org/verify_email?key=…` 全部失败 —— 17 次客户端超时、
16 次 `unexpected EOF`、2 次 `502 Incomplete response`。事后单独复测这些链接
（直连与走代理各一次）仍然是 curl code 28（20s 无响应），说明**上游该端点当时
在故障**。代码侧无需改动：`VerifyEmail` 已有 3 次重试，且验证不在关键路径上，
额度也不依赖它（本批 47/50 当场拿到 200，随后补齐）。

### 6.19.1 验证点击限流（修复第八批 0/50）

HAR 实测（`deepai.org-2.har`，2026-11-06 17:07 本地）：

```text
GET https://api.deepai.org/verify_email?key=…  →  200 text/html
timings: wait 13.2s + ssl/connect 0.37s + receive 0.33s = 13.9s
```

**端点可用，但每次要占上游 Passenger worker 约 13 秒。** 第八批 10 个后台
goroutine 同时点，把 worker 池打满，于是出现 `502 Incomplete response received
from application` / `unexpected EOF` / 60s 超时 —— 0/50 是**自己把上游打限流了**，
不是端点坏了（同时段的浏览器注册 `201 / 4.8s` 完全正常）。

改动：

- 验证点击加**全局并发闸门 2**（`verifyClickSlots`），退避等待期间不占名额；
- 重试序列改为 **2s / 10s / 60s**（原来只有 `do()` 的 700ms/1400ms，对 13s 级
  的重端点没意义），4xx 不重试；
- `VerifyEmail` 改为单次尝试，重试统一由外层负责，避免嵌套；
- 链接校验从「硬编码 api.deepai.org」改为「跟随客户端 Origin + `/verify_email`
  路径」，既支持自建后端，也不会被当成开放代请求器；
- 新增 `register -reverify -group <分组>`：从 CF 收件箱重新取链接 → 限流点击 →
  回填 `email_verified`。

实测补齐 `stress-50i`（50 个账号，2 并发）：

| 轮次 | 成功 | 失败 | 耗时 |
| --- | ---: | ---: | ---: |
| 第一轮 | **49 / 50** | 1（上游 500，瞬时） | 8m04s |
| 第二轮（重试失败项） | **1 / 1** | 0 | <1m |
| **合计** | **50 / 50** | 0 | — |

### 6.20 第九批：限流后的验收（至今最好的一批）

同配置（并发 10 / 12 出口），验证点击限流生效后复跑：

| 指标 | 第七批 | 第八批 | **第九批** |
| --- | ---: | ---: | ---: |
| 入池 | 50/50 | 50/50 | **50/50** |
| 失败 | 0 | 0 | **0** |
| 墙钟 | 82s | 228s | 126s（含 60s grace） |
| 单号 E2E **中位** | 11.7s | 11.9s | **8.9s** |
| E2E 均值 / p90 | 12.5s / 19.1s | 25.4s / 89.2s | **10.0s / 13.4s** |
| 最小 / 最大 | 33.0s | 121s | **4.9s / 27.1s** |
| 吞吐（纯注册段） | 36.6 号/分 | 13.2 | **45.5 号/分** |
| 邮箱验证 | — | 0/50 | **49/50 → 补 1 → 50/50** |
| 额度 | 47/50 | 47/50 | **50/50** |

阶段耗时：`validate` 2.9s、`register` 1.2s、`login` 1.0s、`session` 0.6s、
`mailbox` 0.3s、`enroll` **0.00s**，`verify` 全部 `pending`（后台）。

- 注册段 66s 跑完 50 个账号 = **1.32s/号**，与 `docs/performance.md` 的
  理论上限（~45 号/分）完全吻合；
- 验证限流后**批次内就完成了 49/50**（第八批是 0/50），剩余 1 个用
  `-reverify` 一条命令补齐；
- 完成后 30 秒内延迟复探把额度补齐：**50/50 都有 200**。

号池累计跨过 500 个账号 / 100,000 额度。

### 6.21 第十批 + 今日号池全量验货

第十批（并发 10 / 12 出口）配置未变：

| 指标 | 第九批 | 第十批 |
| --- | ---: | ---: |
| 入池 / 失败 | 50/50 · 0 | **50/50 · 0** |
| 墙钟 | 126s | 124s（含 60s grace） |
| E2E 中位 / 均值 | 8.9s / 10.0s | **10.3s / 10.6s** |
| 吞吐（注册段） | 45.5 号/分 | **46.9 号/分** |
| 邮箱验证 | 49/50 → 补 1 | 49/50 → 补 1 |

**今日号池全量验货**（553 个账号，逐个复探 0 额度、补验证、再打一轮推理）：

| 检查项 | 结果 |
| --- | --- |
| 账号总数 | 553 |
| 有额度 | **551 / 553（99.6%）** |
| 邮箱已验证 | 486 |
| 启用 / 停用 | 553 / 0 |
| 状态 ok / error | 551 / 2 |
| 机器指纹唯一 | **553**（无重复） |
| 代理出口 | 12 条 |
| 总额度 | **110,200** |
| 推理验货（60 请求 / 并发 10） | **60/60 成功**，7.36 rps，p50 1078ms，覆盖全部 50 个账号 |

两个非满格项都在预期内：

- `epsoft-*`（桌面端账号）额度为 0 —— 当日额度已被先前使用消耗；
- `oai-6b196a1e…`（第七批）额度 0 且未验证 —— 该批次验证发生在上游故障窗口，
  邮件已随临时邮箱过期（见下）。

**关于「补验证」的边界**：CF 临时邮箱的邮件只保留很短时间。第九/十批靠
「批次内验证 + `-reverify` 补 1 个」做到 100%；而第七批那 39 个未验证账号，
事后 `-reverify` 全部失败于 `等待验证邮件超时`（25s 内收件箱里已没有邮件）——
**验证必须发生在批次内**，这也是把验证点击限流（而不是事后补）的真正原因。

### 6.22 安全与合规

- 注册能力用于管理**自己的** DeepAI 账号；请遵守上游服务条款，控制批次大小与频率；
- 状态文件与台账权限 `0600`；但 **Windows 上 `0600` 不设 ACL**，同机其他用户仍可读，
  因此强烈建议配置 `DEEPAI2API_MASTER_KEY` 开启整文件加密（见 §七）；
- 台账与 `success.jsonl` 不含密码；密码只在 `*-credentials.jsonl` 里，可单独删除；
- 不需要补跑时可删除台账或使用 `-no-enroll` / 不保存密码；
- 上游按机器发放每日免费额度，本项目不绕过该限制。

---

## 七、控制台

- **概览**：请求量、失败数、token 入/出、上游成本、模型目录（免费 / 付费、Theos、上下文、端点）。
- **账号**：导入桌面端凭据、浏览器登录、手工添加 API Key、刷新配额、启停 / 删除 / 清错。
- **密钥**：签发 `sk-…` 网关密钥；只在创建时显示一次。
  **未签发任何密钥时，仅本机回环允许无密钥调用；非回环来源一律 `503 setup_required`。**
  若监听非回环地址且尚无密钥，启动时会自动生成一个 `bootstrap` 密钥并打印一次。
- **日志**：端点、模型、流式、状态、耗时、字符数、token、成本、账号。
- **设置**：超时、重试、日志保留、CORS、兜底模型、模型别名、管理密码。

安全默认值：

- 监听**非回环**地址时必须设置管理密码，否则拒绝启动（如需绕过用 `-allow-insecure`）；
- 未签发密钥时，非回环来源的数据面请求返回 `503 setup_required`，不是放行；
- 未设置管理密码时，管理接口**只允许本机环回**；
- 写操作做同源校验，阻断 CSRF；
- CORS 默认仅允许本机来源，不提供 `*`；
- 状态文件权限 `0600`，控制台只展示脱敏后的 Key。
- 配置 `DEEPAI2API_MASTER_KEY` 后状态文件整体 AES-256-GCM 加密，
  磁盘上不再出现明文上游 Key / 账号密码；已加密的文件在缺少或错配主密钥时
  **拒绝启动**，不会静默重置成空状态。
- 状态文件默认落在 `os.UserConfigDir()`（Windows `%AppData%`、Unix `~/.config`），
  避免落在 Program Files 或共享盘；旧位置已有文件时会继续沿用，不会丢号池。

---

## 八、架构与目录

```text
main.go                     启动、导入、后台落盘 / 目录刷新
cmd/probe/                  诊断工具
internal/deepai/            上游客户端（协议、SSE、机器指纹、凭据导入）
internal/registry/          模型目录解析与别名
internal/store/             状态持久化 + 账号池调度
internal/register/          注册链路（stages / runner / ledger / mail backend）
  ├─ web.go                DeepAI 网页注册 / 登录 / Session / 验证
  ├─ mailbox.go            CF Temp Email / static / none
  ├─ pipeline.go           单账号阶段 + 批量并发 + 重试
  ├─ ledger.go             JSONL 台账（并集合并 / 升级保护）
  ├─ gateway.go            入池 HTTP 客户端
  └─ deliver.go            keys.txt / apikeys.txt / success.jsonl
cmd/register/               注册链路 CLI（doctor / count / resume）
internal/server/            HTTP 路由、协议转换、控制台
  ├─ chat.go                OpenAI Chat Completions（透传 + 归一化）
  ├─ anthropic.go           Anthropic Messages（含 tool_use / thinking）
  ├─ responses.go           Responses API（透传 / Chat 合成）
  ├─ extensions.go          配额、用量、搜索、语音
  ├─ login.go               浏览器登录（RFC 8252 本机回调）
  └─ web/index.html         单页控制台
```

分层原则：

- **`deepai`** 只关心上游协议（路径、请求头、错误语义、SSE），不知道号池存在；
- **`store`** 只负责状态与调度（持久化、分组、失败换号、配额判定），不发网络请求；
- **`server`** 只做协议转换与鉴权，账号选择一律通过 `Store`；
- **`register`** 是独立的客户端：它通过管理接口入池，不直接改状态文件。

这样上游协议变动只影响 `deepai`，调度策略变动只影响 `store`，
测试可以分别对着假上游和临时状态文件跑。

---

## 九、部署（Docker）

```bash
docker build -t deepaicode2api .
docker run -d --name deepaicode2api \
  -p 10087:10087 \
  -e DEEPAI_API_KEY=<api-key> \
  -e DEEPAI_MACHINE_ID=<原始机器UUID> \
  -e DEEPAI2API_ADMIN_PASSWORD=<管理密码> \
  -v $PWD/data:/data \
  -e DEEPAI2API_DATA_PATH=/data/deepaicode2api-data.json \
  deepaicode2api
```

> `X-DeepAI-Code-Machine` 是**所有模型请求的必需头**：缺失时上游直接返回
> `426 lane_version_unsupported`。容器里通常读不到宿主机的 SMBIOS UUID，
> 因此 `DEEPAI_MACHINE_ID` 在容器部署时**必填**。填桌面端所在机器的原始 UUID
> 即可复用同一份每日免费额度。

---

## 十、限制与说明

- `X-DeepAI-Code-Machine` 缺失时上游对所有模型请求返回 426，因此无 SMBIOS 的环境必须设置 `DEEPAI_MACHINE_ID`；
- 上游免费额度按「机器指纹 + 账号」每天发放，额度耗尽后返回 403；
  机器指纹**每个账号一份**（见 §6.9），共用时只有第一个账号能领到；
- `gpt-6-astra` 等 `openai-responses` 模型必须走 `/v1/responses`；用
  `/v1/chat/completions` 调用会被上游拒绝（网关会原样回传错误）；
- 语音转写上游只接受 `audio/webm` / `audio/mp4`，其他格式请先转码；
- `/v1/messages/count_tokens` 是字符数估算，不是上游真实 tokenizer；
- 状态文件默认明文保存上游 API Key（与桌面端一致）；配置
  `DEEPAI2API_MASTER_KEY` 后整文件加密，请务必保证主密钥不随备份泄露；
- 注册链路会把上游 API Key、网页密码、Session 与完整代理 URL 写入本机状态文件；
  主台账与 `success.jsonl` 已脱敏，密码只在 `*-credentials.jsonl` 里；
- 代理 URL 含账号密码，必须只放在 `.env` / 本地台账（已 gitignore），不要提交。

### 10.1 已知坑（改动前先看）

以下都是踩过的坑，`docs/recon.md` 有更完整的逆向记录：

| 现象 | 原因 | 不要“修”成什么样 |
| --- | --- | --- |
| 无效 Key 返回 200 且是 HTML | 上游 302 到登录页 | 不要 `CheckRedirect` 跟随；`ErrUseLastResponse` 是刻意的 |
| 上游模型目录默认模型为空 | 字段叫 `default` 不是 `default_model` | 不要加 `default_model` 兼容，会静默拿不到值 |
| 池内账号 `granted=0` | 共用机器指纹，每日额度只发一次 | 不要去掉 `Account.MachineID` 回退逻辑 |
| 长响应被掐断 | 上游心跳 `: keep-alive` 被丢弃 | `streamChat` 要原样透传注释行 |
| 工具调用参数被截断 | 同一事件多行 `data:` 未按 `\n` 拼接 | 用 `deepai.ReadSSE`，不要逐行 `json.Unmarshal` |
| 台账里密码消失 | `Append` 走了 `redactRecord` | 这是设计；密码在 `*-credentials.jsonl` |
| `go test -race` 链接失败（`cannot find 'ld'`） | mingw runtime 只带 `ld.bfd.exe` | 不要求改 PATH；跑 `make race-shim` 补一个同名副本 |
| 非流式请求返回 502「不是合法 JSON」 | 上游**缺省 `stream` 时默认按 SSE 返回** | 必须显式下发 `stream:false`；别把 `stream` 当可选字段略过 |

另：`/v1/chat/completions` 的 `reasoning_content` 是网关补的兼容字段，
上游原生只有 `reasoning`；已存在时不会被覆盖。

## 十一、文档索引

| 文件 | 内容 |
| --- | --- |
| `README.md` | 总览、接口、参数、压测与已知坑（本文） |
| `docs/recon.md` | 逆向速查：上游端点、请求头、错误语义、复现命令（替代 HAR） |
| `docs/changes.md` | 2026-11 的安全 / 正确性改造记录：根因、决策与验收 |
| `docs/performance.md` | 压测数据与优化方案：单号 E2E、阶段分解、P0/P1/P2 收益测算 |
| `docs/optimization-plan.md` | 首轮扫描的完整优化方案（已全部实施，保留作为设计依据） |
| `.env.example` | 全部可配置项与说明 |
| `Makefile` | `make build / test / test-race / lint / cover` |

## 十二、License

MIT。本项目仅复刻本机已登录客户端的调用链路，请遵守 DeepAI 的服务条款。
