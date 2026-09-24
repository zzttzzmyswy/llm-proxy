# llm-proxy

Anthropic API 透明代理：把 Claude Code 的请求转发到上游（如 SOPHNET），并做**模型路由**，让 Claude Code 直接使用上游各模型（文本 / 视觉）。

## 为什么需要它

Claude Code 默认只认 Anthropic 官方模型名（`sonnet` / `opus` / `haiku`）。通过一个本地 HTTP 代理，你可以在不修改 Claude Code 的前提下：

- 把 `sonnet` / `opus` / `haiku` 映射到其他模型（如 DeepSeek、GLM）
- **Anthropic→OpenAI 协议翻译**：命中 OpenAI 网关的路由（如 `glm-5.3-flash`，只在 `api.sophnet.com/v1` OpenAI 端注册、Anthropic 受限网关不支持的模型）自动把请求翻译为 OpenAI 格式转发，并把回复（含流式 SSE）翻译回 Anthropic 格式——Claude Code 无需感知网关差异
- **图片先行描述**：带图片的请求先交给视觉模型（VLM）生成图片说明，再把说明以文本形式插入原位置，最后路由到文本模型——这样文本模型也能"看懂"图片，且所有文字始终走文本 LLM
- 修复上游 SSE 流缺 `message_stop` 时 Claude Code 卡死的问题
- 暴露一个 OpenAI 兼容端点（`/v1/chat/completions`）

## 路由策略

```
文字请求 ──→ 路由到文本模型（sonnet/opus/haiku 按配置映射）
带图请求 ──→ 逐张送 VLM 描述 ──→ 用文本块替换 image 块（"这里有一个 image，其内容如下：xxx"）
             │   ↑ 描述时携带该图所在消息的上下文（角色、同消息文本、产出该图的工具调用）
             │   │
             │   └─ 路由标记 supports_image = true（上游模型原生支持图片）时 ▾ 跳过整个描述流程
             └─ 描述失败时回退：整请求原样路由到 VLM，保证图片不丢失
```

> **上下文感知描述**：VLM 不再只看孤立的图片，而是带上"带图那一条消息"的局部上下文（消息角色、同消息文本块、`tool_result` 内图片对应的工具名与入参），让描述贴合对话意图（如报错截图、界面截图）。
>
> **描述缓存**：VLM 描述按"图片内容哈希 + 消息上下文指纹"缓存（上限 20MB，LRU 淘汰）。同一张图在相同上下文里跨轮复用；同图不同上下文会重新描述，避免用错场景的描述。

## 功能

