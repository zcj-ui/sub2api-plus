# Sub2API Plus 交接文档

> 日期：2026-08-16
> 适用版本：v0.2.2（提交 `91755673e`，main/dev 同步）
> 上游：Wei-Shaw/sub2api（LGPL-3.0-or-later，保留原协议与署名，见 NOTICE）

---

## 1. 仓库与分支概览

| 项 | 值 |
|---|---|
| 本地路径 | `F:\sub2api` |
| origin | `https://github.com/zcj-ui/sub2api-plus.git`（公开仓库） |
| upstream | `https://github.com/Wei-Shaw/sub2api.git`（只读，用于同步官方修复） |
| main | 正式版分支，只接受经过验证的提交 |
| dev | 开发分支，与 main 同步推进；推送后自动产出开发构建 |
| 发布方式 | `vX.Y.Z` 标签触发正式 Release（禁止 dev 标签进正式通道） |

工作流约定（用户要求）：**先推 dev，GitHub Actions（CI / Security Scan / Development build）全绿后才推 main；正式版需多轮测试后再打标签**。

## 2. 版本现状

- **v0.2.2**（最新正式版）：账户列表显示修复、在线更新错误结构化、调度粘性重做。Release 含 5 平台二进制归档 + checksums，正文带完整更新日志。
- **v0.2.1**：OpenAI/Codex 兼容性修复批次（对齐上游 #5668 等 10 个 PR/Issue）。
- GHCR 镜像（已验证在线）：`ghcr.io/zcj-ui/sub2api-plus` 的 `latest`、`0.2.2`、`dev` 及不可变 dev 版本标签。
- 一键脚本：`curl -sSL https://raw.githubusercontent.com/zcj-ui/sub2api-plus/main/deploy/install.sh | sudo bash`
- **服务器（107.150.53.139）当前运行 v0.2.1，尚未升级 v0.2.2**（面板"系统更新"点击即可，无数据库迁移）。

## 3. 主要工作内容（按域）

### 3.1 额度与积分（仅 ChatGPT/Codex OAuth）
- 主动查询改走 `GET /backend-api/wham/usage`，解析 `credits.balance`（字符串原值入库 `extra.codex_credit_snapshot`）。
- 前端显示 Credit 原值 + `Credit ÷ 25` 美元参考值。
- 有可用积分（`has_credits`/`unlimited`/`overage_limit_reached` 联合判定）时跳过本地额度阈值的自动停调；连续两次真实 429 仍会冻结（积分不掩盖真实限流）。
- 5h/7d 窗口同时从 wham 与响应头 `x-codex-*` 更新；Spark 影子账号独立窗口，不继承母账号积分。

### 3.2 两次确认 429 状态机
- OpenAI OAuth 账号收到上游 429：第一次只记录额度快照并触发本次请求 failover，**不冻结账号**；同一账号 30 秒内第二次明确 429 才写入限流。
- 计数入 Redis（多实例合并），Redis 故障回退本地内存；成功请求或非 429 清零；恢复对账保证跨故障窗口仍精确"两次"。
- CC/Claude、API Key、Grok、Spark 均不走此状态机。

### 3.3 "卡429"合成工具注入（对齐 DeanZFC/sub2api-overdraft `766daa0`）
- 非工具结尾的请求历史尾部注入成对 `custom_tool_call` + `custom_tool_call_output`，工具名 `exec`，随机 `call_sub2api_overdraft_*` call_id，不写入 tools 列表。
- 注入条件：账号开关 `extra.openai_codex_429_guard_enabled`（默认关闭，显式 `true` 才启用）+ 尾部为 message/user（含 Chat Completions 转换与文本输入形态）。
- 排除：CC/Claude 桥接、compact、真实工具续链、Shadow/Spark、历史已有注入（幂等）、body 超 32 MiB、无效 JSON 原样放行。
- UI 开关统一显示为“奸商模式”，覆盖创建/编辑/批量编辑/数据导入；后端校验仅 OpenAI OAuth 可写。

### 3.4 Codex 指纹（会话身份模拟）
- 模式 `off/device/session/full`，**默认 off（显式 opt-in）**——对齐上游 #5668/#5610 结论：默认收敛曾导致额度缩水与风控。
- session 模式：一账号一稳定设备（`openai_device_id` 或派生）+ 每对话稳定 session/thread + 每请求新 turn；header、body `client_metadata`、原始 JSON 透传三载体共用同一套 ID；failover 清除上一账号暂存身份。
- 会话种子：header `session-id` → `client_metadata.session_id/thread_id` → `prompt_cache_key` → 内容锚，全部按 API Key 隔离。
- 未派生 `prompt_cache_key`/`conversation_id`（刻意保守，cockpit 式增强可后续做）。

