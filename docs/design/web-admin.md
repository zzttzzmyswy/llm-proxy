# llm-proxy Web 管理页面设计

面向 MYS-1343：为 llm-proxy 增加 Web 管理页面，支持**可视化修改配置**与**查看各模型调用情况（TPM、失败率、失败情况）**。

## 目标与非目标

**目标**

1. 浏览器里直接看懂并修改配置：代理参数、上游地址与超时、上游密钥、`[routing]` 别名路由表。
2. 浏览器里看到每个模型的调用情况：请求量、TPM/RPM、失败率、失败分类、最近失败明细、延迟。
3. 保存配置后**立即生效**（进程内热重载），不需要重启 systemd 服务。

**非目标**

- 不做多用户/权限体系，不做审计日志。
- 不做跨重启的统计持久化（统计是进程内内存态，重启清零）。
- 不做 `proxy.port` 的热切换（改端口需重启进程，页面会明确提示）。
- 不改动 `/v1/messages`、`/v1/chat/completions` 的既有转发语义。

## 关键决策

### 1. 服务位置：复用代理端口，路径前缀 `/admin`

管理页面挂在既有监听端口上（`/admin`、`/admin/api/*`），不新开端口。理由：不需要额外改防火墙/反代配置，且管理端与被管理进程生命周期天然一致。

### 2. 认证：HTTP Basic，fail-closed

配置里新增 `[admin] token`（可用 `LLM_PROXY_ADMIN_TOKEN` 环境变量覆盖，避免明文落盘）。

- token 非空 → `/admin*` 走 HTTP Basic 认证（用户名任意，口令即 token，`subtle.ConstantTimeCompare` 比较）。
- token 为空 → **管理页面整体关闭**，所有 `/admin*` 返回 404。

选择 fail-closed 而不是"默认开放"：代理监听在 `*:8088`，一个匿名可用的配置编辑器等于把上游密钥和路由表开放给整个网段。Basic 认证让浏览器原生弹窗，无需登录页、会话 cookie 与服务端会话状态。

### 3. 配置写入：结构化表单 + 带注释重写 + 时间戳备份

页面提供结构化表单（不是裸 TOML 文本框），字段与 `config.example.toml` 一一对应，路由表是可增删改的表格。

保存流程：

1. 服务端校验（端口范围、URL scheme、路由别名与 model 非空、`upstream` 取值合法）。
2. 把当前配置文件备份为 `<path>.bak.<YYYYMMDD-HHMMSS>`（沿用仓库既有习惯）。
3. 生成带说明注释的 TOML 文本，先写 `<path>.tmp` 再 `rename` 原子替换，保持原文件权限。
4. 重新加载配置并热切换。

**已知取舍**：重写会丢失原文件里手写的注释与注释掉的备选路由（live 配置里就有几条注释掉的 `sonnet` 备选）。用带注释的生成模板 + 时间戳备份来补偿，备份文件可随时回滚。这是为了让"可视化修改"成立而付出的代价，写在文档里而不是藏着。

密钥字段特殊处理：接口从不回传明文，只回传 `sophnet_set`（是否已配置）与 `sophnet_from_env`（是否来自环境变量）。提交时用 `sophnet_action: "keep" | "set" | "clear"` 三态显式表达意图，避免"空字符串到底是不改还是清空"的歧义。

### 4. 热重载：RWMutex + 访问器，不原地改全局

现状：`cfg` / `routeTargets` 是裸全局，请求路径直接读；现有测试也直接读写这两个全局。

做法：保留全局变量（测试兼容），新增 `cfgMu sync.RWMutex`：

- 读：`currentConfig()` / `currentRoutes()` 在 `RLock` 下取快照，请求处理函数在入口取**一次**快照后用局部变量，锁不跨越 I/O。
- 写：`reloadConfig()` 解析出全新的 `Config` 与路由表，在 `Lock` 下整体替换。

选择整体替换而不是原地修改字段：请求路径读到的永远是自洽的一份配置，不会出现"路由表已更新、超时还是旧的"的中间态。

### 5. 统计：进程内采集器，按**上游模型名**聚合

