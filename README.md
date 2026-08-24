# Sub2API Plus

An AI API gateway focused on OpenAI/Codex account operations, quota inventory, fixed-proxy routing, and upstream compatibility.

English | [中文](README_CN.md) | [日本語](README_JA.md)

[![Release](https://img.shields.io/github/v/release/zcj-ui/sub2api-plus)](https://github.com/zcj-ui/sub2api-plus/releases)
[![Dev Build](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml/badge.svg?branch=dev)](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml)
[![License](https://img.shields.io/badge/license-LGPL--3.0--or--later-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27.0-00ADD8)](backend/go.mod)
[![Vue](https://img.shields.io/badge/Vue-3.4-4FC08D)](frontend/package.json)

> **Technical preview:** `0.2.x` is for development, compatibility testing, and isolated acceptance only. It has not completed an independent security audit, sustained load test, or disaster-recovery qualification and should not be used directly in production or with irreplaceable data, paid users, or high-value credentials. Read the repository-wide [use, deployment, and risk disclaimer](DISCLAIMER.md) and the [deployment commitment](docs/legal/admin-compliance.en.md) before running it. These maturity notices do not add restrictions to the LGPL license.

## Overview

Sub2API Plus is a maintained derivative of [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api). It is not an official upstream release. Attribution and copyright details are in [NOTICE](NOTICE).

Highlights:

- Unified gateway for OpenAI OAuth/API Key, Codex, Claude, Gemini/Antigravity, Grok, and related channels.
- ChatGPT credit lookup through `/backend-api/wham/usage`, using `credits.balance` and a UI reference conversion of `Credit / 25` USD.
- Selected-account inventory for credits, reset windows, proxy connectivity, and health state.
- Fixed proxy egress for OpenAI accounts, including requests, quota probes, health checks, and WebSocket traffic.
- Two explicit upstream 429 responses before an OpenAI/Codex account enters cooldown.
- OpenAI/Codex synthetic tool-history support with Claude Code, compact, real tool continuation, and Shadow/Spark exclusions.
- Capacity-aware scheduling that fills a small number of healthy accounts before expanding to more accounts.
- Compatibility with root URLs, `/v1`, complete endpoint URLs, and common OpenAI/Anthropic/Gemini reverse proxies.

See the [custom changelog](docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md) and [feature guide](docs/releases/NEW_FEATURES_GUIDE_2026-08-14_CN.md).

## Quick Start

Before the first tagged release, build from source using the instructions below. The
`latest` image is published only by a successful `vX.Y.Z` release workflow; it is
intentionally unavailable after a source-only repository push.

After a tagged release is available:

```bash
mkdir -p sub2api-plus && cd sub2api-plus
curl -sSL https://raw.githubusercontent.com/zcj-ui/sub2api-plus/main/deploy/docker-deploy.sh | bash
docker compose -f docker-compose.yml up -d
```

The tagged-release image is `ghcr.io/zcj-ui/sub2api-plus:latest`. Open `http://HOST:8080` after startup and inspect the application logs for initialization details.

The quick-start stack is a test fixture, not a production-ready configuration. Replace `POSTGRES_PASSWORD`, `JWT_SECRET`, `TOTP_ENCRYPTION_KEY`, and the administrator password even in an isolated test environment. See [deploy/README.md](deploy/README.md) for the full deployment guide.

## Source Build

Requirements: Go `1.26.6`, Node.js `20+`, pnpm `9`, PostgreSQL, and Redis.

```bash
git clone https://github.com/zcj-ui/sub2api-plus.git
cd sub2api-plus

cd frontend
pnpm install --frozen-lockfile
pnpm build

cd ../backend
VERSION="$(./scripts/resolve-version.sh)"
go build -tags embed -ldflags="-X main.Version=${VERSION}" -o sub2api ./cmd/server
go run ./cmd/server
```

Local packaged builds:

```bash
make build-dev
make build-release
```

## Release Channels

| Channel | Trigger | Output | Intended use |
|---|---|---|---|
| Development | Push to `dev` | Multi-platform artifacts and `ghcr.io/zcj-ui/sub2api-plus:dev` | Integration and acceptance testing |
| Release | Push a `vX.Y.Z` tag | GitHub Release, checksums, multi-arch image, and `latest` | Pinned acceptance testing and in-app update validation |

Packaged builds use `zcj-ui/sub2api-plus` as their update repository. Override it at runtime when necessary:

```bash
SUB2API_UPDATE_REPO=zcj-ui/sub2api-plus
```

## OpenAI/Codex Accounts

Create an OpenAI OAuth or API Key account, optionally bind a fixed proxy, save it, then run Inventory or Health Check on the selected account. Inventory results include credits, 5-hour and 7-day windows, reset data, and persisted failure reasons where supported.

OpenAI accounts without `proxy_id` keep the original direct-routing behavior. Once a proxy is configured, requests, quota probes, inventory, health checks, OAuth refreshes, and WebSocket traffic stay pinned to it; a missing, empty, or mismatched configured proxy fails closed instead of silently changing the egress IP.

## API Example

```bash
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer ${SUB2API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.3-codex","input":"hello"}'
```

Common compatibility endpoints include `/v1/responses`, `/v1/chat/completions`, `/v1/messages`, `/antigravity/v1/messages`, and `/antigravity/v1beta/`. Available models depend on accounts, groups, and model mappings.

## Documentation

- [Use, deployment, and risk disclaimer](DISCLAIMER.md)
- [Feature guide](docs/releases/NEW_FEATURES_GUIDE_2026-08-14_CN.md)
- [Custom changelog](docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md)
- [Release and upstream sync guide](docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md)
- [Source build guide](docs/releases/SOURCE_PACKAGE_BUILD_2026-08-14_CN.md)
- [Docker deployment](deploy/README.md)
- [Payment setup](docs/PAYMENT.md)
- [Asynchronous image tasks](docs/ASYNC_IMAGE_TASKS.md)

## Security

Do not commit environment files, runtime configuration, databases, logs, tokens, account exports, or proxy credentials. Test only with synthetic or disposable accounts behind HTTPS, persistent encryption secrets, strong administrator credentials, and least-privilege database accounts.

Examples, Compose files, release archives, and `latest` images are evaluation materials rather than hardened production configurations. Operators remain responsible for upstream terms, credentials, proxy trust, backups, billing verification, privacy, content handling, monitoring, and recovery. Quota snapshots and the `Credit / 25` USD display are informational only and must not be used as invoices, settlement records, or financial controls.

## License and Attribution

Sub2API Plus is distributed under [GNU LGPL v3.0 or later](LICENSE).

<a href="https://star-history.dera.page/#Wei-Shaw/sub2api&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date" />
   <img alt="Star History Chart" src="https://star-history.dera.page/svg?repos=Wei-Shaw/sub2api&type=Date" />
 </picture>
</a>

- Upstream: [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)
- Current distribution: [zcj-ui/sub2api-plus](https://github.com/zcj-ui/sub2api-plus)
- Attribution: [NOTICE](NOTICE)

Binary and container distributions must be accompanied by the corresponding source, build scripts, `LICENSE`, `NOTICE`, and `DISCLAIMER.md`. The disclaimer is an operational maturity notice and does not add license conditions.
