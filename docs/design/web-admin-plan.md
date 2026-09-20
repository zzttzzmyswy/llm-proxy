# Web 管理页面实施计划

配套设计见 [`web-admin.md`](./web-admin.md)。按序执行，每步以测试为准入。

## 步骤

1. **配置层可重载化**（`main.go`）
   - 加 `const version`，启动日志带版本。
   - 加 `cfgMu sync.RWMutex` 与 `currentConfig()` / `currentRoutes()` 访问器。
   - `loadConfig()` 改为「解析出局部 `Config` + 局部路由表 → 加锁整体替换」。
   - 请求路径的 `cfg.X` 直读改为入口取一次快照。
   - 验证：既有四个测试文件全绿（它们直接读写 `cfg` / `routeTargets`，必须保持可编译、可通过）。

2. **用量提取**（新增 `usage.go` + `usage_test.go`）
   - `extractAnthropicUsage(body []byte, isSSE bool) tokenUsage`
   - `extractOpenAIUsage(body []byte, isSSE bool) tokenUsage`
   - `tailBuffer`：只保留末尾 N 字节的有界写入器。
   - `translateOpenAIStream` 返回值加 `tokenUsage`（函数调用作为语句时忽略返回值，既有测试调用点无需改动）。

3. **统计采集器**（新增 `stats.go` + `stats_test.go`）
   - 可注入时钟；秒级环形桶 1800 槽；模型数上限 128 溢出到 `(other)`。
   - `beginReq(alias, model, upstream) *reqStat`，`success(in, out)` / `failure(category, status, msg)` 幂等。
   - `snapshot()` 产出设计文档里的 JSON 结构。

4. **请求路径埋点**（`main.go`）
   - `handleMessages`：Anthropic 分支从 `buf` / 透传缓冲取 usage；OpenAI 分支从 `respBody` 或尾部缓冲取 usage。
   - `handleOpenAIRequest`、`handleChatCompletions` 同样埋点。
   - 失败分类按设计文档的表落地；`respondUpstreamError` 的调用点补分类。

5. **管理端**（新增 `admin.go` + `admin_test.go`）
   - Basic 认证中间件（fail-closed）、配置读（脱敏）、配置写（校验 → 备份 → 原子写 → 热重载）、统计读、统计清零。
   - 先写测试：未启用 404、认证失败 401、往返一致、校验失败不落盘、备份生成、热重载生效、密钥三态。

6. **UI**（新增 `admin.html` + `go:embed`）
   - 顶部：版本、运行时长、自动刷新开关。
   - 模型调用情况：每模型一行/卡片（请求、TPM、RPM、失败率、平均/最大延迟、累计 token）+ 30 分钟 TPM 迷你柱图。
   - 失败情况：分类计数 + 最近失败表。
   - 配置：表单 + 路由表增删改 + 保存；保存后展示 warnings（如端口变更需重启）。

7. **端到端验证**
   - `go test ./...` 全绿（含既有回归）。
   - 用临时配置起实例（端口避开正在运行的 8088），curl 走认证 + 配置保存 + 统计查询。
   - 浏览器打开页面确认渲染与交互正常。
   - 确认**不影响**正在运行的 `llm-proxy.service`。

8. **交付**
   - README + `config.example.toml` 补 `[admin]` 与页面说明。
   - bump 到 `v0.9.0`，构建 amd64/arm64 + `SHA256SUMS`，发 GitHub release（不覆盖既有资产）。
   - 推分支、开 PR，issue 状态置 `in_review`。

## 风险与对策

| 风险 | 对策 |
|------|------|
| 热重载改动波及全部请求路径，可能引入回归 | 先做第 1 步并跑全量既有测试，再往下做 |
| 配置重写丢失用户手写注释 | 时间戳备份 + 生成带注释模板；文档写明取舍 |
| 统计在请求路径上，可能拖慢转发 | 采集器只做整数加法与定长环形数组写，无分配、无 I/O；快照序列化只在管理端请求时发生 |
| 长流式响应缓冲导致内存增长 | 除既有 Anthropic SSE 缓冲外，一律用尾部缓冲（64KB） |
| 误改线上 8088 服务 | 全程在 worktree 内开发；端到端验证用独立端口与临时配置；不重启线上服务 |
