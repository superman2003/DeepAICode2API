# Claude Code、Codex 与缓存

## Claude Code / CC Switch

使用 Anthropic Messages 协议。Base URL 为 `http://服务器IP:10087`，不加 `/v1`，
API Key 使用本网关签发的密钥。配置示例在 `examples/claude-settings.json`，请替换地址与占位密钥。

适配层保留 system、文本、图片、tool_result 和工具定义上的 `cache_control`，
以及请求级自动缓存设置。会话标识会映射成稳定的 `prompt_cache_key`，
同一会话优先使用同一账号；账号不可用时仍会换号。绑定按网关密钥和分组隔离，
只保存账号绑定关系，不在内存中保存完整对话。

非流式响应及流式最终 `message_delta` 会返回上游实际给出的
`cache_read_input_tokens`、`cache_creation_input_tokens`。Anthropic 的 `input_tokens`
不含缓存读写部分，适配时会正确扣除，避免重复计数。
上游只在末帧返回 usage 时，完整缓存用量也只能在流结束时确定。

## Codex

使用 HTTP Responses 协议，Base URL 为 `http://服务器IP:10087/v1`。
参考 `examples/codex-config.toml`，合并到自己的 Codex 配置，不要直接覆盖其他供应商设置。
在运行 Codex 的 PowerShell 中设置：

```powershell
$env:DEEPAI2API_GATEWAY_KEY="你的网关API密钥"
```

示例明确关闭 WebSocket，使用 HTTP/SSE。支持 Responses 的文本流、事件序号、
工具参数增量、并行 function_call 历史、自定义工具的文本输入及回传。
自定义工具会转换成一个带 `input` 字符串参数的函数工具，返回时恢复为 `custom_tool_call`；
工具输入保留原文，自定义 grammar 不会由网关执行验证。

流中断或上游错误会结束为 `response.failed`；达到输出上限会结束为 `response.incomplete`，
不会把截断结果标记为完整成功。缓存读写用量取自上游真实 usage。

Chat 桥接路径不保存对话正文，客户端需要传完整 `input` 历史；
`previous_response_id` 会返回明确错误，避免静默丢失上下文。
上游原生 Responses 模型则按原协议透传该字段。

## 缓存的边界

这不是本地答案缓存，不会将上一次模型回答直接复用于另一条请求。
是否缓存、最低前缀长度、TTL 和费用由实际上游模型决定；
上游报告 0 时，网关也报告 0，不会伪造命中。
控制台概览和每条日志显示缓存读取、写入 token。

协议依据：[Anthropic 缓存文档](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)、
[OpenAI Responses](https://developers.openai.com/api/reference/resources/responses/methods/create)、
[OpenAI 流式事件](https://developers.openai.com/api/reference/resources/responses/streaming-events)、
[Codex 配置参考](https://developers.openai.com/codex/config-reference/)。

## 注册数量

单批注册默认上限 1000，并发仍为单独设置的 1-5，按队列处理。
在 `.env` 设置 `DEEPAI2API_REGISTER_MAX_COUNT=5000` 可提高上限，硬上限 10000。
注册页读取后端限制更新输入框；修改 `.env` 后重启。
TempMail.lol 的轮询间隔、共享冷却及 429 退避不随批次数量缩短。