| 特性 | 说明 |
|------|------|
| 模型路由 | 任意别名映射到上游模型:字符串默认走 `[upstream] default_upstream` 指定的网关,表值 `{ model = "...", upstream = "openai" }` 走 OpenAI 网关(协议自动翻译) |
| OpenAI 网关路由 | 任意别名可配置 `{ model = "x", upstream = "openai" }`，请求自动翻译为 OpenAI 格式走 OpenAI 网关，回复翻译回 Anthropic（含流式 SSE）；`sonnet/opus/haiku` 等字符串形式默认走 Anthropic 网关 |
| 默认网关 | `[upstream] default_upstream`：`""`/`"claude"`/`"anthropic"`（默认）→ Anthropic 网关，`"openai"` → OpenAI 网关；未显式声明 `upstream` 的 routing 条目（含内置兜底目标）被回填 |
| image 能力声明 | `supports_image = true` 表示上游模型原生支持图片输入，带图请求跳过内置 VLM 描述直接发给该模型（含 image 400 重试一并跳过）；缺省 `false` 走 VLM 描述 |
| haiku 缺省 | 配置未声明 `haiku` 时自动沿用 `sonnet` 的目标 |
| 图片先描述后路由 | 带图请求先经 VLM 描述，把说明文本插入原图位置，再发给文本模型 |
| 上下文感知描述 | 描述带图消息时携带局部上下文（角色、同消息文本、工具名/入参），描述贴合对话意图 |
| 描述缓存（20MB） | 按图片哈希 + 上下文指纹缓存，同图同上下文复用，同图异上下文重新描述 |
| 描述失败回退 | VLM 描述调用失败时整请求路由到 VLM，图片不丢失 |
| 图像数据 URL 转换 | OpenAI 风格 `image_url` 的 `data:` URL 转成 Anthropic image 块再送 VLM |
| SSE 透传 | 流式响应透传，同时规范化损坏的 thinking 块，防 Claude Code 崩溃 |
| thinking 剥离 | 转发前剥离历史 thinking 块（含嵌套 tool_result），保留文本与 `thinking` 参数，杜绝上游 "must be passed back" 400 |
| thinking 400 兜底 | 剥离后仍遇 thinking 400 时自动重试一次（禁用 `thinking` 参数退出思考模式），参数已无则不重试 |
| message_stop 安全网 | 仅对 SSE 流补发缺失的 `message_stop`，防 Claude Code 卡死 |
| 空响应检测 | 上游 0 字节响应 → 发 `error` SSE 事件，触发 Claude Code 重试 |
| 压缩禁用 | `DisableCompression: true`，避免 gzip 破坏 SSE 缓冲 |
| 超时保护 | `header_timeout_seconds: 120s`，上游不响应头时按瞬态错误**自动重试**（`max_retries` 次）后向下游报错 |
| 流停滞保护 | `body_idle_seconds: 90s`，流式上游中途静默（模型挂起/连接假死）时向客户端发 SSE `error` 事件终止，杜绝 Claude Code 永久等待 |
| 瞬态重试 | 上游网络错误（超时/连接重置/EOF/**DNS 解析失败**）或 429/5xx 自动重试（默认 2 次，退避 0.5s/1s/2s），重试耗尽可能的 5xx 原样返回 |
| DNS 兜底 | 上游域名的解析结果进程内缓存 5 分钟；解析失败（SERVFAIL / REFUSED / UDP 读超时）时回退到最后一次可用地址并打 `[DNS]` 告警，本机 DNS 抖动不再直接变成 502。故障期间每 30 秒才重新探测一次解析器，避免每个请求都等一遍解析超时。NXDOMAIN 是确定性答案，不重试 |
| 错误帧格式 | 上游失败时非流返回 Anthropic error JSON envelope、流式返回 SSE `error` 事件，客户端可解析而不会悬置 |
| 密钥安全 | 支持 `SOPHNET_API_KEY` 环境变量，无需明文落盘 |
| Web 管理页面 | `/admin`：可视化修改配置（改完立即生效）+ 查看各模型调用情况（TPM/RPM、失败率、失败分类与明细、延迟）。HTTP Basic 认证，未配置口令时整体关闭 |
| 历史速率图 | `/admin` 上的趋势图：按模型堆叠的 token/请求速率，以及缓存命中率、失败率、平均延迟三条「稳定性」曲线，可切 1 小时 / 6 小时 / 24 小时窗口 |

## Web 管理页面

浏览器打开 `http://<host>:8088/admin`，用配置里的 `[admin] token` 作为口令（HTTP Basic，用户名任意）。

页面分四块：

- **总览 / 模型调用情况**：每个模型一行，含请求数、成功/失败、失败率、TPM（最近 60 秒 token）、RPM（最近 60 秒请求）、输入/输出 token 累计、缓存命中率、平均与最大延迟，以及最近 30 分钟每分钟 token 的迷你趋势图。按实际发给上游的模型名聚合，并列出打到该模型的别名分布。
- **历史速率**：趋势图 + 区间小结，用来评估上游平台的稳定性（见下）。
- **失败情况**：按分类（`upstream_5xx` / `rate_limited` / `upstream_4xx` / `network_timeout` / `network_error` / `empty_response` / `stream_stalled` / `upstream_stream_error` / `translate_error` / `vlm_describe_failed`）计数，并列出最近失败的时间、模型、别名、分类、状态码与消息。网关在 HTTP 200 的流内以 `error` 事件报的错也会计入 `upstream_stream_error`，不会被当成成功。
- **配置**：结构化表单编辑代理参数、上游地址与超时、上游密钥、管理页面口令、`[routing]` 路由表（可增删改）。保存后**立即生效**（进程内热重载），无需重启服务。表单编辑的是**文件里声明的**路由，另有只读的「生效路由」展示回填默认网关与内置兜底之后每个别名实际走哪个模型——这样修改 `default_upstream` 会真正影响那些没有显式声明网关的条目。

### 历史速率图

窗口三档，每档的桶长都整除一小时，刻度因此落在整点上：

| 窗口 | 桶长 | 点数 |
|------|------|------|
| 1 小时 | 1 分钟 | 60 |
| 6 小时 | 5 分钟 | 72 |
| 24 小时 | 15 分钟 | 96 |

五种口径共用同一份数据（切换口径不重新请求）：

- **Token 速率 / 请求速率**：堆叠面积图，纵轴是**每分钟**的量。后端给的是桶内总量，前端按桶长归一化，所以切窗口时曲线量级可比。
- **缓存命中率**：叠加折线，纵轴固定 0–100%。口径是 `命中缓存的 prompt token / 全部 prompt token`（分母含写缓存那部分）。命中率掉下来意味着上游的 prompt 缓存没生效——同样的请求重新全价计费，通常是上游侧缓存被驱逐或路由漂移的信号。
- **失败率 / 平均延迟**：叠加折线（率不能堆叠）。失败率纵轴同样固定 0–100%，否则一次失败就把曲线顶满，看不出「本来是 0」。

两个网关的缓存计数方式不同（Anthropic 的 `input_tokens` 不含缓存计数，OpenAI 的 `prompt_tokens` 已经含 cached），差异在解析层就抹平了，所以同一个「命中率」在两条路径上含义一致。上游完全不报缓存计数时读作 0%。

图例可点掉单个模型；鼠标划过出该桶的明细。图下方是所选区间每个模型的请求数、失败数、失败率、token、缓存命中率、平均与最大延迟，按失败率降序——**失败率从 0 开始爬、延迟抬升、命中率掉下来，都是上游开始不稳的信号**，比单看总量更早。「模型调用情况」表也带一列缓存命中。

口径与后端实现：

- 历史是**进程内**的 24 小时分钟环形缓冲（每模型 1440 个槽），与「最近 60 秒 TPM/RPM」用的秒级环形桶并存：后者给精确的当前值，前者给长趋势。**重启清零**，不做落盘——统计本来就是内存态。
- 每个请求记入 token、请求数、失败数、延迟累计、单次最大延迟，以及缓存命中 / 写入的 prompt token。
- 模型数超过 8 个时，其余合并为 `(other)` 一条，避免十几个颜色糊在一起；页面会说明合并了几个。区间总数仍然是全部模型的合计，不会因为折叠而少算。
- 一次失败会被 `max_retries` 重试放大：代理只在**重试全部失败**后才记一条失败（客户端看到的那次）。所以图上低于上游真实抖动是正常的。


### 启用

```toml
[admin]
token = "your-admin-password"
```

或用环境变量（推荐，避免明文落盘）：

```bash
export LLM_PROXY_ADMIN_TOKEN="your-admin-password"
```

`token` 留空时管理页面**整体关闭**，所有 `/admin*` 返回 404——代理监听在可路由地址上，匿名可用的配置编辑器会把上游密钥和路由表暴露给整个网段。环境变量 `LLM_PROXY_ADMIN_TOKEN` 非空时例外：它优先于文件，此时文件里 `token` 为空也不影响页面开放。

### 保存行为

保存配置时：

1. 校验（端口范围、URL、路由别名与模型名、`upstream` 取值）；不合法直接拒绝，配置文件不动。
2. 把当前配置备份为 `<配置文件>.bak.<时间戳>`。
3. 生成带说明注释的 TOML，先写临时文件再原子替换。
4. 重新加载并热切换。

> **取舍**：重写会**丢失原文件里手写的注释与注释掉的备选路由**（生成的文件带标准注释，但你的自定义注释不会保留）。备份文件是回滚路径。原文件中管理页面不认识的顶层字段也会被丢弃，保存时会在页面上明确告警。
>
> **管理页面口令可以在页面上修改**（留空则保持不变，勾选「清除口令」则关闭管理页面）。修改口令后当前页面持有的旧凭据失效，页面会自动刷新并提示用新口令重新登录。若口令来自 `LLM_PROXY_ADMIN_TOKEN`，页面上改不动它，保存时会告警；此时勾选「清除口令」只清空文件里的口令，页面仍由环境变量保持开放，告警会如实说明这一点。
>
> **端口变更不会热生效**，保存时页面会提示需重启服务；其余配置项立即生效。

### 统计口径

统计是**进程内内存态**，重启清零。token 用量优先取上游返回的 `usage`；OpenAI 网关的流式响应若未返回 usage（网关未支持 `stream_options.include_usage`），该模型的 token 数为按文本长度估算，页面会标「估算」标记，不把估算值伪装成精确值。

「输入 token」指**未命中缓存的 prompt**：Anthropic 的 `input_tokens` 本来就不含缓存计数，OpenAI 的 `prompt_tokens` 含 cached，代理会把后者减掉，两条路径口径一致。所以输入 token 比过去显示的数值小，少掉的那部分就是缓存命中的 token（页面另有缓存命中率一列）。

## 架构

```
Claude Code ──HTTP──> llm-proxy (:8088) ──HTTP──> 上游 anthropic/openai 端点
                          │
                          ├─ 文字请求 → 按模型名映射到文本目标模型
                          ├─ 带图请求 → VLM 描述后插入文本，再发文本模型
                          │               └─ 描述按图片哈希缓存（20MB LRU）
                          └─ /admin    → Web 管理页面（配置热重载 + 调用统计）
```

## 快速开始

### 1. 配置

复制示例配置并填写上游密钥：

```bash
cp config.example.toml /etc/llm-proxy/config.toml
# 编辑 config.toml，填入真正的 sophnet 密钥
```

密钥也可通过环境变量提供（推荐，避免明文落盘）：

```bash
export SOPHNET_API_KEY="your-key-here"
```

### 2. 构建与运行

```bash
sh build.sh             # 把 admin.js 内联进 admin.html → admin_page.html（改了页面才需要）
go build -o llm-proxy .
./llm-proxy            # 默认监听 :8088
```

管理页面的源码是 `admin.html`（结构 + 样式）与 `admin.js`（脚本）两个文件，`build.sh` 把脚本内联成单文件 `admin_page.html` 供二进制 `go:embed`。拆开是为了让图表的纯计算部分能在 node 下单测（`admin_js_test.js`），页面本身仍然是一个自包含文档、不依赖 CDN。改了页面没跑 `build.sh` 时 `TestAdminPageIsUpToDateWithItsSources` 会失败，不会静默发出旧页面。

配置路径默认 `/etc/llm-proxy/config.toml`，可用 `LLM_PROXY_CONFIG` 覆盖：

```bash
LLM_PROXY_CONFIG=/path/to/config.toml ./llm-proxy
```

### 3. 以 systemd 服务运行

```bash
sudo cp llm-proxy /usr/local/bin/llm-proxy
sudo cp llm-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now llm-proxy
```

## 客户端配置（Claude Code）

```bash
# ~/.zshrc
export ANTHROPIC_BASE_URL="http://localhost:8088"
export ANTHROPIC_AUTH_TOKEN="<任意值，代理会替换为真实上游密钥>"
```

```json
// ~/.claude/settings.json
{
  "model": "sonnet",
  "apiBaseUrl": "http://localhost:8088"
}
```

## 配置参考

`config.example.toml` 中的字段：

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `proxy.port` | `8088` | 监听端口 |
| `proxy.vlm_model` | `Qwen3.5-397B-A17B` | 用于图片描述的视觉模型 |
| `proxy.vlm_max_tokens` | `8000` | 图片描述请求的最大输出 token 数 |
| `upstream.anthropic_url` | `https://www.sophnet.com/api/open-apis/anthropic` | Anthropic 风格上游 |
| `upstream.openai_url` | `https://www.sophnet.com/api/open-apis/openai` | OpenAI 风格上游 |
| `upstream.default_upstream` | `""`(claude/anthropic) | 未显式声明 `upstream` 的 routing 条目的默认网关:`""`/`"claude"`/`"anthropic"` → Anthropic 网关,`"openai"` → OpenAI 网关 |
| `upstream.header_timeout_seconds` | `120` | 每次尝试等待上游响应头的最长时间(秒)。超时按瞬态错误自动重试,重试耗尽后向客户端报 502(不会无限等) |
| `upstream.body_idle_seconds` | `90` | 读取上游响应体允许的最长静默时间(秒)。流式上游中途停住时向客户端发 SSE `error` 事件终止,避免 Claude Code 永久等待 |
| `upstream.max_retries` | `2` | 瞬态网络错误(超时/连接重置/EOF)或 429/5xx 时的额外重试次数(总尝试 = `max_retries` + 1,退避 0.5s/1s/2s) |
| `keys.sophnet` | — | 上游密钥（可用 `SOPHNET_API_KEY` 覆盖） |
| `admin.token` | `""`（管理页面关闭） | Web 管理页面口令，HTTP Basic 用（可用 `LLM_PROXY_ADMIN_TOKEN` 覆盖）。留空 = `/admin*` 全部 404 |
| `routing.sonnet` | `DeepSeek-V4-Pro` | `sonnet` 映射目标(字符串 = 默认网关,或表值选网关) |
| `routing.opus` | `GLM-5.2` | `opus` 映射目标 |
| `routing.haiku` | 沿用 `sonnet` | `haiku` 映射目标 |
| `routing.<别名>` | — | 任意别名(含 sonnet/opus/haiku):字符串走默认网关,表值 `{ model = "...", upstream = "anthropic"\|"openai", supports_image = true\|false }` 显式选网关与 image 能力 |

> **OpenAI 网关路由示例**：`flash = { model = "glm-5.3-flash", upstream = "openai" }`
> 之后 Claude Code 以模型名 `flash` 发请求即可，代理把请求翻译为 OpenAI 格式转发到 `upstream.openai_url`，并把回复（含流式）翻译回 Anthropic 格式。请求翻译会剥离 thinking 块、把图片转 `image_url`、`tool_use/tool_result` 转 `tool_calls`/`role=tool`；响应侧 `finish_reason→stop_reason`、`usage` 映射，流式 SSE 输出标准 Anthropic 事件序列。
>
> **image 能力声明示例**：`vision = { model = "gpt-4o", supports_image = true }`（anthropic 网关直发图片）或 `vision = { model = "glm-5.3-flash", upstream = "openai", supports_image = true }`（openai 网关图片翻译为 `image_url`）。这类路由带图请求不再进 VLM 描述、不再做 image 400 重试。

## 测试

```bash
go test ./...
```

覆盖：文本/图像/`image_url` 路由、图像经 VLM 描述后插入文本并路由到文本模型、VLM 描述请求携带带图消息的上下文（角色/同消息文本）、`tool_result` 内图片带出工具名与入参、嵌套 `tool_result` 图片替换、多图逐一描述、同图不同上下文不共用缓存描述、VLM 描述失败回退到 VLM、描述缓存（同图同上下文跨请求命中、异图不混淆、超限淘汰、不可缓存 URL）、haiku 显式路由与缺省回退、非流式 JSON 原样透传、SSE 安全网补帧与去重、stripThinking 剥离时禁用 thinking 参数、损坏 thinking 块规范化（缺失的 `thinking` 字段补空串且不改动其余块）、OpenAI 网关路由（`[routing]` 表值解析、Anthropic→OpenAI 请求翻译的纯文本/图片/工具调用/thinking 剥离、OpenAI→Anthropic 非流式回复与错误透传、流式 SSE 文本与工具调用事件序列、`openai_url` 全端点去重）、image 能力声明（表值 `supports_image` 解析、带图请求跳过 VLM 直发目标/翻译为 `image_url`、image 400 透传不重试）、默认网关（`default_upstream = "openai"` 回填所有未显式声明 upstream 的条目且请求实际走 OpenAI 网关、显式 `upstream = "anthropic"` 不被覆盖、缺省保持 claude/anthropic 网关）、超时与重试（上游 header 超时自动重试成功后客户端拿到正常回复、重试耗尽返回 502 + Anthropic error JSON envelope、流式上游中途停滞发 SSE `error` 事件终止、`/v1/chat/completions` 透传对 503 重试、超时配置默认值、`isRetryableError`/`isRetryableStatus` 分类）、上游 DNS 韧性（解析结果缓存复用与 TTL 过期后重新解析、解析故障时回退到最后可用地址并打 `[DNS]` 告警、故障期间按间隔退避重探、无缓存时解析错误上报且被判为可重试、地址字面量不经过解析器、解析失败消耗完整重试预算、自定义拨号钩子下 HTTP/2 不降级）、环境变量覆盖配置路径与密钥。

Web 管理页面部分覆盖：用量提取（Anthropic/OpenAI 的非流式与流式 `usage`，缺失 usage 时不伪造精确值，尾部缓冲与有界缓冲的边界）、统计采集（请求/失败计数、失败率、别名分布、延迟均值与峰值、TPM/RPM 的 60 秒窗口边界、30 分钟分桶、模型数上限溢出、`warn` 不计请求数、每条请求只发布一次、reset）、保留标签的内存边界（模型桶键/别名表键/失败与 `warn` 明细的 `alias` 都不得指向客户端原字符串，用 `unsafe.StringData` 断言而非只看长度；覆盖首次插入与"已有别名被重复计数"两条路径；32 个 1MiB 模型名、32 个被重复计数的别名在 GC 后的保留堆内存）、请求路径埋点（Anthropic 非流式与流式、上游 5xx 与不可达、空响应、OpenAI 网关路由、`/v1/chat/completions` 透传、热重载后路由跟随变化）、管理端（未配置口令时 404、认证失败 401、环境变量口令优先、配置读接口密钥脱敏、保存落盘 + 备份 + 热重载生效、非法输入不落盘、密钥 keep/set/clear 三态、保存保留管理口令、口令三态、端口变更与未声明别名与未知顶层字段的告警、环境变量口令生效时 clear 告警不误称页面已关闭、保存失败提示不承诺配置未改动、重载失败回滚、生成 TOML 的往返解析）。

## 日志

```bash
sudo journalctl -u llm-proxy -f
```

## 已知限制

- 上游模型（如 DeepSeek-V4-Pro）实际上下文上限远低于 Claude 的 `[1m]` 上下文窗口；`[1m]` 后缀只影响 Claude Code 的上下文管理，不改变上游限制
- `/v1/chat/completions` 为原样透传，不做模型路由（如需请自行扩展）
- 管理页面的调用统计是进程内内存态，重启清零；只保留最近 30 分钟的分钟级趋势，失败明细每个模型最多 50 条
- 统计里的模型名与别名来自客户端请求，页面上按 128 字节截断显示；模型数封顶 128、每个模型的别名数封顶 64，超出归入 `(other)`
- 管理页面保存配置会重写整个文件，原文件里手写的注释与注释掉的备选路由不会保留（每次保存前有 `.bak.<时间戳>` 备份）

## 许可证

[MIT](./LICENSE)
