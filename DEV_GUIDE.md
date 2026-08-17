# Sub2API Plus 开发指南

> 本指南中的启动、构建、测试、镜像和发布命令只用于开发与隔离验收。当前 `0.2.x` 未完成生产级安全、压力、容灾和回滚验证，不应把本地测试通过或 release 构建成功理解为可直接投入生产。开发前请阅读 [DISCLAIMER.md](DISCLAIMER.md)；该提示不改变 LGPL-3.0-or-later 许可。

## 仓库关系

- 当前仓库：[`zcj-ui/sub2api-plus`](https://github.com/zcj-ui/sub2api-plus)
- 上游仓库：[`Wei-Shaw/sub2api`](https://github.com/Wei-Shaw/sub2api)
- 默认分支：`main`
- 开发快照分支：`dev`
- 协议：`LGPL-3.0-or-later`

为减少长期 Fork 的迁移成本，Go module 路径暂时保留为 `github.com/Wei-Shaw/sub2api`。内部 Go import 不代表运行时更新源；正式和开发构建会把 `zcj-ui/sub2api-plus` 写入更新信息。

## 技术栈

| 模块 | 技术 |
|---|---|
| 后端 | Go 1.26.6、Gin、Ent |
| 前端 | Vue 3、TypeScript、Vite、Pinia、pnpm 9 |
| 数据 | PostgreSQL、Redis |
| 发布 | GitHub Actions、GoReleaser、GHCR |

## 目录

```text
backend/                 Go 服务、迁移和运行资源
frontend/                Vue 管理端
deploy/                  Docker、Compose、systemd 和安装脚本
docs/                    功能、支付、合规和发布文档
.github/workflows/       CI、开发快照、稳定发布和安全扫描
```

## 本地环境
## 三、CI/CD 流水线

### GitHub Actions Workflows

| Workflow | 触发条件 | 检查内容 |
|----------|----------|----------|
| **backend-ci.yml** | push, pull_request | 单元测试 + 集成测试 + golangci-lint v2.9 |
| **security-scan.yml** | push, pull_request, 每周一 | govulncheck + gosec + pnpm audit |
| **release.yml** | tag `v*` | 构建发布（PR 不触发） |

### CI 要求

- Go 版本必须是 **1.26.6**：三个 workflow 都用 `go-version-file: backend/go.mod` 取版本，随后硬断言 `go version | grep -q 'go1.26.6'`。升级 Go 时要同时改 `backend/go.mod`、`backend-ci.yml`（两处）、`release.yml`、`security-scan.yml` 里的这句断言，**以及三个 Dockerfile 里的 Go 构建镜像**（`Dockerfile` / `deploy/Dockerfile` 的 `ARG GOLANG_IMAGE`、`backend/Dockerfile` 的 `FROM golang:`）。前者漏了 CI 会在版本校验步骤直接失败；**后者漏了 CI 不会报，而是等到有人用这些 Dockerfile 构建时才失败**（`go.mod requires go >= X (running Y; GOTOOLCHAIN=local)`）。
- 前端使用 `pnpm install --frozen-lockfile`，必须提交 `pnpm-lock.yaml`

### 本地测试命令

```bash
git clone https://github.com/zcj-ui/sub2api-plus.git
cd sub2api-plus
git remote add upstream https://github.com/Wei-Shaw/sub2api.git
```

前端：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm dev
```

后端需要可用的 PostgreSQL 和 Redis：

```bash
cd backend
go run ./cmd/server
```

不要在文档或提交中保存本机数据库密码、OAuth Token、API Key、代理认证或 `.env` 内容。

测试应优先使用合成响应、可撤销凭据和可丢弃账户。真实上游测试可能产生费用、触发账号风控并把请求内容交给第三方；执行者需要独立核对代理出口、上游条款、日志脱敏和测试预算。不要把额度快照、测活成功或一次兼容性测试成功当作持续可用性保证。

## 构建

```bash
make build-dev
make build-release
```

手工指定更新仓库：

```bash
make -C backend build-release UPDATE_REPO=zcj-ui/sub2api-plus VERSION=0.2.0
```

Windows 上执行大规模 Go 测试时，建议把缓存和临时目录放到空间充足的数据盘：

```powershell
$env:GOCACHE='F:\sub2api\.gocache'
$env:GOTMPDIR='F:\sub2api\tmp\go-build'
```

## 测试

后端：

```bash
cd backend
go test -tags=unit ./...
go test -tags=integration ./...
golangci-lint run ./...
```

前端：

```bash
cd frontend
pnpm run lint:check
pnpm run typecheck
pnpm run test:run
pnpm run build
```

发布配置：

```bash
actionlint
export GITHUB_REPOSITORY=zcj-ui/sub2api-plus
export GITHUB_REPO_OWNER=zcj-ui GITHUB_REPO_OWNER_LOWER=zcj-ui
export GITHUB_REPO_NAME=sub2api-plus GITHUB_REPO_NAME_LOWER=sub2api-plus
export DOCKERHUB_USERNAME=skip TAG_MESSAGE=validation
goreleaser check --config .goreleaser.yaml
goreleaser check --config .goreleaser.simple.yaml
DEV_VERSION=0.2.1-dev.1.local \
  goreleaser check --config .goreleaser.dev.yaml
```

## OpenAI/Codex 修改检查项

- 请求、额度、测活和 WebSocket 在账户绑定代理后必须使用同一代理。
- “卡429”工具历史只作用于符合条件的 OpenAI/Codex 用户消息尾部。
- Claude Code、compact、真实工具续链和 Shadow/Spark 继续排除。
- 首次明确 429 只切换账户；确认窗口内第二次明确 429 才写入冷却。
- Credit 可用时不因本地订阅窗口阈值提前停调。
- 新字段必须同步新建、编辑、批量编辑、导入、API 类型和测试。

## 同步上游

在 `dev` 上合并上游，完成 CI 和人工验收后再通过 Pull Request 合入 `main`：

```bash
git fetch upstream --tags
git switch dev
git merge --no-ff upstream/main
git push origin dev
```

不要对已公开的 `main` 或 `dev` 强制推送。

## 发布

- 推送 `dev`：生成开发归档和 `ghcr.io/zcj-ui/sub2api-plus:dev`。
- 推送 `vX.Y.Z`：生成稳定 Release、校验文件和 `latest` 镜像。
- 发布前手工同步 `backend/cmd/server/VERSION`。
- Release 必须包含源码、`LICENSE`、`NOTICE` 和可复现构建配置。

完整流程见 [docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md](docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md)。
