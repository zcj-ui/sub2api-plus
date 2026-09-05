# Sub2API Plus 更新日志

当前版本：`0.2.12`（源码工具链 Go `1.27.0`；旧版本条目保留历史记录）

## 0.2.12 官方 0.2.1 同步与 Plus 兼容修复（2026-09-05）

本次合并官方 `Wei-Shaw/sub2api` 主线 `ab99d56e96`（官方版本 `0.2.1`），相对上一轮官方基线 `b1748c4ea9` 纳入 77 个提交、265 个文件的改动。Plus 使用独立版本号 `0.2.12`，不是把官方版本号直接覆盖到分支。

### 官方功能与修复

- OpenAI/Codex：新增 GPT-6 Astra 模型与上游能力同步；模型列表临时不可用时保留已同步能力，支持 Astra Messages 提示缓存、ultrafast 服务档位和以 none 为来源的推理强度映射。
- 路由和续聊：按渠道映射后的模型进行账号能力筛选；修复 unavailable continuation 恢复、历史任务委派续聊、自动化 heartbeat bootstrap、OpenCode 会话透传与工具发现/search 能力声明。
- WebSocket：回放历史共享不可变正文，避免多轮深拷贝积累；记录被拒绝的加密内容摘要，后续回合移除同一失效内容；补齐 error/response.failed 等路径的 cyber-policy 记录。
- 账户管理：列表默认使用 compact DTO，保留状态、并发、凭据存在标记、Extra 和分组 ID，移除重复分组对象与凭据秘密值；支持 ETag 刷新。新增 OpenAI 分组的 pinned-accounts Codex 模型清单配置。
- 图片：账户可选择把 Images 非流式结果中的 URL 下载回填为 b64_json；客户端明确要求 URL 时不回填。下载拒绝私网目的地，逐跳检查重定向，并以内容字节识别允许的图片格式。
- 计费与运营：纳入 Anthropic max 推理档倍率、定价文件内容哈希热重载、上游请求 ID 落库/自定义响应头、零值系统指标保留、支付宝待支付订单对账等更新。
- Claude/国产渠道：同步官方 CLI 版本环境覆盖及 billing 指纹与实际 User-Agent 对齐、GLM-5.3 thinking effort、Gemini 自定义模型清单、转发失败释放会话槽、负载调度渠道模型限制检查；不向 Claude/CC 引入 Plus 的卡429功能。

### Plus 合并兼容处理

- 统一旧 lite 列表与官方 compact 列表接口，避免精简请求只返回 ID/name 等元数据、导致账户状态和额度列缺失。保留 Plus 健康快照、盘点入口和指纹配置字段。
- 保留 OpenAI 已配置代理的 fail-closed 行为，并在新增图片下载回填阶段再次检查配置；代理缺失、ID 错配、禁用、过期或 URL 无效时不改为直连。未配置代理不受影响。
- 保留四档指纹 off/device/session/full（默认 off）、Credits 查询及 Credit/25 参考换算、两次明确 429 确认和既有可选长连接策略。
- 将官方 WS 不可变历史更新放在 Plus 成功回合条件内；失败回合不覆盖上一轮有效续聊快照，失效连接仍清理绑定。
- 保留 Plus 的缺省 service_tier 匹配规则，并合并 ultrafast；保留自定义长上下文计费，同时恢复官方独立 1 小时缓存写入价格及区间处理。
- 失败会话槽释放与共享调度器的 API Key 会话限制对齐。合并上游请求 ID 字段时补齐 Plus 的 Chat Completions 压缩桥接返回值。

### 发布前本地验证

- Go 1.27.0：`go test -tags=unit -p 1 ./... -count=1` 全部通过，包含 service、handler/admin、repository、middleware 和 migrations。
- 前端：`pnpm exec vitest run` 通过 268 个测试文件、2015 个用例；`pnpm build` 通过。
- 后端：`go build -tags=embed -trimpath ./cmd/server` 成功；进行了前端/迁移、WS/续聊、代理/计费三个维度的独立审查。
- 定向回归覆盖 compact 列表状态/凭据脱敏、代理失效时禁止图片下载、API Key 会话槽释放、1 小时缓存价、压缩桥接上游 ID，以及换号后不再发送已拒绝密文。

### 数据与升级说明

- 新增 4 个官方迁移（上游请求 ID 列/索引、渠道推理倍率、分组模型清单配置），既有 SQL 文件不改写。迁移以完整文件名识别，重复数字前缀不覆盖旧迁移。
- 发布包不携带数据库、账户数据、配置或构建缓存；本次 Git/Release 发布不自动操作已部署服务器。升级前仍须备份数据库、配置、代理和凭据，并验证备份可恢复。
- 新图片回填开关位于账户新建/编辑的 OpenAI 图片配置；自定义上游请求 ID 响应头位于账户配置；模型清单策略位于 OpenAI 分组配置。未开启的可选功能保持原行为。
- CI/自动化测试通过不能证明所有生产环境无缺陷。本项目继续按 LGPL-3.0-or-later、NOTICE 和仓库免责声明分发，不承诺生产可用性。

## 0.2.11 官方增量同步（2026-09-05）

- 合并官方 `3510aa22b5..b1748c4ea9`：分组模型定价界面、上游错误代理归因、归因遗漏补齐和队列硬上限。
- 保留 Plus 出站代理规则、OpenAI/Codex 自定义策略和发布配置。
- 该版本 Release 已完成，提供五个平台归档和 checksums.txt；后续条目不覆盖历史版本记录。

## 0.2.10 官方最新同步说明（2026-09-01）