### 3.5 代理强制（用户硬性要求）
- 账户设置了 `proxy_id`：所有出站（请求转发、WS、额度、测活、盘点、OAuth 刷新、能力探测）固定走该代理；代理关系缺失/ID 错配/URL 无效 → **失败关闭，绝不静默直连**。
- 账户未设置代理：保持原直连行为（不强制绑定代理）。

### 3.6 调度（v0.2.2 重做，解决"会话拆号/缓存丢/优先级跳"）
- **并发满不换号**：粘性账号打满返回等待计划（受 `StickySessionWaitTimeout/StickySessionMaxWaiting` 约束），会话不再拆分。
- **粘性逃逸默认关闭**（TTFT/错误率触发的跳号改为显式 `gateway.openai_scheduler.sticky_escape_enabled`）；高并发 TTFT 抖动不再引发迁移。
- **溢出阀**：显式开逃逸且等待队列饱和时，请求释放到有空位的账号但保留原绑定，原号恢复后自动回去；**新会话始终优先有立即空位的账号**（`PackedTopKOpensIdleOverflowWhenActiveIsFull` 锁定）。
- **空闲租约**：绑定 TTL 从 1h 改为滑动窗口，默认 60s，运行时设置键 `openai_sticky_session_idle_ttl_seconds` 可调（如 10s）；每次命中续期。
- 装箱排序：管理员优先级 → 活跃优先（打满少数号）→ 满载程度 → 健康评分 → 稳定 ID。

### 3.7 测活 / 一键盘点 / 失败池
- `POST /admin/accounts/batch-health-probe`：OAuth 查额度+重置次数，API Key 走真实 Responses/Chat 连接探测；两次完整确认才判死。
- `POST /admin/accounts/batch-inventory`：选定账户盘点（≤200/批，前端自动分批），返回健康+积分+窗口。
- 测活死亡持久化 `extra.account_health_probe`，进入独立失败池并退出调度；测活成功自动恢复；全局失败池接口支持刷新后还原。
- 所有探测强制走账户配置代理。

### 3.8 前端显示修复（v0.2.2）
- 根因：首屏列表请求带 `lite=1`（响应仅 id/name/platform/type/健康快照），导致 `admin.accounts.status.undefined`、并发 `0/`、额度列全坏，需手动刷新。已移除首屏 lite（lite 仅保留给全选元数据），回归测试锁定。
- 状态徽标兜底：未知/缺失 status 显示"未知"；补 `disabled/expired` 历史状态文案（中英）。

### 3.9 在线更新（v0.2.2）
- 目录无写权限返回结构化 `UPDATE_DIRECTORY_NOT_WRITABLE`（HTTP 409），不再只有 `internal error`。
- 安装/升级脚本对安装目录 `chown $SERVICE_USER` + `chmod u+rwx`，文档同步。
- 更新源绑定本仓库（构建注入 + `SUB2API_UPDATE_REPO` 可覆盖），缓存按仓库隔离。

### 3.10 Antigravity/反代上游兼容（v0.2.1 前后）
- `upstream` 账户支持 New API 类反代：URL 规范化（根/`/v1`/完整路径/query 保留）、双鉴权头、模型映射、头覆写、429/401/403/5xx 正确 failover。
- Chat Completions/Responses/Gemini Native 路径打通；实测 `sub2 → ai.aiking.one` 全链路 SSE 成功。
- 上游错误脱敏（非官方 host/私有 IP 不外泄）。

### 3.11 测试与质量
- 后端 `internal/service` 全量、config、handler/admin 等通过；关键定向用例按 `-count=3` 重复运行。
- 前端 vue-tsc + Vitest 全量（229 文件）通过；ESLint、Vite 构建通过。
- 已知环境项：Windows 本机拒绝执行临时目录的 `middleware.test.exe`（编译通过，Linux CI 覆盖）；`internal/middleware` 以编译验证。

## 4. 发布流程操作手册

1. 改动提交到 main（本地）；`git branch -f dev main && git push origin dev`。
2. 等 GitHub Actions 三项（CI / Security Scan / Development build）全绿。
3. `git push origin main`。
4. 升版本号：`backend/cmd/server/VERSION` + `docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md` 增加 `## X.Y.Z` 段落（**Release 正文会自动提取该段落**，工作流 awk 注入 RELEASE_NOTES；无段落则回退 tag 消息）。
5. `git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z` → 自动出 Release + 归档 + checksums + `latest` 镜像。
6. 服务器在面板"系统更新"点击升级（或用 termark 手动部署，见下）。

## 5. 服务器部署（美国物理机）

