# 官方 Issues / Pull Requests 审查记录（2026-08-28）

本记录对应当前专用审查分支 `codex/upstream-issue-pr-sync-20260828`，其历史同步
基线为 `codex/upstream-issue-pr-sync-20260820`。审查基线为本地 Plus `v0.2.7`
（`9b6ab7d1fe`），官方远端同步到 `upstream/main`
（`7b693ae4295e20329f18ff451b29a38879cb4705`）。`main` 和 `dev` 在本轮没有
签出、修改或强推；所有修补均在专用分支完成，构建缓存放在 F 盘临时目录。

## 已纳入的官方更新与本地适配

| 来源 | 处理 | 说明 |
| --- | --- | --- |
| [#6319 / #6320](https://github.com/Wei-Shaw/sub2api/issues/6319) | 已适配 | 多实例本地账号阻断按持久化行版本和 generation/deadline CAS 清理；数据库写入未被观察到时保持 fail-closed。 |
| [#6327 / #6328 / #6339](https://github.com/Wei-Shaw/sub2api/issues/6327) | 已适配 | 批量编辑显式提交 `codex_fingerprint_mode=off`，不会再产生空 `extra`；四档指纹模式保持 `off/device/session/full`。 |
| [#6326](https://github.com/Wei-Shaw/sub2api/pull/6326) | 已适配 | HTTP/2 响应体读取失败计入真实失败，避免 2xx 头部掩盖流中断。 |
| [#6306](https://github.com/Wei-Shaw/sub2api/pull/6306) | 已适配 | `/v1/responses` 透传在首个语义输出前发送受控 SSE 保活。 |
| [#6332](https://github.com/Wei-Shaw/sub2api/pull/6332) | 已适配 | 配额 singleflight 执行体内再次检查缓存，消除并发重复上游查询。 |
| [#6280](https://github.com/Wei-Shaw/sub2api/pull/6280) | 已适配 | 更新版本比较先剥离 `-suffix`，开发版和正式版比较稳定。 |
| [#6179](https://github.com/Wei-Shaw/sub2api/pull/6179) | 已适配 | 上游错误事件保存请求当时的代理快照；配置代理缺失/错配显示 `unknown`，不伪装成直连。 |
| [#6330](https://github.com/Wei-Shaw/sub2api/pull/6330) | 已适配并加固 | 智谱团队 GLM Coding Plan 固定走国内 quota 端点 `?type=2`，组织/项目 ID 成对校验，个人模式清理团队头；支持 `data.limits`、数组和 `CREDIT_LIMIT` 响应。 |
| [#6254](https://github.com/Wei-Shaw/sub2api/pull/6254) | 已适配并加固 | OpenAI OAuth 生图工具不可用冷却可配置 1–120 分钟，默认 30 分钟；设置读取故障使用默认值并限制挂起读取数量。 |
| [#6322](https://github.com/Wei-Shaw/sub2api/pull/6322) | 最小适配 | 仅移植 GPT‑5.6 `temperature/top_p` 能力判断、最终映射模型重算和明确 400 单字段有界重试；未引入其跨 Claude/Gemini 的账号温度三态大改动。 |

## 明确保留/跳过的范围

- Claude、Claude Code、Anthropic 相关行为按 Plus 既有契约保留。本轮没有采纳
  [#6343](https://github.com/Wei-Shaw/sub2api/pull/6343)、[#6344](https://github.com/Wei-Shaw/sub2api/issues/6344)
  等会改变 Claude 工具参数或归因的改动。
- Grok 专属图像、MiniMax、OTLP 日志、支付返利等 PR 与当前 OpenAI/Codex
  兼容目标无直接关系，暂不混入本分支，避免扩大回归面。
- 官方 PR 的迁移编号与 Plus 已发布的 231 冲突；保留已发布的
  `231_repair_active_codex_fingerprint_seed.sql`，官方新增迁移顺延为 232/233，
  并以迁移序列回归测试锁定该兼容约束。

## 本地额外修补

1. `AccountUsageCell` 主动 OpenAI 积分/用量查询后，父表行更新不会立即触发第二次
   重复 `/usage` 请求；watcher 抑制窗口有明确生命周期。
2. 本地运行时账号阻断在无 `UpdatedAt` 的旧快照中默认保持，只有显式清除、更新行版本，
   或“阈值阻断 + 新鲜可用积分”才释放，避免上游错误被误放行。
3. WS 透传测试改为断言客户端语义字段并允许服务端观测元数据，避免把合法的请求开始
   时间戳误报成协议回归。
4. 重新跑前端生产依赖审计：当前仅保留有期限的 `xlsx` 两条已知 High 例外；删除了
   已过期且已不再出现在当前生产依赖报告中的 lodash/axios 例外条目。

## 验证清单

- Go：`go test -tags=unit -p 1 -count=1 ./...` 全包通过。
- Go 静态检查：`go vet -tags=unit ./internal/pkg/apicompat ./internal/service ./internal/handler/admin ./internal/repository ./migrations` 通过。
- 前端：257 个测试文件、1863 个用例通过；`vue-tsc --noEmit`、`pnpm run lint:check`、`pnpm run build` 通过。
- 嵌入式后端：`go build -tags=embed ./cmd/server` 通过。
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