本版本紧跟 `Wei-Shaw/sub2api` 官方 `upstream/main` 最新提交 `3510aa22b5`，在 `0.2.9` 的 Plus 定制基础上同步官方近期协议、模型、计费和管理端更新。同步范围覆盖官方 `v0.1.184`、`v0.1.185` 之后的主线提交，Plus 的代理强制绑定、Credits、卡 429、指纹四档和账户调度规则继续保留。

### 已同步的官方能力

- OpenAI/Codex：同步官方 reasoning effort 分组范围控制、Fast/priority 分组策略、OpenAI API Key Chat Cache 身份保持、Responses 模型找不到与限流区分、WS 缺少终态处理、自动化 bootstrap 和 delegation bootstrap 兼容。
- WebSocket：同步首帧/续帧生命周期、超大帧 HTTP bridge 边界、终态关闭处理、容量降载事件改写和会话模型切换；Plus 的两次明确 429 确认与“奸商模式”长连接粘性继续只作用于 OpenAI/Codex。
- 国产供应商：同步 Kimi 原生 Responses、DeepSeek/Kimi 自适应端点选择、Fable 5.1 模型识别、智谱/DeepSeek/Kimi 账户模式和配额处理。Kimi/DeepSeek 的原生 Responses 端点不再误走 Chat Completions 基址。
- 计费与价卡：同步 GPT-5.6、Fable 5.1、缓存写入 1 小时价、OpenAI Fast 免费/强制策略和 reasoning effort 上限；长上下文阶梯继续优先使用目录/override 的显式字段，显式零值可关闭阶梯，xAI 阈值按官方“达到即应用”口径处理。
- 管理端与前端：同步分组 Fast/Reasoning 控件、用量/支付/兑换页面、账户状态与额度组件、筛选批量编辑、模型目录和新的日期输入格式；保留 Plus 的一键盘点、测活失败池、Credits/美元参考值和代理状态展示。
- 数据库迁移：纳入官方 usage log native compaction v2、频道 1 小时缓存价、分组 Fast/Reasoning 控制和未分组访问限制迁移；现有 Plus 指纹 seed 修复迁移保持不可变并与新迁移共存。

### 同步期间的兼容修复

- 自适应国产协议路由统一使用 `UsesNativeCNResponses`：DeepSeek 与 Kimi 在有原生端点时发送 `/responses`，GLM 等无原生端点的账号继续发送 `/chat/completions`。
- OpenAI Responses 的默认 instructions 只注入 OAuth/Codex 请求，API Key 的大输入原样保留，避免第三方兼容上游收到不属于客户端的系统指令。
- Fast 强制分组在 HTTP、OAuth 透传、原生 Responses 和 WebSocket `response.create` 四条路径都先写入 `service_tier=priority`，再执行全局过滤规则。
- 认证缓存快照升级到 v22，完整保存 `force_openai_fast`、`free_openai_fast` 和 reasoning effort 超限策略；旧快照会自动回源刷新。
- WS 容量降载事件采用“先观察、后决定”：若只有一个 pre-output `error` 随即断开，返回请求级 failover；若后续有 `response.failed`，则向客户端改写为可重试的 `server_error`，保留正常长连接体验。
- Agent Identity 的 Chat 恢复重试复用同一 UUID 形态的 session_id；TTFT 默认按 semantic output 计时，可通过 `openai_ttft_mode` 选择 visible 口径。
- 定价解析支持显式 override 零阈值、缺失倍率按 1、GPT-5.5 Fast 2.5x、GPT-5.6 Fast 2x 和 Fable 专属 7d_oi 模型限流，避免少计费或把 Fable 误伤为整组 Claude 限流。

### 验证记录

- 完整后端回归：`go test -tags=unit -p 1 -count=1 ./internal/service ./internal/handler ./internal/repository ./migrations` 全部通过。
- 本地验证使用 Go `1.27.0`，service、handler、repository、migrations 均为 `ok`；`git diff --check` 干净。
- 之前 `main` 的 CI 和安全扫描均通过；本次同步提交后会重新触发同一套 shell、frontend、golangci-lint、unit、integration 和 security jobs。

> 本版本继续遵循 `LGPL-3.0-or-later`、NOTICE、免责声明和源码构建要求。官方同步不代表生产认证；升级前保留数据库、配置、代理和账户凭据备份，并先用少量账户执行测活与一键盘点。

## 0.2.9 小版本发布说明（2026-08-30）

这是在 `0.2.8` 兼容性同步基础上的小版本修订，重点收敛账户探测写入、补齐 CI 集成测试边界，并把本轮已经验证过的 OpenAI/Codex 适配能力正式固化到版本文件。版本号只提升一个补丁位，数据库结构、现有账户凭据格式和代理配置格式保持兼容。

### 上游同步与协议兼容

- 持续跟进 `Wei-Shaw/sub2api` 的 `upstream/main`，保留中国区配额/请求头、渠道分时计价、Responses/Chat Completions 桥接、Grok/智谱等渠道适配，以及前端账户管理改进。
- OpenAI Responses、Chat Completions 和 Codex WebSocket 共用一致的回合状态投影；WebSocket 后续回合遇到明确 429 时，重放请求会按新模型重新选择账户、能力、平台和渠道映射，不再沿用上一回合的错误路由。
- 保留超大首帧 HTTP bridge、`/responses/input_tokens`、sideband/live HTTP、compact 探测、工具增量事件和非标准反代错误的兼容处理；异常响应会先完成结构化识别，再决定重试或切换账户。
- 对 `previous_response_id`、终态 output `id`/`call_id`、服务层级和重置配额响应增加边界校验，避免控制字符、别名冲突或超大响应造成会话状态污染。

### OpenAI/Codex 账户、额度与代理

