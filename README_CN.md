# Sub2API Plus

面向 OpenAI/Codex 多账户运营、额度盘点、固定代理路由和上游兼容的 AI API 网关。

[English](README.md) | 中文 | [日本語](README_JA.md)

[![Release](https://img.shields.io/github/v/release/zcj-ui/sub2api-plus)](https://github.com/zcj-ui/sub2api-plus/releases)
[![Dev Build](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml/badge.svg?branch=dev)](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml)
[![License](https://img.shields.io/badge/license-LGPL--3.0--or--later-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8)](backend/go.mod)
[![Vue](https://img.shields.io/badge/Vue-3.4-4FC08D)](frontend/package.json)

> **测试版本警告：** `0.2.x` 只用于开发、兼容性测试和隔离验收，尚未完成独立安全审计、长期压力测试、灾难恢复演练及生产级验证，不应直接用于生产环境，也不应承载真实付费用户、不可替代数据或高价值凭据。运行前必须阅读仓库级[使用、部署与风险免责声明](DISCLAIMER.md)及[部署与运营合规承诺](docs/legal/admin-compliance.zh.md)。这些成熟度说明不对 LGPL 协议增加额外限制。

## 项目定位

Sub2API Plus 基于 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 持续开发，重点增强 OpenAI/Codex 账户管理和真实上游中转能力。本仓库不是上游官方发行版，完整来源和版权说明见 [NOTICE](NOTICE)。

主要能力：

- OpenAI OAuth、API Key、Codex、Claude、Gemini/Antigravity、Grok 等多渠道统一网关。
- ChatGPT 原始额度接口 `/backend-api/wham/usage`，解析 `credits.balance`，前端按 `Credit / 25` 显示美元参考值。
- 对已选择账户执行一键盘点：额度、积分、重置窗口、代理连通性和测活状态一次完成。
- OpenAI 账户固定代理出口；代理丢失或配置错误时明确失败，不静默直连。
- OpenAI/Codex 两次明确 429 才进入账户冷却；有可用 Credit 时不因本地阈值提前停调。
- “卡429”合成工具历史只作用于符合条件的 OpenAI/Codex 请求；Claude Code、compact、真实工具续链和 Shadow/Spark 不注入。
- 调度器优先在并发容量内复用少量健康账户，达到容量后再扩展更多账户。
- 兼容根地址、`/v1`、完整端点和常见反代上游的 OpenAI、Anthropic、Gemini 请求格式。

详细变更见[定制更新日志](docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md)，操作方法见[新功能使用说明](docs/releases/NEW_FEATURES_GUIDE_2026-08-14_CN.md)。

## 快速部署

### Docker Compose

首次创建仓库且尚未推送 `vX.Y.Z` 标签时，请先按下方“本地源码运行”构建。只有正式标签工作流成功后才会发布 `ghcr.io/zcj-ui/sub2api-plus:latest`；只推送源码不会生成该镜像。

首个正式标签发布成功后使用：

```bash
mkdir -p sub2api-plus && cd sub2api-plus
curl -sSL https://raw.githubusercontent.com/zcj-ui/sub2api-plus/main/deploy/docker-deploy.sh | bash
docker compose -f docker-compose.yml up -d
```

正式标签默认镜像：

```text
ghcr.io/zcj-ui/sub2api-plus:latest
```

查看初始化日志：

```bash
docker compose -f docker-compose.yml logs -f sub2api
```

浏览器访问 `http://服务器地址:8080`。快速部署只作为测试环境模板；即使在隔离测试环境也必须替换 `.env` 中的 `POSTGRES_PASSWORD`、`JWT_SECRET`、`TOTP_ENCRYPTION_KEY` 和管理员密码。

完整部署参数见 [deploy/README.md](deploy/README.md)。

### 本地源码运行

环境要求：Go `1.26.6`、Node.js `20+`、pnpm `9`、PostgreSQL、Redis。

```bash
git clone https://github.com/zcj-ui/sub2api-plus.git
cd sub2api-plus

cd frontend
pnpm install --frozen-lockfile
pnpm build

cd ../backend
go run ./cmd/server
```

本地完整编译：

```bash
make build-dev      # 开发编译版
make build-release  # 正式编译版
```

## 版本通道

| 通道 | 入口 | 产物 | 用途 |
|---|---|---|---|
| 开发版 | `dev` 分支 | Actions 多平台归档、`ghcr.io/zcj-ui/sub2api-plus:dev` | 联调和功能验收 |
| 发布版 | `vX.Y.Z` 标签 | GitHub Release、SHA256、多架构镜像、`latest` | 固定版本验收和在线更新验证 |

正式版与开发编译版都会写入当前更新仓库。运行时可用下面的环境变量显式覆盖：

```bash
SUB2API_UPDATE_REPO=zcj-ui/sub2api-plus
```

发布、上游同步和分支策略见[仓库发布指南](docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md)。

## OpenAI/Codex 账户要求

1. 在管理后台创建 OpenAI OAuth 或 API Key 账户。
2. OpenAI OAuth/API Key 账户可按需绑定固定代理；未设置 `proxy_id` 时保持原有直连行为。
3. 保存后勾选账户，执行“一键盘点”或“测活”。
4. 根据 Credit、5 小时/7 天窗口、重置次数和失败池结果处理异常账户。

OpenAI 账户未设置 `proxy_id` 时保持原有直连行为。一旦绑定代理，请求、OAuth 刷新、额度查询、测活、盘点和 WebSocket 都固定使用该代理；代理记录丢失、URL 为空或 ID 错配时失败关闭，不会静默改走本机直连。

## API 入口

使用后台创建的 API Key 访问服务：

```bash
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer ${SUB2API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.3-codex","input":"hello"}'
```

常用兼容入口包括 `/v1/responses`、`/v1/chat/completions`、`/v1/messages`、`/antigravity/v1/messages` 和 `/antigravity/v1beta/`。实际可用模型由账户、分组和模型映射共同决定。

## 文档

- [使用、部署与风险免责声明](DISCLAIMER.md)
- [新功能使用说明](docs/releases/NEW_FEATURES_GUIDE_2026-08-14_CN.md)
- [更新日志](docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md)
- [发布与上游同步](docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md)
- [源码构建说明](docs/releases/SOURCE_PACKAGE_BUILD_2026-08-14_CN.md)
- [Docker 部署](deploy/README.md)
- [支付配置](docs/PAYMENT_CN.md)
- [异步图片任务](docs/ASYNC_IMAGE_TASKS.md)
- [管理端支付集成 API](docs/ADMIN_PAYMENT_INTEGRATION_API.md)

## 安全与数据

- 不要提交 `.env`、`backend/config.yaml`、数据库、Redis 数据、日志、Token 或账户导出文件。
- 对外分享问题日志前移除 API Key、OAuth Token、代理认证和账户标识。
- 只用合成或可丢弃账户测试，并使用 HTTPS、强密码、固定加密密钥和最小权限数据库账户。
- 使用第三方上游、OAuth 账户和代理前，确认其服务条款及所在地区要求。
- 示例配置、Compose、发布归档和 `latest` 镜像都不是经过加固的生产配置；“正式构建”仅表示分发通道，不表示生产认证或服务等级承诺。
- 额度、重置窗口和测活结果都是时点快照，`Credit / 25` 美元显示仅供参考，不得作为账单、结算、授信或自动收费依据。
- 批量盘点、导入、批量编辑和在线更新前，先备份数据库、加密密钥、配置与账户数据，并在隔离副本中实际验证恢复和回滚。

## 协议与归属

本项目按 [GNU Lesser General Public License v3.0 or later](LICENSE) 发布。

- 上游项目：[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)
- 当前发行版：[zcj-ui/sub2api-plus](https://github.com/zcj-ui/sub2api-plus)
- 原始版权与修改说明：[NOTICE](NOTICE)

分发二进制或容器时，应同时提供对应版本源码、构建脚本、`LICENSE`、`NOTICE` 和 `DISCLAIMER.md`。免责声明只说明版本成熟度和运维风险，不增加许可条件。
