# Sub2API Plus 更新日志

当前准备版本：`0.2.0`

发布日期：2026-08-14

> 发布状态：`0.2.x` 是技术预览和验收版本，不代表生产认证。请勿直接接入真实付费用户、高价值凭据或不可替代数据；部署前阅读[完整风险声明](../legal/admin-compliance.zh.md)并完成独立审计、压测、备份恢复和回滚演练。

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

### 卡429开关

- 原“注入开关”统一改名为“卡429开关”。
- 开关出现在 OpenAI OAuth 账户的新建、编辑、批量编辑和“更多 -> 导入数据”流程中，默认开启。
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
- `git diff --check` 通过，变更 Go 文件均通过 `gofmt` 检查。
- 真实链路 `Sub2API Plus -> Antigravity ForwardUpstream -> 反代上游` 的 Claude Code 风格 Anthropic SSE 请求已验证成功。
- Gitleaks 当前树与完整 Git 历史扫描均为零未豁免命中；历史 fixture 仅按 commit fingerprint 记录。
- Windows 本地安全策略阻止执行生成的 `internal/middleware` 测试 EXE；该测试程序编译成功，其余全部 Go 包在带/不带 `unit` 标签下强制重跑通过，Linux CI 继续执行完整集合。

构建仍会显示 Vite 的既有动态导入与 chunk 大小警告，不影响运行。