- Codex 指纹收敛继续提供 `off`、`device`、`session`、`full` 四档，默认 `off`；HTTP、WebSocket、导入、编辑、批量编辑、复制和调度快照使用同一账户级 seed 生命周期。
- OpenAI OAuth 账户的卡 429 策略仍为“两次明确上游 429”才进入账户级冷却；第一次 429 执行请求级 failover，短时间内第二次明确 429 才冻结。存在可用 Credit 时不会因为本地估算阈值提前返回 429。Claude/CC 渠道不启用这套策略。
- “卡429/奸商模式”开关仅作用于 OpenAI/Codex，默认关闭；开启后保留老连接的账户粘性，只有连接出现异常才切换，避免把正常长连接误判为失活。该模式的体验和容量权衡已在界面及文档中明确说明。
- 配置了 `proxy_id` 的 OpenAI 账户实行 fail-closed：模型请求、OAuth 刷新、额度、盘点、测活、WebSocket、live/sideband 和 `input_tokens` 均复用该代理；代理缺失、URL 无效或查询失败时直接报告原因，不旁路直连。未配置代理的账户继续按原有直连策略运行。
- ChatGPT 积分查询固定使用 `GET /backend-api/wham/usage`，读取 `credits.balance`；管理端美元金额仅作前端参考换算（`Credit / 25`），不会当作实际账单余额。单账户查询、一键盘点和导入后的首次刷新共用解析与缓存。
- 账户调度优先在并发承受范围内填满少量健康且有冗余的账户，需求增加时再扩展到账户；测活失败会重试一次，连续失败进入独立失败池并保留脱敏原因，避免死号继续参与正常调度。

### 本轮新增的稳定性修复

- 修复代理上游账单/用量探测在快照缺失或为 `null` 时仍写入账户时间戳和 outbox 的问题。现在只有存在有效快照时才更新对应账户记录，空探测不会制造“刚刚使用”或虚假的变更事件。
- 修复集成测试夹具把“手工暂停”默认值带成 `true` 的问题；测试现在显式设置场景状态，避免把夹具默认值误当成生产行为。
- 保留上一轮 `golangci-lint` 修复：清理未使用符号、无效赋值、nil context、错误类型断言和静态分析告警，确保 Go `1.27.0` 工具链下 lint 与 CI 规则一致。
- 账户列表、状态兜底、额度窗口、批量编辑确认、导入数据模型、测活失败池和一键盘点的前后端字段继续保持一致；未知状态不会再直接渲染为 `admin.accounts.status.undefined`。
- 更新/重启相关请求继续使用幂等键和断连独立上下文，结构化返回权限、备份、校验、替换和回滚错误；本版本不自动覆盖运行中的数据目录，也不携带构建缓存或数据库文件。

### 前端与管理端体验

- 账户详情、批量编辑、导入、重新授权、额度卡片和一键盘点页面共享 OpenAI/Codex 字段定义，指纹档位、Credit、美元参考值、重置次数、5 小时/7 天窗口和代理状态可在同一账户上下文查看。
- 盘点操作必须先选择账户；结果按成功、失败、跳过和额度已获取分类展示，失败项显示最后一次脱敏错误。普通“查询”与“一键盘点”使用相同的积分解析器，避免只有盘点才刷新额度的显示差异。
- 批量操作继续区分“勾选目标”和“筛选命中目标”，筛选全量更新显示命中数量并要求二次确认，降低误操作范围。

### 验证与交付记录

- 本地 `go test -tags=unit -p 1 -count=1 ./internal/repository` 通过；Go `1.27.0` 嵌入式后端构建通过。
- `dev` 构建、单元测试、集成测试、前端构建和安全扫描均已通过；开发产物为 `sub2api-0.2.9-dev.29.ca8ed491`。
- `main` 分支 CI 全部通过（shell、frontend、golangci-lint、unit、integration）；安全扫描通过。最近一次主分支运行记录：`33302900943`。
- 发布包仍遵循仓库 `LGPL-3.0-or-later` 协议、NOTICE、免责声明和源码构建说明；版本号变更不改变上游许可证义务。

### 升级与回滚提示

1. 升级前保留数据库、`config.yaml`、`.env`、代理定义和账户凭据的离线备份，并确认备份可恢复。
2. 从 `main` 拉取源码后按[源码构建说明](SOURCE_PACKAGE_BUILD_2026-08-14_CN.md)构建，或使用 CI 生成的对应归档；不要把 `.gocache-*`、`.tmp`、数据库和运行日志加入源码包。
3. 升级后先在管理端选择少量账户执行测活和一键盘点，确认代理出口、Credit、重置次数和状态显示，再逐步放量。
4. 出现异常时保留日志中的 request id 与结构化错误，按[发布指南](REPOSITORY_RELEASE_GUIDE_CN.md)回滚到上一版本；回滚前后都不要覆盖用户数据目录。

> 本版本是技术预览和兼容性验收版本。CI 通过表示源码、构建和自动化测试达到当前门槛，不等同于任何生产环境认证；部署前仍应完成独立审计、压测、备份恢复、代理连通性和回滚演练。

## 0.2.8 官方问题/PR 同步与 OpenAI/Codex 兼容性修复（历史条目）

