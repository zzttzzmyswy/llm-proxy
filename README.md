# llm-proxy

Anthropic API 透明代理：把 Claude Code 的请求转发到上游（如 SOPHNET），并做**模型路由**，让 Claude Code 直接使用上游各模型（文本 / 视觉）。

## 为什么需要它

Claude Code 默认只认 Anthropic 官方模型名（`sonnet` / `opus` / `haiku`）。通过一个本地 HTTP 代理，你可以在不修改 Claude Code 的前提下：

- 把 `sonnet` / `opus` / `haiku` 映射到其他模型（如 DeepSeek、GLM）
- **图片先行描述**：带图片的请求先交给视觉模型（VLM）生成图片说明，再把说明以文本形式插入原位置，最后路由到文本模型——这样文本模型也能"看懂"图片，且所有文字始终走文本 LLM
- 修复上游 SSE 流缺 `message_stop` 时 Claude Code 卡死的问题
- 暴露一个 OpenAI 兼容端点（`/v1/chat/completions`）

## 路由策略

```
文字请求 ──→ 路由到文本模型（sonnet/opus/haiku 按配置映射）
带图请求 ──→ 逐张送 VLM 描述 ──→ 用文本块替换 image 块（"这里有一个 image，其内容如下：xxx"）
             └─ 描述失败时回退：整请求原样路由到 VLM，保证图片不丢失
```

## 功能

| 特性 | 说明 |
|------|------|
| 模型路由 | `sonnet` / `opus` / `haiku` 按配置映射到上游模型名 |
| haiku 缺省 | 配置未声明 `haiku` 时自动沿用 `sonnet` 的目标 |
| 图片先描述后路由 | 带图请求先经 VLM 描述，把说明文本插入原图位置，再发给文本模型 |
| 描述失败回退 | VLM 描述调用失败时整请求路由到 VLM，图片不丢失 |
| 图像数据 URL 转换 | OpenAI 风格 `image_url` 的 `data:` URL 转成 Anthropic image 块再送 VLM |
| SSE 透传 | `io.Copy` + `flushWriter` 零解析转发流式响应 |
| message_stop 安全网 | 仅对 SSE 流补发缺失的 `message_stop`，防 Claude Code 卡死 |
| 空响应检测 | 上游 0 字节响应 → 发 `error` SSE 事件，触发 Claude Code 重试 |
| 压缩禁用 | `DisableCompression: true`，避免 gzip 破坏 SSE 缓冲 |
| 超时保护 | `ResponseHeaderTimeout: 180s`，上游不响应头时快速失败 |
| 密钥安全 | 支持 `SOPHNET_API_KEY` 环境变量，无需明文落盘 |

## 架构

```
Claude Code ──HTTP──> llm-proxy (:8088) ──HTTP──> 上游 anthropic/openai 端点
                          │
                          ├─ 文字请求 → 按模型名映射到文本目标模型
                          └─ 带图请求 → VLM 描述后插入文本，再发文本模型
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
go build -o llm-proxy .
./llm-proxy            # 默认监听 :8088
```

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
| `keys.sophnet` | — | 上游密钥（可用 `SOPHNET_API_KEY` 覆盖） |
| `routing.sonnet` | `DeepSeek-V4-Pro` | `sonnet` 映射目标 |
| `routing.opus` | `GLM-5.2` | `opus` 映射目标 |
| `routing.haiku` | 沿用 `sonnet` | `haiku` 映射目标 |

## 测试

```bash
go test ./...
```

覆盖：文本/图像/`image_url` 路由、图像经 VLM 描述后插入文本并路由到文本模型、嵌套 `tool_result` 图片替换、多图逐一描述、VLM 描述失败回退到 VLM、haiku 显式路由与缺省回退、非流式 JSON 原样透传、SSE 安全网补帧与去重、环境变量覆盖配置路径与密钥。

## 日志

```bash
sudo journalctl -u llm-proxy -f
```

## 已知限制

- 上游模型（如 DeepSeek-V4-Pro）实际上下文上限远低于 Claude 的 `[1m]` 上下文窗口；`[1m]` 后缀只影响 Claude Code 的上下文管理，不改变上游限制
- `/v1/chat/completions` 为原样透传，不做模型路由（如需请自行扩展）

## 许可证

[MIT](./LICENSE)