采集器 `stats.go`，`sync.Mutex` 保护，按实际发给上游的模型名聚合（live 配置里 `sonnet→DeepSeek-Flash`、`opus→GLM-5.3`、`haiku→glm-5.3-flash`，三个模型三条记录）。每条记录包含：

- 请求数 / 成功数 / 失败数、失败率
- 输入、输出 token 累计
- 延迟：累计、最大（用于平均值与峰值）
- 别名分布（哪些 Claude 档位打到了这个模型，如 `sonnet: 12`）
- 失败分类计数 + 最近 N 条失败明细（时间、别名、分类、HTTP 状态、消息）
- **秒级环形桶**：1800 个槽（30 分钟），每槽记录 `{秒, tokens, requests, failures}`

TPM / RPM 由环形桶精确计算：TPM = 最近 60 秒槽位 token 之和，RPM = 最近 60 秒请求数之和。30 分钟迷你图 = 环形桶按分钟聚合。一个结构同时满足"精确 60 秒窗口"和"30 分钟趋势"，不需要维护两套。

桶数上限：跟踪的模型数封顶 128，超出归入 `(other)`，防止 `/v1/chat/completions` 透传路径上客户端随意填 `model` 导致内存无界增长。

### 6. token 用量来源：优先真实 usage，缺失时估算并标注

| 路径 | 来源 | 精度 |
|------|------|------|
| `/v1/messages` → Anthropic 网关，非流式 | 响应 JSON 的 `usage` | 精确 |
| `/v1/messages` → Anthropic 网关，流式 | SSE `message_start.usage.input_tokens` + `message_delta.usage.output_tokens` | 精确 |
| `/v1/messages` → OpenAI 网关，非流式 | 上游 JSON 的 `usage.prompt_tokens/completion_tokens` | 精确 |
| `/v1/messages` → OpenAI 网关，流式 | 上游 SSE chunk 的 `usage`（网关带则精确）；缺失时回退：输入按请求体估算、输出用翻译器累计的 `outTok` | 估算 |
| `/v1/chat/completions` 透传 | 上游 JSON / SSE 的 `usage` | 精确 |

估算的记录会在模型条目上打 `estimated` 标记，页面显示"部分为估算"，不把估算值伪装成精确值。

流式路径不做全量缓冲（既有的 Anthropic SSE 缓冲保持不变），OpenAI 流式路径用**尾部缓冲**（保留最后 64KB）取末尾的 usage chunk，内存有界。

### 7. 失败分类

| 分类 | 触发条件 |
|------|----------|
| `upstream_5xx` | 上游 5xx |
| `rate_limited` | 上游 429 |
| `upstream_4xx` | 上游其余 4xx |
| `network_timeout` | 连接/响应头超时 |
| `network_error` | 连接重置、EOF 等瞬态网络错误 |
| `empty_response` | 上游 0 字节响应 |
| `stream_stalled` | 响应体静默超 `body_idle_seconds` |
| `translate_error` | OpenAI↔Anthropic 翻译失败 |
| `vlm_describe_failed` | VLM 描述失败（请求回退到 VLM，仍算成功，但记一条事件） |

### 8. 版本与发布

新增 `const version`，启动日志与页面页脚展示。按仓库既有约定发 `v0.9.0` release，资产为 `llm-proxy-linux-amd64` / `llm-proxy-linux-arm64` / `SHA256SUMS`。

## 组件划分

| 文件 | 职责 | 依赖 |
|------|------|------|
| `stats.go` | 统计采集器：记录请求结果、按窗口聚合、产出快照 | 仅标准库 |
| `usage.go` | 从 Anthropic / OpenAI 的 JSON 与 SSE 载荷里提取 token 用量；尾部缓冲 | 仅标准库 |
| `admin.go` | `/admin*` 路由：Basic 认证、配置读写校验、统计查询 | `stats.go`、配置层 |
| `admin.html` | 单页 UI，`go:embed` 内嵌，无外部 CDN 依赖 | — |
| `main.go` | 新增配置访问器与 `reloadConfig`；在三条请求路径上埋点 | — |