- 在独立分支 `codex/upstream-issue-pr-sync-20260828` 同步官方 `upstream/main`，保留 Plus 的 OpenAI/Codex 指纹、代理强制绑定、额度积分和两次明确 429 规则；Claude/CC 逻辑不纳入本批次改动。
- 修复多实例账号运行时阻断的过期清理竞态、HTTP/2 响应体失败识别、透传首输出 SSE 保活、配额 singleflight 重查，以及批量编辑关闭 Codex 指纹时的空更新问题。
- 增加智谱团队版 GLM Coding Plan 额度查询：可选组织/项目 ID、`?type=2` 团队端点、请求头校验和账号代理强制复用；个人版会清理团队头并继续走个人端点。
- 增加 OpenAI OAuth 生图工具不可用冷却配置接口，默认 30 分钟、允许 1–120 分钟；设置读取失败时使用默认值，不阻塞请求。
- 同步官方 `#6358` Spark 429 模型级限流与 `#6370/#6372` 修复：关闭 429 默认回避时，瞬时 OAuth 429 不再建立本地 cooldown；明确耗尽窗口仍按上游 reset 时间处理，普通 OpenAI/Codex OAuth 继续两次明确 429 才冻结。
- 同步官方 `b5827cfd54` DeepSeek V4 峰谷价卡和 `#6364` Codex 默认配置：默认模型为 `gpt-5.6-sol`、支持时默认 medium reasoning；DeepSeek 工作日 UTC 高峰按 2x、未知 `deepseek-*` 以 Flash 价兜底，分组/渠道自定义售价保持不变。
- 加固 Responses/WS 兼容边界：previous_response_id 原始控制字符拒绝、终态 output 的精确 `id` 优先于 `call_id` 别名、WS 握手 429 保留配额响应头；配额和 reset-credit 响应增加 1MiB/64 项上限，代理凭据日志彻底去除 userinfo。
- 同步官方 #5469：WS v2 passthrough 超大首帧只在合法、无续传的首个 `response.create` 上使用 HTTP bridge；重复字段、续传和畸形帧继续走 WS。额度卡片对 live `credits:null` / `spend_control:null` 清除旧缓存，账户类型筛选补齐 `service_account` 与 `upstream`。
- 同步官方 #6062/#5866/#6367：长 Responses 会话遇到 reasoning 内容长度拒绝时单轮清理全部 reasoning content；Channel Monitor v2 对非管理员强制执行 API Key 可用组交集并对空权限 fail-closed；管理端 cache tooltip 隐藏时不再制造移动端横向滚动。前端全量现为 262 个测试文件、1938 个用例。
- 同步官方 #6378：OpenAI/Codex 计费分离最终出站 service tier 与上游观测 tier；OAuth/SetupToken 私有端点回显 `default` 时保留实际 Fast 档，公开 API Key 仍按明确回显降档。
- 同步官方 Issue #6377：账户批量编辑明确区分勾选目标与筛选全量目标；筛选更新显示命中数量并强制二次确认，防止误改未勾选账号。
- 同步官方 #6245：智谱仅信用额度响应按 unit 正确映射 5h/weekly，避免周窗口临界期被 reset 时间排序反置；TOKENS_LIMIT 仍优先于 CREDIT_LIMIT。
- 纳入 OpenAI/Codex GPT-5.6 采样参数兼容补丁：按最终映射模型决定是否保留 `temperature`/`top_p`，只有上游明确拒绝顶层字段时才做有界单字段重试，嵌套工具参数不被误删；compact 端点仍会剥离采样字段。
- 当前版本不会自动覆盖 `main`/`dev`，也不会携带构建缓存或数据库文件；发布前需在本分支完成后端、前端和嵌入式构建复核。本轮全仓 Go、前端 262 个文件/1938 个用例、typecheck、lint、Vite build 和嵌入式构建均已复核。

发布日期：2026-08-30（准备中）

> 发布状态：`0.2.x` 是技术预览和验收版本，不代表生产认证。请勿直接接入真实付费用户、高价值凭据或不可替代数据；部署前阅读[完整风险声明](../legal/admin-compliance.zh.md)并完成独立审计、压测、备份恢复和回滚演练。

## 0.2.7 官方同步至 `0.1.183`、指纹透传与 429 兼容

- 同步官方 `upstream/main` 至 `efb46db0a9`（官方版本 `0.1.181` / `0.1.182` / `0.1.183`）。Plus 版本号为 `0.2.7`，不采用官方 `0.1.183`。
- 纳入官方 Go 1.27、OAuth 出站 plugin、自动用卡、`service_tier`、SetupToken 协议路由、Grok 4.6 / Realtime、渠道分时计价、Responses Lite 并行工具约束与大整数精度、OpenCode Go 用量重置解析、OAuth 图片 prompt 原样转发、Codex 路由模型目录、Antigravity Sonnet 4.5/4.6 与 token clamp、Kimi K3 / 并发 403、支付完成后余额刷新、邮箱换绑别名去重。
- 完整官方 CLI 快照与 device 模式透传 session/thread/request；compact 与不完整身份继续隔离。普通 OAuth 请求不再凭空声明 `remote_compaction_v2`。
- 瞬时 429 仍两次明确确认才冻结，不采用官方“配额耗尽立刻暂停”。第一次 failover，30 秒内第二次冻结。卡429 只作用于 OpenAI/Codex，不进 Claude/CC。
- 粘性会话保留 Plus wait-on-full：队列未饱和时继续等待，不因容量溢出改写长期绑定。`session-id` 可用于调度哈希，不覆盖指纹透传。
- 非流式 SSE「Selected model is at capacity」转为请求级 failover。WebSocket 图片桥保留超大整数精度。插件安装在 Windows 上先关闭 ZIP 再提交，避免文件占用导致 rename 失败。
- `proxy_id` 仍 fail-closed；代理丢失或配置错误时明确失败，不静默直连。

验证结果：`internal/service` 全量、repository、domain、handler、创建账号前端用例均通过。`dev` / `main` CI 绿；Release 工作流的 golangci-lint 与 CI 对齐为 v2.13，以支持 Go 1.27。

## 0.2.6 官方同步、OpenAI/Codex 稳定性与更新链路修复

