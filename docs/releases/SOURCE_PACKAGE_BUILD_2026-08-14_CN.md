# Sub2API Plus 源码构建说明

> 当前 `0.2.x` 源码和构建产物只用于开发、兼容性测试和隔离验收，尚未通过独立安全、压力和灾难恢复验证，不应直接投入生产或接入真实付费用户、高价值凭据及不可替代数据。构建完成、校验和一致和测试通过都不构成生产认证。详见仓库级[使用、部署与风险免责声明](../../DISCLAIMER.md)及[部署与运营合规承诺](../legal/admin-compliance.zh.md)。

## 环境

- Go `1.27.0`（以 `backend/go.mod` 与 CI 为准）
- Node.js `20+`
- pnpm `9`
- 可用的 PostgreSQL 和 Redis，或 Docker/Compose

## Docker 镜像

```bash
docker build \
  --build-arg VERSION=0.2.10 \
  --build-arg BUILD_TYPE=release \
  --build-arg UPDATE_REPO=zcj-ui/sub2api-plus \
  --build-arg REPOSITORY_URL=https://github.com/zcj-ui/sub2api-plus \
  -t ghcr.io/zcj-ui/sub2api-plus:0.2.10 .
```

开发镜像把 `VERSION` 换成 `0.2.11-dev.1.local`、`BUILD_TYPE` 换成 `dev`。

## 本机编译

先构建前端嵌入资源：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm build
```

再构建后端：

```bash
cd ../backend
go build -tags embed -trimpath \
  -ldflags="-s -w -X main.Version=0.2.10 -X main.BuildType=release -X main.UpdateRepo=zcj-ui/sub2api-plus" \
  -o bin/sub2api ./cmd/server
```

也可在仓库根目录执行：

```bash
make build-dev
make build-release
```

## 运行数据

不要把以下内容加入源码或发布归档：

- `.env`、`backend/config.yaml`、`deploy/config.yaml`
- `backend/data/`、PostgreSQL/Redis 数据目录
- API Key、OAuth Token、账户导出、代理认证
- `node_modules/`、`.gocache/`、`tmp/`、`dist/`、本地二进制

正式归档和镜像必须携带 `LICENSE`、`NOTICE` 与 `DISCLAIMER.md`。分发页面应继续显著说明当前版本只用于开发和隔离验收；不得用“正式构建”暗示其已经通过生产安全、容灾、持续可用性或特定上游兼容认证。