| 项 | 值 |
|---|---|
| Termark 资产 | 名称"美国物理"，ID `769QthsdMjJJXTmi`，107.150.53.139 |
| 部署形态 | 二进制 + systemd（`sub2api.service`，运行用户 `sub2api`），端口 8080，Nginx 反代 |
| 目录 | `/opt/sub2api`（属主 root:sub2api 0775，运行用户可写以支持在线更新） |
| 数据库 | 本机 PostgreSQL（库 sub2api）+ Redis 127.0.0.1:6379 db3 |
| 备份 | `/root/sub2api-backups/20260815-104327`（19 文件 SHA256 校验，含 pg 转储+RDB+旧二进制+配置）；停服快照子目录 `pre-cutover-*` |
| 回滚二进制 | `/opt/sub2api/sub2api.rollback.*`；systemd 无自动重启回滚 |
| 当前版本 | **v0.2.1（4b581b73），待升级 v0.2.2** |
| 部署方法 | CI 产物（linux_amd64 归档）→ termark 上传 → 停服补快照 → 原子替换 → 健康检查（`/health` 200 + 数据计数核对） |

操作约束：只用 termark（不直接 ssh/scp）；部署前必做新备份；账户/代理数据绝不改。

## 6. 安全与合规

- LGPL-3.0-or-later 原样保留；`NOTICE` 声明上游来源与本仓修改；`DISCLAIMER.md`（≥3000 字）分散于三语 README、部署、安全、发布文档；Release 正文带技术预览警告。
- Secret Scanning + Push Protection 已启用；gitleaks 当前树与全部历史零命中（`.gitleaksignore` 仅精确记录已审阅 fixture）。
- Dependabot 已知 9 条无修复版本告警：5 条 docker 仅测试依赖 + 4 条 xlsx 重复记录（例外至 2026-10-06）；`govulncheck` 实际调用路径 0 漏洞。
- 用户测试域名/API Key 未进仓库。

## 7. 重要设计决策（勿轻易反转）

1. **指纹收敛默认 off**：上游 #5610/#5668 实测默认收敛引发额度缩水；用户在官方仓验证 opt-in 后额度恢复。需要时按账号显式开。
2. **粘性逃逸默认关 + 并发满等待**：会话拆号的直接根因；TTFT 抖动是高并发常态不是故障信号。
3. **代理语义**：设了代理→强制并 fail-closed；没设→直连。不是"所有 OpenAI 都必须绑代理"。
4. **两次 429 才冻结**：首次 429 是上游常态反馈，立即冻结会误杀；积分只绕过本地阈值，不绕过真实 429。
5. **卡429 只作用于 OpenAI/Codex OAuth**：CC/Claude 渠道明确排除（用户要求）。

## 8. 待办与已知事项

- [ ] **服务器升级 v0.2.2**（面板点击或 termark 部署；调度修复需此版本生效）。
- [ ] "临时回退官方客户端"功能：用户已要求延后（备份确认+强制下载流程的设计草稿曾做过又剥离）。
- [ ] cockpit 式指纹增强（`prompt_cache_key`/`conversation_id` 派生）可选后续。
- [ ] xlsx 两条 High 无上游修复，例外 2026-10-06 到期需复核。
- [ ] 上游同步：官方自 #5668 后仅版本号提交，无功能更新；#5649（分组用量）刻意未纳入。

## 9. 本机工具链备忘

- Go：`C:\Users\z2088\AppData\Local\codex-go\go1.26.5-complete\go\bin\go.exe`
- 缓存（勿放 C 盘）：`GOCACHE=F:\sub2api\.gocache`、`GOMODCACHE=F:\sub2api\.gomodcache`、`GOTMPDIR=F:\sub2api\tmp`
- 前端：`F:\sub2api\frontend\node_modules\.bin\`（vitest/vue-tsc，从 frontend 目录运行）
- GitHub API 匿名读足够；写操作令牌来自 git credential manager（`git credential fill`，勿打印勿落盘）
- 本 shell 为 cmd：`|` `;` 会被吞，复杂命令用 `powershell -NoProfile -Command "..."`；写含中文的 .ps1 必须带 UTF-8 BOM

## 10. 关键文件索引

| 域 | 文件 |
|---|---|
| 429 状态机 | `backend/internal/service/openai_account_runtime_block_fastpath.go`、`ratelimit_service.go` |
| 额度/积分 | `backend/internal/service/openai_quota_service.go`、`account_usage_service.go` |
| 卡429 注入 | `backend/internal/service/openai_codex_transform.go`、`openai_tool_continuation.go` |
| 指纹 | `backend/internal/service/openai_codex_fingerprint.go` |
| 调度粘性/逃逸/TTL | `backend/internal/service/openai_account_scheduler.go`、`openai_gateway_scheduling.go` |
| 代理强制 | `backend/internal/service/openai_required_proxy.go` |
| 测活/盘点 | `backend/internal/service/account_health_probe.go` |
| 更新器 | `backend/internal/service/update_service.go`、`deploy/install.sh` |
| 发布 | `.github/workflows/release.yml`、`dev-build.yml`、`.goreleaser*.yaml` |
| 变更日志 | `docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md` |
| 免责声明 | `DISCLAIMER.md`、`NOTICE` |