- 同步官方 `upstream/main` 至 `32a0d9ba2d`，纳入近期渠道监控配额、分时定价、中国供应商路由、Responses/Chat 兼容、Grok 工具协议、WebSocket 续接和前端管理界面改进。
- OpenAI/Codex WebSocket 在当前回合明确 429 时，重放会重新依据重放请求中的模型选择账户、能力、平台和渠道映射；同一连接上的模型切换不再沿用前一回合的错误调度信息。
- 修复 Codex 指纹服务端 seed 生命周期：新建和导入忽略外部 seed，编辑不得替换既有 seed，批量更新由仓储原子补齐；API Key 与 OAuth 类型切换会清理不适用的状态，避免账户身份被导入数据污染。
- 修复 HTTP/raw 请求体对畸形 `client_metadata` 的 map/raw 状态不一致；非对象值保持原样，后续有效请求仍可得到一致的 prompt-cache 与会话投影。
- 更新检查、下载、校验、归档、文件替换和回滚错误均返回结构化脱敏原因；Linux 非容器二进制继续支持面板原地更新，Docker、Windows、macOS 和源码运行会显示相应部署限制，避免表面成功但二进制未更新。
- 系统更新与回滚的幂等 claim、执行和完成写入使用断连独立且有界的上下文；浏览器或反向代理中断后，已完成的替换不再因最终幂等写入使用已取消上下文而误报 503。
- 管理端更新、回滚和重启请求补齐 `Idempotency-Key`；更新检查失败或使用缓存时会明确显示不确定状态，不再伪装为“已是最新”。

验证结果：`internal/service` 全量、handler、repository、migrations、管理端 system handler、`go vet`、嵌入式后端构建、前端全量 Vitest、类型检查和 Vite 生产构建均通过；`git diff --check` 干净。

## 0.2.5 Codex 生命周期兼容性与 CI 修复

- 恢复官方 Codex 指纹四档生命周期（`off`、`device`、`session`、`full`），统一 HTTP 与 WebSocket 行为。
- WebSocket 连接池按稳定的 session、thread、parent-thread、installation 和 API key 隔离，并忽略每回合变化的标识。
- 增加历史 device-only 迁移恢复标记，保留管理员明确编辑后的档位语义。
- 修复 `golangci-lint` 报错，补充正文投影、连接复用和错误检查回归测试。
- 已通过完整 CI、安全扫描、前端检查、后端单元/集成测试和开发构建。

## 0.2.4 发布门禁与 WebSocket 状态清理

- 修复正式 CI 在 `golangci-lint v2.9` 下发现的未使用函数、无效赋值、未检查类型断言和静态检查问题，保持 WebSocket 429 守护、会话租约和错误切换语义不变。
- 清理不再使用的状态容量兼容 helper，并为状态存储测试补齐类型断言检查，避免测试 panic 被误判为运行时成功。
- 恢复账户级 Codex 指纹四态生命周期：`off` 默认透传，`device/session/full` 仅在显式 opt-in 时生效，并在导入、编辑、批量编辑和调度快照中保持 seed 与账号归属一致。
- 修复完整官方 CLI 请求在正文规范化、HTTP/WS 重试和 `count_tokens` 路径中被误判为兼容请求的问题；探针、额度和独立搜索改用动态 canonical 身份，避免旧版本 UA 漂移。
- 重新验证 OpenAI WebSocket ingress/v2、HTTP bridge、额度/调度状态存储和安全审计路径；本地 Go 测试与同版本 golangci-lint 均通过。

验证结果：`internal/service`、`internal/securityaudit` 测试通过，`golangci-lint v2.9` 全仓 `0 issues`，`gofmt` 与 `git diff --check` 通过。`v0.2.3` 标签曾因 CI 外部 schema 超时及随后暴露的 lint 问题未生成 Release；本版本使用新标签发布，不重写历史标签。

## 0.2.3 OpenAI/Codex 正式兼容性与账户运营增强

- 完成 OpenAI/Codex WebSocket 会话级账号粘性、回合租约和断线状态回传，避免同一会话在并发或重连时漂移到不同账号，并防止旧连接释放新连接的租约。
- 强化 Responses、Chat Completions 和 WebSocket 之间的兼容桥接：补齐回合状态、压缩探测、必填时间字段、工具调用增量和失败事件，兼容反代上游的非标准错误形态。
- OpenAI OAuth 的额度、Credit 和测活请求统一沿用账户配置的代理出口；未配置代理的账户保持直连，不会被全局代理设置误改。
- ChatGPT `GET /backend-api/wham/usage` 额度解析继续读取 `credits.balance`，并将 `Credit / 25` 作为管理端美元参考值；盘点、单账户查询和批量导入共用同一解析与缓存路径。
- OpenAI/Codex 的卡 429 策略保持默认关闭；仅同一账户连续收到两次明确上游 429 才进入账户级冷却，有可用 Credit 时不因本地阈值提前冻结。Claude/CC 渠道不启用该策略。
- 完善账户调度的容量闸门、优先级和冗余选择，优先填满少量健康账户，再按需求扩展到更多账户；WebSocket 长连接在账户异常时才切换。
- 一键盘点和测活失败池支持按选择范围执行、失败重试一次、记录失败原因，并区分跳过账户与确认失活账户；导入、编辑、批量编辑和账户状态显示保持字段一致。
- 修复账户列表首屏精简响应导致的 `status.undefined`、额度窗口缺失和并发显示异常，未知状态现在有明确兜底文案。
- 纳入上游近期 OpenAI/Codex 修复及反代兼容改动，并保留数据库迁移、回滚备份和源码构建流程；不改变 Claude Code 原有请求语义。

验证结果：已完成后端 service、handler、repository、securityaudit 定向与全量测试，WebSocket/Redis 租约和两次 429 逻辑重复验证，前端 Vitest、`vue-tsc --noEmit`、ESLint、Vite 生产构建、Go 格式和 diff 检查均通过。Windows 本机仅有临时目录测试二进制执行策略限制，编译已通过并由 Linux CI 覆盖。

## 0.2.2 账户列表显示、在线更新与调度粘性修复