`stats.go` / `usage.go` 不依赖 HTTP 层，可以独立单测。

## 接口

```
GET  /admin                      → HTML 页面（Basic 认证）
GET  /admin/api/config           → 当前配置（密钥脱敏）
POST /admin/api/config           → 保存配置 → 备份 → 原子写入 → 热重载
GET  /admin/api/stats            → 统计快照
POST /admin/api/stats/reset      → 清零统计
```

`GET /admin/api/config` 响应：

```json
{
  "config_path": "/etc/llm-proxy/config.toml",
  "admin_enabled": true,
  "proxy": {"port": 8088, "vlm_model": "MiniMax-M3", "vlm_max_tokens": 8000},
  "upstream": {
    "anthropic_url": "...", "openai_url": "...", "default_upstream": "",
    "header_timeout_seconds": 120, "body_idle_seconds": 90, "max_retries": 2
  },
  "keys": {"sophnet_set": true, "sophnet_from_env": false},
  "routing": [
    {"alias": "sonnet", "model": "DeepSeek-Flash", "upstream": "", "supports_image": true}
  ]
}
```

`POST /admin/api/config` 请求体在响应结构基础上把 `keys` 换成
`{"sophnet_action": "keep|set|clear", "sophnet": "<新密钥>"}`。
成功返回 `{"ok": true, "config": {...}, "warnings": ["..."]}`，校验失败返回 400 + `{"ok": false, "error": "..."}`，且**不落盘**。

`GET /admin/api/stats` 响应：

```json
{
  "uptime_seconds": 1234,
  "generated_at": "2026-09-20T12:00:00+08:00",
  "totals": {"requests": 40, "failures": 3, "tpm": 812, "rpm": 6},
  "models": [
    {
      "model": "DeepSeek-Flash", "upstream": "anthropic", "estimated": false,
      "aliases": {"sonnet": 40},
      "requests": 40, "successes": 37, "failures": 3, "failure_rate": 0.075,
      "input_tokens": 12000, "output_tokens": 3000, "total_tokens": 15000,
      "tpm": 812, "rpm": 6,
      "avg_latency_ms": 1840, "max_latency_ms": 9021,
      "error_counts": {"upstream_5xx": 3},
      "recent_errors": [
        {"time": "2026-09-20T11:59:01+08:00", "alias": "sonnet",
         "category": "upstream_5xx", "status": 503, "message": "upstream 503"}
      ],
      "sparkline": [0, 120, 0, 340]
    }
  ]
}
```

`sparkline` 为最近 30 分钟每分钟的 token 数，最旧在前，长度固定 30。

## 错误处理

- 认证失败 → 401 + `WWW-Authenticate: Basic realm="llm-proxy admin"`。
- 管理页面未启用 → 404（不暴露存在性）。
- 配置校验失败 → 400，响应体带可读原因，配置文件保持原样。
- 备份/写入失败 → 500，不进行热重载（内存配置与磁盘保持一致）。
- 统计查询在无任何请求时返回空 `models` 数组，而不是 null。

## 测试策略

- `usage_test.go`：Anthropic JSON/SSE、OpenAI JSON/SSE 的用量提取；缺失 usage 时返回零值而非报错；尾部缓冲只保留末尾内容。
- `stats_test.go`：请求/失败计数、失败率、别名分布、延迟平均与峰值、TPM/RPM 的 60 秒窗口边界（注入可控时钟）、30 分钟迷你图分桶、模型数上限溢出到 `(other)`、reset。
- `admin_test.go`：未配置 token 时 404、认证失败 401、认证成功 200；配置读写往返；校验失败不落盘；保存生成时间戳备份；保存后热重载生效（路由目标变化立即反映到 `routeTarget`）；密钥三态（keep/set/clear）与脱敏。
- 端到端：本地起临时实例，curl 走一遍配置保存与统计查询，浏览器渲染页面确认无 JS 报错。
- 回归：既有 `main_test.go` / `timeout_test.go` / `translation_test.go` / `leak_rewriter_test.go` 必须全绿。
