# Sub2API Plus Container Image

> The `0.2.x` images are technical-preview artifacts for isolated testing. They are not production-qualified; do not attach live paid users, irreplaceable data, or high-value credentials. Pin a version and digest, keep tested backups, and read the [complete risk notice](../docs/legal/admin-compliance.en.md).

Stable image:

```text
ghcr.io/zcj-ui/sub2api-plus:latest
```

This tag exists only after a successful `vX.Y.Z` release workflow. For a new
source-only fork with no release yet, use the repository source build instead
of the Compose image workflow.

Development image:

```text
ghcr.io/zcj-ui/sub2api-plus:dev
```

## Recommended Deployment

Use the repository Compose files because the application requires PostgreSQL, Redis, persistent secrets, and a data directory:

```bash
mkdir -p sub2api-plus && cd sub2api-plus
curl -sSL https://raw.githubusercontent.com/zcj-ui/sub2api-plus/main/deploy/docker-deploy.sh | bash
docker compose -f docker-compose.yml up -d
```

## Upgrade

```bash
docker compose -f docker-compose.yml pull
docker compose -f docker-compose.yml up -d
```

Pin a stable version in `.env` when automatic image movement is not desired:

```dotenv
SUB2API_IMAGE=ghcr.io/zcj-ui/sub2api-plus:0.2.0
SUB2API_UPDATE_REPO=zcj-ui/sub2api-plus
```

## Architectures

- `linux/amd64`
- `linux/arm64`

## Image Metadata

Images include OCI source, revision, version, and `LGPL-3.0-or-later` labels. `LICENSE`, `NOTICE`, and `DISCLAIMER.md` are installed under `/usr/share/licenses/sub2api/`. The disclaimer records the current technical-preview maturity and operational risks; it does not add conditions to the license.

See [README.md](README.md) for environment variables, persistence, health checks, and acceptance-test requirements.