- 修复账户列表首次加载整表渲染损坏：此前首屏请求使用 `lite=1` 精简响应（仅 `id/name/platform/type/健康快照`），导致 `admin.accounts.status.undefined`、并发显示 `0/`、额度窗口和"从未使用"等列全部异常，需要手动刷新或修改每页数量才恢复。首屏现在始终请求完整字段，`lite` 仅保留给全选元数据这类只读 ID 的场景，并新增回归测试锁定。
- 账户状态徽标增加兜底：缺失或未知 `status` 统一显示"未知"，不再把原始 i18n 键名渲染到界面上；补齐 `disabled/expired` 历史状态文案（中英文）。
- 在线更新失败不再只显示 `internal error`：服务用户对安装目录无写权限时返回结构化 `UPDATE_DIRECTORY_NOT_WRITABLE`（HTTP 409），提示修复目录属主或权限后重试；备份文件无法删除时也会明确报错而不是被静默吞掉。
- 安装/升级脚本为安装目录补齐运行用户写权限（`chown $SERVICE_USER` + `chmod u+rwx`），手动部署文档同步说明该要求，避免后续面板在线更新踩同样的权限问题。
- 指纹模拟显示异常确认为上述 lite 根因（列表 `extra` 缺失所致）；指纹收敛的后端派生、编辑/批量编辑写入链路与文案经测试验证保持正常，默认仍为显式 opt-in（`off`）。
- 调度粘性重做，解决"同一会话被拆到多个账号导致上游 prompt cache 全丢"与"优先级来回跳"：
  - 粘性账号并发满时不再自动换号，改为在原账号排队等待（受 `StickySessionWaitTimeout/StickySessionMaxWaiting` 约束），会话不再拆号。
  - 粘性逃逸（TTFT/错误率触发跳号）改为显式 opt-in，默认关闭；TTFT 抖动是高并发常态，不再触发迁移。需要的管理员可通过 `gateway.openai_scheduler.sticky_escape_enabled` 显式开启。
  - 溢出阀：显式开启逃逸且等待队列饱和时，粘性请求释放到其他有空位的账号并保留原绑定，账号恢复后会话自动回到原号；新会话始终优先选择有立即空位的账号，不会被满号阻塞。
  - 粘性绑定空闲租约从固定 1 小时改为可配置滑动窗口，默认 60 秒，可通过设置键 `openai_sticky_session_idle_ttl_seconds` 运行时调整（如 10 秒）；每次请求命中都会续期。
- 修复三个 Codex 用量快照测试在重复运行（`-count>1`）下的偶发失败：测试构造的服务未带独立节流器，落入包级 30 秒快照写入节流窗口；现改为每个用例使用零间隔节流器，生产行为不变。

验证结果：`internal/service` 全量测试、`internal/config` 测试、`internal/handler/admin` 测试、前端 `vue-tsc --noEmit`、完整 Vitest（229 个文件）均通过；更新器/指纹/快照/粘性调度定向测试以 `-count=3` 重复运行通过；`gofmt` 与 `git diff --check` 干净。

## 0.2.1 OpenAI/Codex 兼容性修复

- 对齐上游 PR #5668：回传并按账户隔离 `x-codex-turn-state`，补齐 remote compaction v2 探测和 beta feature 恢复；Codex 指纹收敛改为显式 opt-in，缺省保持 `off`。
- API Key 和第三方 OpenAI 兼容上游恢复 custom/freeform 工具调用，流式参数增量不再输出空工具名，旧式 `tool_choice.function.name` 自动转换为 Responses 标准形态。
- 为只支持 Chat Completions 的兼容上游桥接 Codex remote compaction v2：请求侧保留压缩历史并注入总结指令，响应侧生成唯一 `compaction` item，兼容 JSON 和 SSE 客户端。
- 所有网关合成的 Responses 对象和失败事件补齐必填 `created_at`，避免严格 Rust/Codex 客户端因字段缺失而终止反序列化。
- 识别纯 message 形态的 `Our servers are currently overloaded`，覆盖 HTTP 400/503 和流式错误；空 WebSocket `error` 对象会补充可重试错误详情，不再向客户端暴露无意义的 `{}`。
- OpenCode Go 兼容上游的 `GoUsageLimitError` 会解析 `Resets in 4hr 59min` 等单段或组合时长，账户保持冷却到真实重置时间。
- OpenAI OAuth 账户的空 `openai_capabilities` 容器按未配置处理，避免导入或历史数据导致健康账户被调度器静默排除。
- 调度 Redis 快照补齐预加载前账户元数据并升级缓存命名空间，避免账户优先级、并发和扩展字段在缓存路径丢失。
- 非官方上游 URL、私有 IP 和 host:port 从下游错误文案中脱敏；官方 provider host 保留，error passthrough 路径使用同一规则。

本轮审查了官方仓近期 OpenAI/Codex、Responses、429、调度、代理和安全相关 Issues/PR。Anthropic/Claude Code 专属改动、支付/国产供应商新功能、OpenAI Team 联动熔断及大范围流式 failover 重构未纳入本版本，避免扩大回归面。

验证结果：Go 1.27.0 下 `internal/service` 全量测试、其余后端包测试、`go vet -tags=unit ./...`、middleware 测试程序单独编译、前端 `vue-tsc --noEmit`、完整 Vitest 和 Vite 生产构建均通过。Windows 本机策略会阻止执行临时目录中的 `internal/middleware` 测试程序；该包已成功编译，完整执行继续由 Linux CI 覆盖。

## 2026-08-15 仓库与发布流程更新

