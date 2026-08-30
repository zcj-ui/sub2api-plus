# 官方 Issues / Pull Requests 审查记录（2026-08-30 持续审查）

本记录对应当前专用审查分支 `codex/upstream-issue-pr-sync-20260828`，其历史同步
基线为 `codex/upstream-issue-pr-sync-20260820`。本轮审查基线为本地 Plus `v0.2.8`
（`7c9695c1fb`），并于 2026-08-30 重新抓取官方 `upstream/main`
（`b5827cfd54d58c248a9480b800444d0b40f0c6ea`）。`main` 和 `dev` 在本轮没有
签出、修改或强推；所有修补均在专用分支完成，构建缓存放在 F 盘临时目录。

## 已纳入的官方更新与本地适配

| 来源 | 处理 | 说明 |
| --- | --- | --- |
| [#6319 / #6320](https://github.com/Wei-Shaw/sub2api/issues/6319) | 已适配 | 多实例本地账号阻断按持久化行版本和 generation/deadline CAS 清理；数据库写入未被观察到时保持 fail-closed。 |
| [#6327 / #6328 / #6339](https://github.com/Wei-Shaw/sub2api/issues/6327) | 已适配 | 批量编辑显式提交 `codex_fingerprint_mode=off`，不会再产生空 `extra`；四档指纹模式保持 `off/device/session/full`。 |
| [#6326](https://github.com/Wei-Shaw/sub2api/pull/6326) | 已适配 | HTTP/2 响应体读取失败计入真实失败，避免 2xx 头部掩盖流中断。 |
| [#6306](https://github.com/Wei-Shaw/sub2api/pull/6306) | 已适配 | `/v1/responses` 透传在首个语义输出前发送受控 SSE 保活。 |
| [#6332](https://github.com/Wei-Shaw/sub2api/pull/6332) | 已适配并加固 | 配额 singleflight 执行体内重查缓存、缓存上限/过期清理和 epoch 防竞态写回，监控并发不会重复打配额端点。 |
| [#6280](https://github.com/Wei-Shaw/sub2api/pull/6280) | 已适配 | 更新版本比较先剥离 `-suffix`，开发版和正式版比较稳定。 |
| [#6179](https://github.com/Wei-Shaw/sub2api/pull/6179) | 已适配 | 上游错误事件保存请求当时的代理快照；配置代理缺失/错配显示 `unknown`，不伪装成直连。 |
| [#6330](https://github.com/Wei-Shaw/sub2api/pull/6330) | 已适配并加固 | 智谱团队 GLM Coding Plan 固定走国内 quota 端点 `?type=2`，组织/项目 ID 成对校验，个人模式清理团队头；支持 `data.limits`、数组和 `CREDIT_LIMIT` 响应。 |
| [#6254](https://github.com/Wei-Shaw/sub2api/pull/6254) | 已适配并加固 | OpenAI OAuth 生图工具不可用冷却可配置 1–120 分钟，默认 30 分钟；设置读取故障使用默认值并限制挂起读取数量。 |
| [#6322](https://github.com/Wei-Shaw/sub2api/pull/6322) | 最小适配 | 仅移植 GPT‑5.6 `temperature/top_p` 能力判断、最终映射模型重算和明确 400 单字段有界重试；未引入其跨 Claude/Gemini 的账号温度三态大改动。 |
| [#6316](https://github.com/Wei-Shaw/sub2api/pull/6316) | 已适配 | OpenAI Messages 入口在没有 OpenAI 显式会话信号时采纳 `X-Claude-Code-Session-Id` 作为本地粘性路由键；不写入 `prompt_cache_key`，不改变 Claude/CC 上游语义。 |
| [#5320](https://github.com/Wei-Shaw/sub2api/pull/5320) | 已适配并加固 | Codex 额度达到自动暂停阈值时绕过普通快照节流，按账号串行同步落库，成功后通知自动重置 worker。 |
| [#5321](https://github.com/Wei-Shaw/sub2api/pull/5321) | 已适配并加固 | 使用 `codex_usage_observed_at_unix_nano` 做 PostgreSQL 数值 CAS；旧的延迟 `/wham/usage` 或响应头快照不会覆盖更新快照，无关 extra 仍独立合并。 |
| [#2258](https://github.com/Wei-Shaw/sub2api/issues/2258) / [#5007](https://github.com/Wei-Shaw/sub2api/pull/5007) | 已适配 | 两个 Codex 窗口均未耗尽时不再把 5h/7d 长 reset 当成账号冷却；突发 429 走有界回退，明确耗尽窗口仍保留真实重置时间。 |
| [#3171](https://github.com/Wei-Shaw/sub2api/issues/3171) / [#3172](https://github.com/Wei-Shaw/sub2api/pull/3172) | 已适配 | WS v2 passthrough 首个下游业务帧成功后可启用 Ping/Pong（默认 20s/5s，设 0 关闭），失败走优雅断开并排空上游；不支持无 reader 探活的空闲 ctx_pool 连接超过 90s 淘汰。 |
| [#4895](https://github.com/Wei-Shaw/sub2api/pull/4895) | 已适配 | passthrough 首轮在尚未向下游输出时遇到上游 EOF/传输错误最多安全重拨一次；重试耗尽先发送结构化 `response.failed`，已有语义输出或超时不重放。 |
| [#2159](https://github.com/Wei-Shaw/sub2api/issues/2159) | 已适配 | OpenAI 账号明确绑定的代理在关系缺失、状态非 active 或已过期时 fail-closed；代理变更会刷新绑定账号的调度快照，Claude/CC 的通用代理语义保持原样。 |
| [#6169](https://github.com/Wei-Shaw/sub2api/issues/6169) | 已适配并加固 | OpenAI 图片下载 URL 做 HTTP(S)、私网/元数据地址、混合 DNS、重定向逐跳校验；请求使用克隆客户端，避免并发修改共享 transport。 |
| [#5862](https://github.com/Wei-Shaw/sub2api/issues/5862) | 已适配 | OpenAI/Codex 独立调度器现在读取分组 `model_routing`；先尝试匹配的账号池，旧 sticky 在路由外会清理并回写，路由池全部不可用时才回退普通候选。WS 后续回合沿用同一规则。 |
| [#6035](https://github.com/Wei-Shaw/sub2api/pull/6035) / [#1886](https://github.com/Wei-Shaw/sub2api/issues/1886) | 已适配 | 专用 `/v1/images/*` 请求标记独立额度域；文本 5h/7d 自动暂停不再摘除仍有生图额度的 OpenAI OAuth 账号，Responses 内嵌生图仍按文本窗口门控。 |
| [#6138](https://github.com/Wei-Shaw/sub2api/pull/6138) | 已适配并加固 | `max_sessions` 在 OpenAI API-key 调度路径原子注册并跨候选重试；缓存故障保持既有 fail-open，OAuth/Claude 请求格式不变。 |
| [#4065](https://github.com/Wei-Shaw/sub2api/pull/4065) | 已适配 | OpenAI API-key `/v1/models` 同步遇 TLS EOF/连接重置时仅对该平台使用 Chrome 请求器有限重试；代理 URL 仍按账户配置解析，其他平台不触发。 |
| [#6354](https://github.com/Wei-Shaw/sub2api/issues/6354) | 已适配 | SMTP 测试接口区分省略与显式 `smtp_use_tls`，省略时继承已保存配置，避免 465 隐式 TLS 被误以明文探测。 |
| [#5840](https://github.com/Wei-Shaw/sub2api/issues/5840) / [#6104](https://github.com/Wei-Shaw/sub2api/issues/6104) | 已适配并加固 | WS ctx_pool 容量降载在首个语义输出前转为请求级 failover；普通老连接在 55 分钟退休并安全换连，已确认 429 guard socket 保留。 |
| [#6358](https://github.com/Wei-Shaw/sub2api/pull/6358) | 已适配并加固 | Spark 429 按 `gpt-5.3-codex-spark` 模型级冷却；普通 OpenAI OAuth 仍保持两次明确 429 才进入账号冷却，WS 无状态码错误不会误记全局窗口。 |
| [#6370](https://github.com/Wei-Shaw/sub2api/issues/6370) / [#6372](https://github.com/Wei-Shaw/sub2api/pull/6372) | 已适配并加固 | 关闭“429 默认回避”时不再创建瞬时 OAuth 运行时冷却；只有明确耗尽窗口/响应体 reset 仍尊重上游重置时间。 |
| [b5827cfd](https://github.com/Wei-Shaw/sub2api/commit/b5827cfd54d58c248a9480b800444d0b40f0c6ea) | 已适配 | DeepSeek V4 价卡同步官方峰谷费率、未知 `deepseek-*` 按 Flash 兜底，工作日 UTC 高峰 2x；分组/渠道自定义价格不被覆盖。 |
| [#6364](https://github.com/Wei-Shaw/sub2api/pull/6364) | 已适配 | 生成 Codex 配置和 CC Switch 导入默认使用 `gpt-5.6-sol`，支持时默认 reasoning effort 为 `medium`；原有模型仍可手动选择。 |
| [#5469](https://github.com/Wei-Shaw/sub2api/pull/5469) | 已适配并加固 | WS v2 passthrough 超大首帧仅在合法、无续传的首个 `response.create` 上切到 HTTP bridge；重复关键字段、畸形 JSON、续传帧保持 WS，避免大帧被 relay 直接拒绝。 |
| [#6062](https://github.com/Wei-Shaw/sub2api/pull/6062) | 已适配并加固 | Responses 重试遇到 `reasoning.content` 数组长度拒绝时，一次清除所有 reasoning 项的 content，保留 encrypted_content、消息和工具调用，避免长会话耗尽重试预算。 |
| [#5866](https://github.com/Wei-Shaw/sub2api/pull/5866) | 已适配并加固 | Channel Monitor v2 非管理员查询服务端交集 API Key 可用组；无可用组直接返回空结果，不因空筛选条件扩大到全量分组。 |
| [#6367](https://github.com/Wei-Shaw/sub2api/pull/6367) | 已适配 | 管理端用量卡片隐藏的 cache tooltip 使用 `display:none`，避免移动端透明固定宽度元素制造横向滚动。 |
| [#6378](https://github.com/Wei-Shaw/sub2api/pull/6378) | 已适配并加固 | Codex OAuth/SetupToken 将最终出站 Fast tier 与私有端点回显的 `default` 分离；公开 API Key 仍按权威回显降档，避免 OAuth Fast 被误按普通价计费。 |
| [#6377](https://github.com/Wei-Shaw/sub2api/issues/6377) | 已适配 | 账户批量操作明确区分“按勾选编辑”和“按筛选条件更新”；筛选范围显示命中数量并要求管理员二次确认，避免误改未勾选账号。 |
| [#6245](https://github.com/Wei-Shaw/sub2api/pull/6245) | 已适配 | 智谱仅返回 `CREDIT_LIMIT` 时同样按 `unit=3/6` 识别 5h/weekly；同时返回 `TOKENS_LIMIT` 时信用额度不污染窗口槽位，缺少 unit 才使用 reset 时间兜底。 |

## 明确保留/跳过的范围

- Claude、Claude Code、Anthropic 相关行为按 Plus 既有契约保留。本轮没有采纳
  [#6343](https://github.com/Wei-Shaw/sub2api/pull/6343)、[#6344](https://github.com/Wei-Shaw/sub2api/issues/6344)
  等会改变 Claude 工具参数或归因的改动。
- Grok 专属图像、MiniMax、OTLP 日志、支付返利等 PR 与当前 OpenAI/Codex
  兼容目标无直接关系，暂不混入本分支，避免扩大回归面。
- 官方 PR 的迁移编号与 Plus 已发布的 231 冲突；保留已发布的
  `231_repair_active_codex_fingerprint_seed.sql`，官方新增迁移顺延为 232/233，
  并以迁移序列回归测试锁定该兼容约束。
- [#6283](https://github.com/Wei-Shaw/sub2api/pull/6283) 原生 compaction 用量语义标记需要
  `usage_logs` 新列、查询缓存、趋势/分组统计和前端筛选的整链路迁移（约 44 个文件）；
  当前分支保留请求级 `openai_native_compaction_v2` 信号，持久化报表改动放到单独批次。
- [#6269](https://github.com/Wei-Shaw/sub2api/pull/6269) Chrome uTLS 是另一套独立 HTTP/2
  transport 和设置开关，与现有 Codex reqwest/rustls profile 有重叠；在没有针对
  HTTP/HTTPS CONNECT、SOCKS5、失效代理和 HTTP/2 fallback 的端到端测试前不叠加。
- [#6290](https://github.com/Wei-Shaw/sub2api/pull/6290)、[#6199](https://github.com/Wei-Shaw/sub2api/pull/6199)、
  [#6244](https://github.com/Wei-Shaw/sub2api/pull/6244) 涉及重置卡调度、关联账号复制或
  HA 调度 schema，改动面大且包含架构选择，保留为后续专项。
- [#6353](https://github.com/Wei-Shaw/sub2api/pull/6353) 长上下文阶梯计价会重写计费目录和迁移链；
  本轮先保持现有 Plus 长上下文策略，避免把 DeepSeek 价卡同步与全量账单重构混在一起。
- [#6363](https://github.com/Wei-Shaw/sub2api/pull/6363) 提议在所有 SSE 请求收到上游响应头前发送
  request-scoped keepalive；当前 Plus 已覆盖 Responses passthrough/compact 和首语义输出后的
  流式保活，普通 HTTP Responses 的“首响应头前”场景留作下一轮专项，避免复用 writer 时引入竞态。
- [#5889](https://github.com/Wei-Shaw/sub2api/pull/5889) 的 strict raw Responses lane 涉及
  API Key 原始字节透传、认证模式、continuation 绑定和全套 UI/文档；当前已有兼容/透传模式，
  先保留为独立专项，待补齐反代认证、响应 ID 归属和取消路径端到端测试后再合入。

## 本地额外修补

1. `AccountUsageCell` 主动 OpenAI 积分/用量查询后，父表行更新不会立即触发第二次
   重复 `/usage` 请求；watcher 抑制窗口有明确生命周期。
2. 本地运行时账号阻断在无 `UpdatedAt` 的旧快照中默认保持，只有显式清除、更新行版本，
   或“阈值阻断 + 新鲜可用积分”才释放，避免上游错误被误放行。
3. WS 透传测试改为断言客户端语义字段并允许服务端观测元数据，避免把合法的请求开始
   时间戳误报成协议回归。
4. 重新跑前端生产依赖审计：当前仅保留有期限的 `xlsx` 两条已知 High 例外；删除了
   已过期且已不再出现在当前生产依赖报告中的 lodash/axios 例外条目。
5. 输入/输出边界加固：压缩请求体直接流式解压并对解压结果做 `n+1` 限制；
   `/responses/input_tokens` 上游响应统一使用配置化有界读取；UseKeyModal 的 Unix/CMD/
   PowerShell/TOML 模板对管理员可控 URL 和 key 做转义；iframe 私网判断补齐十六进制
   IPv4-mapped IPv6、IPv6 ULA/link-local/multicast 与 6to4 私网嵌入。
6. OpenAI WS 同一回合的速率信号去重：状态码明确为 429 的 `response.failed` 只推进一次
   两击计数；无状态码的 `usage_limit_reached` 仅触发请求级处理，不伪造账号级确认；
   额度快照写入后重新读取持久化行，避免旧响应清掉新阈值暂停。
7. 批量测活/盘点按批保留已完成结果，后续批次请求失败时在界面显示部分完成计数；账户计划徽章
   补齐 Team/Business/Enterprise/Edu/K12 等档位，积分余额兼容字符串和数字形态。
8. 公开交接文档已移除真实主机、远程资产 ID、备份目录和工具链私密路径，改用占位符并标注受控运维记录。
9. 本轮继续补齐官方最新 OpenAI/Spark 与计费修复：禁用 429 回避时清理瞬时 OAuth
   cooldown、Spark 429 模型级隔离、DeepSeek 官方峰谷价格与未来型号 Flash 兜底；同时
   增加 1MiB 配额响应上限、响应 ID 稳定化、SSE 多行帧严格收尾和 HTTP/2 fallback 状态清理。
10. 额度卡片将 live `/wham/usage` 的 `credits:null` 与 `spend_control:null` 视为权威空值，
    不再被旧缓存复活；账户类型筛选补齐 `service_account` 与 `upstream`。WS 超大首帧桥接与
    Spark 握手 429 模型透传均有回归用例。
11. 长 Responses/Codex 会话的 reasoning 内容拒绝改为单轮全量清理；Channel Monitor v2
    强制应用用户可用组范围；移动端用量 tooltip 脱离布局。三项均补充回归测试。
12. OpenAI/Codex service tier 计费按账户协议解析：OAuth/SetupToken 的私有 `default` 回显
    不覆盖最终 Fast 请求，API Key 仍接受公开 API 的明确降档；出站与观测值分别留存。
13. 批量账户更新的筛选范围增加显式警告、范围确认和独立按钮文案；新增的 i18n mock
    保留 `vue-i18n` 完整导出，避免并行 Vitest 污染 `createI18n`。
14. 智谱 CREDIT_LIMIT-only 套餐的 5h/weekly 解析按显式 unit 分类，并保留旧套餐
    缺少 unit 时的 reset 时间兜底。

## 验证清单（本轮持续更新）

- Go：`go test -vet=off -tags=unit -p 1 -count=1 ./...` 全包通过，包含 Spark/429、DeepSeek
  计费、配额 singleflight、HTTP/2、Responses bridge、WS 解析和代理 fail-closed 回归。
  `go vet -tags=unit` 相关包通过。
- 前端：262 个测试文件、1938 个用例通过；`vue-tsc --noEmit`、`pnpm run lint:check`、
  `pnpm run build` 通过（仅保留一个既有 unused-arg warning）。
- 嵌入式后端：`go build -tags=embed ./cmd/server` 通过；本轮工具链为 Go `1.27.0`。
- 所有定向测试、迁移序列、代理 fail-closed、积分解析、双 429、WS 旧连接保留和
  指纹生命周期测试均在全量测试前后各复核一次。

后续若官方 `main` 继续变化，先重新抓取并重新做本表中的范围判断，再在新的
`codex/` 专用分支合并；禁止直接在 `main` 上解决冲突或覆盖 Plus 的既有代理/额度契约。

## 新增配置的使用方式

OpenAI OAuth 生图工具冷却目前提供受管理员认证保护的接口（默认值已经可用，
不配置也不会改变普通文本请求）：

```http
GET /api/v1/admin/settings/openai-images-oauth-unavailable-cooldown
PUT /api/v1/admin/settings/openai-images-oauth-unavailable-cooldown
Content-Type: application/json

{"cooldown_minutes": 30}
```

`cooldown_minutes` 允许 1–120。该设置只在上游明确报告生图工具不可用时生效，
不会把模型普通文本回复误判为账号故障，也不会作用于 Claude/CC。账号配置了
`proxy_id` 时，查询和冷却相关请求仍沿用账号代理；代理关系失效会直接报路由错误，
不会回落直连。

WS passthrough 下游保活与首轮重拨配置：

```yaml
gateway:
  openai_ws:
    passthrough_downstream_ping_interval_seconds: 20 # 0 = 关闭；允许 5–60
    passthrough_downstream_ping_timeout_seconds: 5    # 必须小于 interval
```

保活只作用于 OpenAI Responses WebSocket v2 的 passthrough 下游连接；它不会改变
Claude/CC 的连接策略。首轮上游断开重拨是固定的一次安全预算，已产生下游语义输出后不会
自动重放。OpenAI 账号若配置 `proxy_id`，额度查询、健康盘点、HTTP/WS 转发及首轮重拨
都继续使用该代理；代理被禁用或过期时返回路由错误，不偷偷切直连。
