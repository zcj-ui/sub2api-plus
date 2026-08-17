# Sub2API Plus 更新日志

当前准备版本：`0.2.4`

发布日期：2026-08-18

> 发布状态：`0.2.x` 是技术预览和验收版本，不代表生产认证。请勿直接接入真实付费用户、高价值凭据或不可替代数据；部署前阅读[完整风险声明](../legal/admin-compliance.zh.md)并完成独立审计、压测、备份恢复和回滚演练。

## 0.2.4 发布门禁与 WebSocket 状态清理

- 修复正式 CI 在 `golangci-lint v2.9` 下发现的未使用函数、无效赋值、未检查类型断言和静态检查问题，保持 WebSocket 429 守护、会话租约和错误切换语义不变。
- 清理不再使用的状态容量兼容 helper，并为状态存储测试补齐类型断言检查，避免测试 panic 被误判为运行时成功。
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

验证结果：Go 1.26.6 下 `internal/service` 全量测试、其余后端包测试、`go vet -tags=unit ./...`、middleware 测试程序单独编译、前端 `vue-tsc --noEmit`、完整 Vitest 和 Vite 生产构建均通过。Windows 本机策略会阻止执行临时目录中的 `internal/middleware` 测试程序；该包已成功编译，完整执行继续由 Linux CI 覆盖。

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

- 构建基线升级到 Go `1.26.6`，并同步根目录、后端和部署镜像以及三语源码构建文档。
- 升级 gRPC `1.82.1`、OpenTelemetry `1.43.0` 和 Protobuf `1.36.11`，消除对应的 Critical/High 依赖告警。
- 前端升级到 Vite `6.4.3`、Vitest `3.2.7`、DOMPurify `3.4.13`，并通过精确 override 修补 Mermaid、Rollup、ws、yaml、lodash、minimatch 等间接依赖。
- 修正 Vitest 3 覆盖率阈值配置，启用可执行的全局基线棘轮；当前阈值为语句/行 `69%`、分支 `70%`、函数 `45%`，后续覆盖率只能逐步提高。
- 修复支付提供商测试中的异步动态导入遗留，以及并发 release 测试依赖固定休眠导致的偶发不稳定。
- 后端 CI 与安全扫描工作流新增手动触发入口，便于发布前对指定分支重新验收。
- 修复安装脚本测试依赖 GNU `head -n -1` 导致 macOS runner 失败的问题，并清理全部 golangci-lint 2.9 报告项。
- 为旧版 Gitleaks Action 补充精确到提交、文件、规则和行号的历史测试夹具指纹；当前源码和新提交仍按完整规则扫描。
- npm 生产依赖审计仅剩 `xlsx` 的两个 High 公告；上游 npm 包暂无修复版本，继续由 `.github/audit-exceptions.yml` 的限时例外约束至 `2026-10-06`，到期前必须复查或替换。

## 验证记录

- Go 1.26.6 下除本机执行受限包外的 105 个后端包强制单元测试通过，包含 `internal/service`、`internal/repository`、handler、server 与 OpenAI WS。
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