- 新增 `dev` 开发快照通道：多平台归档保留 14 天，GHCR 发布 `dev` 和不可变版本号标签，不覆盖 `latest`。
- 正式通道只接受 `vX.Y.Z` 标签，继续生成多平台归档、SHA256、GitHub Release 和稳定镜像。
- 正式与开发构建自动写入当前 `github.repository`；面板更新、回滚和安装脚本不再固定读取上游仓库。
- 新增 `SUB2API_UPDATE_REPO=owner/repository` 运行时覆盖，并按仓库隔离更新缓存。
- Compose 新增 `SUB2API_IMAGE`，一键部署脚本会为 Fork 自动选择对应 GHCR 镜像。
- 修复手工标签发布时前端错误地从默认分支构建的问题，并移除发布后的机器人 VERSION 回写提交。
- 修复标签说明直接插入 shell 的发布脚本注入风险。
- GoReleaser 配置升级到当前 v2 归档语法，GHCR 镜像名统一使用当前仓库完整小写名称。
- 新增 `NOTICE`，保留上游版权和 `LGPL-3.0-or-later`，归档及镜像均包含许可证与来源声明。
- 新增 `make build-dev`、`make build-release` 和仓库发布指南。

本轮验证包括 Go 更新服务/构建信息测试、前端类型检查、Pinia 26 项测试、Shell 安装测试、全部 YAML 解析、GoReleaser v2.17.1 三套配置检查，以及 Linux/Windows 开发快照真实编译。

## 适用范围

本次定制重点是 OpenAI/Codex 账户、ChatGPT 额度查询、账户调度和上游反代兼容。Claude Code 渠道没有启用“卡429”注入，原有 CC 请求语义保持不变。

## 新功能

### OpenAI/Codex 额度与积分

- 接入 ChatGPT 原始额度接口 `GET /backend-api/wham/usage`。
- 读取 `credits.balance` 作为 Credit 余额，并在管理端按 `Credit / 25` 显示美元参考值。
- 保存普通 OpenAI OAuth 账户的 5 小时、7 天用量窗口与 Credit 快照，供账户列表和调度判断使用。
- 有可用 Credit 时，不因本地额度阈值自动把账户判为 429；真实上游 429 仍按明确响应处理。

### 奸商模式（卡429开关）

- 原“注入开关”统一改名为“奸商模式”（技术字段仍为 `openai_codex_429_guard_enabled`，保持数据兼容）。
- 开关出现在 OpenAI OAuth 账户的新建、编辑、批量编辑和“更多 -> 导入数据”流程中，默认关闭；只有显式开启的账户启用该策略。
- 对非工具续链的 Codex 请求，在消息历史末尾追加一组成对的 `custom_tool_call` 与 `custom_tool_call_output`。
- 合成工具名为 `exec`，不加入客户端传入的工具列表，但会作为历史上下文被 Agent 看到。
- 与参考实现一致，调用 ID 使用随机 `call_sub2api_overdraft_*`，且只在 `input` 最后一项严格为 `message/user` 时注入。
- compact 请求、Claude Messages 桥接、真实工具续链和 CC 渠道均跳过注入。
- OpenAI Shadow/Spark 账户不启用该注入，避免影子额度和母账户开关互相污染。
- 已有合成历史会被识别，重复转发时不会再次注入；处理请求体的上限为 32 MiB。

### 429 判定优化

- 仅 OpenAI/Codex OAuth 账户采用“两次明确 429”确认机制。
- 同一账户在 30 秒内第一次收到真实上游 429 时只切换账户，不写入账户级限流状态。
- 同一账户在确认窗口内第二次收到真实上游 429 后，才写入限流并进入冷却。
- 确认计数通过 Redis 原子脚本在多实例间共享；Redis 暂时不可用时退回单实例内存计数，仍不会因一次 429 冻结账户。
- 任意成功请求会清除该账户的 429 连续计数。
- API Key、Grok、Claude/CC 等渠道继续使用各自原有错误处理逻辑。
- Redis 计数器临时失败、恢复，以及成功请求清零失败后恢复时，会与本地观察值按账户串行对账；恢复发生在第二次 429 时也不会漏计或把旧一代计数带入新一代。

### 一键盘点与测活失败池

- 账户列表支持对全部已选择账户执行“一键盘点”；前端按每批 200 个自动分批，后端每批并发数固定为 8。
- OpenAI OAuth：经账户绑定代理查询额度、Credit、重置次数、5 小时和 7 天窗口。
- OpenAI API Key：执行真实连接测活，不伪造或显示 ChatGPT Credit。
- 单账户查询失败时自动再试一次；连续两次失败后标记为测活失败。
- 失败账户进入独立的测活失败池，展示账户名称、类型和失败原因。
- 不支持的渠道会显示为“跳过”，不会误判为死亡账户。

### 代理出口约束

- OpenAI 账户未配置 `proxy_id` 时保持原有直连；配置后请求、OAuth 刷新、额度查询、盘点、测活和 WebSocket 路径严格使用该代理。
- Antigravity 上游账户配置代理后同样固定代理出口。
- OpenAI 已绑定代理但代理记录缺失、ID 错配或 URL 为空时直接报告路由错误，不静默回退直连，避免账户出口 IP 漂移。

### OpenAI 调度优化

- 在优先级一致时，优先继续使用仍有并发余量的活跃账户。
- 按账户并发容量尽量打满少量账户；当前账户接近容量后才启用更多账户。
- 保留优先级、粘性会话、额度余量、错误率和溢出回退等既有约束。
- 健康恶化的粘性账户可逃逸，并避免同一轮调度再次选回该账户。

### 上游反代兼容

- Antigravity 上游兼容根地址、带 `/v1` 地址和已经带 `/v1/messages` 的地址，避免重复拼接路径。
- 上游模型映射会在转发请求体中生效。
- 模型列表兼容根地址、`/v1` 与 `/v1/models`，并同时发送常见的 Anthropic 认证头。
- 改进 Claude Code 风格请求头、SSE 转发和反代中转兼容。
- Gemini 上游地址已避免在配置中已有 `/v1beta` 时再次拼接 `/v1beta/v1beta`，并保留原查询参数。

