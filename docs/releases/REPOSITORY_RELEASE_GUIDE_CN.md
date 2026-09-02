# Sub2API Plus 仓库更新与发布指南

> **发布不等于生产认证：** 当前 `0.2.x` 的开发版和正式发布版均只用于开发、兼容性测试和隔离验收。正式标签、`latest` 镜像、校验和及在线更新能力仅表示构建和分发流程完成，不表示已经通过生产级安全、压力、容灾或服务等级验证。发布、下载或升级前请阅读 [DISCLAIMER.md](../../DISCLAIMER.md)。

- 当前仓库：[`zcj-ui/sub2api-plus`](https://github.com/zcj-ui/sub2api-plus)
- 上游仓库：[`Wei-Shaw/sub2api`](https://github.com/Wei-Shaw/sub2api)
- 稳定镜像：`ghcr.io/zcj-ui/sub2api-plus:latest`
- 开发镜像：`ghcr.io/zcj-ui/sub2api-plus:dev`
- 协议：`LGPL-3.0-or-later`

## 分支与通道

| 通道 | Git 入口 | 产物 | 用途 |
|---|---|---|---|
| 开发版 | 推送 `dev` | 多平台归档、SHA256、GHCR `dev` 和不可变版本标签 | 联调、测试和验收 |
| 正式版 | 推送 `vX.Y.Z` | GitHub Release、多平台归档、SHA256、GHCR 多架构镜像和 `latest` | 固定版本验收、在线更新和回滚验证，不代表可直接生产部署 |

开发版 Actions Artifact 保留 14 天，不覆盖稳定版。正式工作流只接受三段式标签，例如 `v0.2.10`。

新仓库首次只推送 `main`/`dev` 源码时不会生成 `latest`；`dev` 工作流只发布开发镜像。首个经过审查的 `vX.Y.Z` 标签工作流成功后，Compose 默认的 `ghcr.io/zcj-ui/sub2api-plus:latest` 才可使用。

手工触发正式工作流时可选择 Simple 模式，仅构建 x86_64 GHCR 镜像。Simple 模式不会创建 GitHub Release，也不会覆盖 `/releases/latest`，避免安装器和在线更新误读一个缺少归档与校验和的版本。

## 克隆与远程

```bash
git clone https://github.com/zcj-ui/sub2api-plus.git
cd sub2api-plus
git remote add upstream https://github.com/Wei-Shaw/sub2api.git
git remote -v
```

`origin` 用于当前发行版，`upstream` 只用于拉取上游源码。

## Compose 发行源

`deploy/.env.example` 默认配置：

```dotenv
SUB2API_IMAGE=ghcr.io/zcj-ui/sub2api-plus:latest
SUB2API_UPDATE_REPO=
SUB2API_REPOSITORY_URL=https://github.com/zcj-ui/sub2api-plus
```

`SUB2API_UPDATE_REPO` 留空时使用二进制构建阶段写入的 `zcj-ui/sub2api-plus`。需要临时切换时才显式设置。

## 开发版

```bash
git switch dev
git pull --ff-only origin dev
git push origin dev
```

开发工作流从 `backend/cmd/server/VERSION` 推导下一个补丁版本，例如 `0.2.11-dev.42.1a2b3c4d`，构建 Linux、Windows、macOS 的 amd64/arm64 归档（Windows arm64 除外），并发布 `ghcr.io/zcj-ui/sub2api-plus:dev`。

## 正式版

发布前同步版本文件、更新日志并创建带说明标签：

```bash
git switch main
git pull --ff-only origin main
printf '0.2.10\n' > backend/cmd/server/VERSION
git add backend/cmd/server/VERSION docs/releases/
git commit -m "chore: prepare v0.2.10"
git tag -a v0.2.10 -m "Sub2API Plus v0.2.10"
git push origin main
git push origin v0.2.10
```

正式工作流从同一标签构建前后端，不向 `main` 自动写回版本文件。构建会注入版本、提交、时间、`release` 类型和更新仓库。

## 在线更新与回滚

面板通过 `zcj-ui/sub2api-plus` Releases 检查稳定版本。更新缓存包含仓库名，切换更新源后不会复用其他仓库的缓存。

```bash
SUB2API_UPDATE_REPO=zcj-ui/sub2api-plus ./sub2api
```

开发编译版和正式版都会显示更新信息，但面板原地更新及回滚只支持 Linux 非容器二进制服务。Docker 镜像必须通过 `docker compose pull` 与 `docker compose up -d` 更新；Windows、macOS 和源码运行版请使用相应发布包或部署工具。界面会根据后端返回的部署能力禁用不适用的原地更新操作。

## 同步上游

先在 `dev` 合并上游，完成 CI 和人工验收后通过 Pull Request 合入 `main`：

```bash
git fetch upstream --tags
git switch dev
git merge --no-ff upstream/main
git push origin dev
```

长期 Fork 使用合并提交记录同步点，不对公开分支强制 rebase 或 force push。

## 发布前检查

- 后端聚焦单元/集成测试、前端类型检查和相关 Vitest 通过。
- `actionlint`、三套 GoReleaser 配置和 YAML 解析通过。
- `git diff --check` 与 `gofmt` 通过。
- 仓库不包含 `.env`、Token、账户导出、运行数据、日志、缓存或二进制。
- 归档和镜像包含 `LICENSE` 与 `NOTICE`。
- 归档、Release 说明和源码包保留 `DISCLAIMER.md`，且没有把“正式版”“稳定镜像”描述成生产认证或可用性保证。
- Release 提供 `checksums.txt` 和 GitHub 自动生成的对应源码。

## LGPL 要求

- 保留原 `LICENSE`、`NOTICE`、上游版权和 Git 历史。
- 修改继续按 `LGPL-3.0-or-later` 发布。
- 不把 Sub2API Plus 描述成上游官方发行版。
- 分发二进制或容器时，同时提供对应源码、构建脚本和许可证文本。