### 账户管理兼容

- OpenAI 账户新建、编辑、批量编辑、数据模型导入和 API Key 更新流程同步支持本次新增字段。
- 导入时“卡429开关”只写入 OpenAI OAuth 账户，Claude、Antigravity 和 OpenAI API Key 不会被误写。
- 账户列表状态可读取持久化测活结果，页面刷新后仍能识别失败账户。
- 代理导出键改为不含密码的 `sha256:` 规范键；旧版 `protocol|host|port|username|password` 键仍可导入，但不会在错误响应中原文回显。
- 账户备份会递归包含主代理的完整备用代理链；账户导入和代理专用导入都在全部代理取得 ID 后解析备用关系，支持前向引用和多级链。
- 代理回退模式在 handler 与 service 两层只接受 `none`、`proxy`、`direct`，避免非法值绕过 Web 表单落库。

## 修复与稳健性

- 修复部分反代上游因基础 URL 路径重复导致的 404/路由失败。
- 修复首次 429 过早污染账户调度状态的问题。
- 修复有 Credit 的账户被本地阈值提前停调的问题。
- 修复合成工具历史被误判为真实工具续链的问题。
- 修复 WebSocket 请求的 `input` 为字符串或单对象时漏掉卡429合成对，以及 Responses Lite 先追加开发者工具项后导致尾部判断失效的问题。
- 修复额度查询失败仍进入 10 分钟抑制期、普通用量快照异步落库丢失错误，以及并发刷新失败误删更新探针预约的问题。
- 重置券兼容 `consumable_until` / `consumableUntil` 和 `applicable_available_count`；重置按钮优先按当前可用券数启用。
- 修复代理导入前向备用引用、伪造 `proxy_key` 和创建后重复更新吞错问题，代理配置只在全部引用解析后统一写入。
- 修复 Windows 时钟精度导致的内容审核缓存测试偶发失败。
- 增加批量盘点的重复账户 ID 校验、请求取消处理和结果顺序保持。
- 修复 `/backend-api/wham/usage` 返回空对象、空窗口或无时间信息窗口时被误判为健康的问题。
- 修复代理密码包含 `|` 时旧拼接键可能碰撞并绑定到错误代理的问题。
- 修复代理专用导入仅在状态变化时才同步 fallback 配置的问题。
- 修复安全审计异常检查器在 Python 3.8 启动阶段因类型注解语法失败的问题。

### 依赖与验证链加固

- 构建基线升级到 Go `1.27.0`，并同步根目录、后端和部署镜像以及三语源码构建文档。
- 升级 gRPC `1.82.1`、OpenTelemetry `1.43.0` 和 Protobuf `1.36.11`，消除对应的 Critical/High 依赖告警。
- 前端升级到 Vite `6.4.3`、Vitest `3.2.7`、DOMPurify `3.4.13`，并通过精确 override 修补 Mermaid、Rollup、ws、yaml、lodash、minimatch 等间接依赖。
- 修正 Vitest 3 覆盖率阈值配置，启用可执行的全局基线棘轮；当前阈值为语句/行 `69%`、分支 `70%`、函数 `45%`，后续覆盖率只能逐步提高。
- 修复支付提供商测试中的异步动态导入遗留，以及并发 release 测试依赖固定休眠导致的偶发不稳定。
- 后端 CI 与安全扫描工作流新增手动触发入口，便于发布前对指定分支重新验收。
- 修复安装脚本测试依赖 GNU `head -n -1` 导致 macOS runner 失败的问题，并清理全部 golangci-lint 2.9 报告项。
- 为旧版 Gitleaks Action 补充精确到提交、文件、规则和行号的历史测试夹具指纹；当前源码和新提交仍按完整规则扫描。
- npm 生产依赖审计仅剩 `xlsx` 的两个 High 公告；上游 npm 包暂无修复版本，继续由 `.github/audit-exceptions.yml` 的限时例外约束至 `2026-10-06`，到期前必须复查或替换。

## 验证记录

- Go 1.27.0 下除本机执行受限包外的后端包强制单元测试通过，包含 `internal/service`、`internal/repository`、handler、server 与 OpenAI WS。
- Go handler、server 及 OpenAI WS 定向测试通过；本机 Defender 会隔离 Windows `internal/middleware` 测试二进制，该包保留给 Linux CI 执行。
- OpenAI 429、额度探针缓存和账户调度定向 `-race` 测试通过。
- 前端 Vitest：228 个测试文件、1587 个测试通过。
- 前端覆盖率门禁通过：语句与行 `69.33%`、分支 `70.8%`、函数 `45.96%`。
- `vue-tsc --noEmit` 通过。
- 前端生产构建通过。
- `govulncheck ./...` 对实际调用路径报告 0 个漏洞；生产依赖审计例外检查通过。
- golangci-lint `v2.9.0` 完整扫描为 0 项，安装脚本测试兼容 macOS runner。
- `git diff --check` 通过，变更 Go 文件均通过 `gofmt` 检查。
- 真实链路 `Sub2API Plus -> Antigravity ForwardUpstream -> 反代上游` 的 Claude Code 风格 Anthropic SSE 请求已验证成功。
- Gitleaks 当前树与完整 Git 历史扫描均为零未豁免命中；历史 fixture 仅按 commit fingerprint 记录。
- Windows 本地安全策略阻止执行生成的 `internal/middleware` 测试 EXE；该测试程序编译成功，其余全部 Go 包在带/不带 `unit` 标签下强制重跑通过，Linux CI 继续执行完整集合。

构建仍会显示 Vite 的既有动态导入与 chunk 大小警告，不影响运行。
