# Contributing to Sub2API Plus

> Contributions and test results are evaluated in development and isolated acceptance environments. They do not certify production readiness or remove the operational risks described in [DISCLAIMER.md](DISCLAIMER.md). This notice does not add conditions to the LGPL-3.0-or-later license.

## Workflow

1. Create a branch from `dev`.
2. Keep changes scoped and include tests for behavior changes.
3. Run the relevant backend and frontend checks from [DEV_GUIDE.md](DEV_GUIDE.md).
4. Open a Pull Request into `dev` and describe user-visible behavior, migration impact, and verification.
5. Stable changes reach `main` through a reviewed `dev` to `main` Pull Request.

## Compatibility

OpenAI/Codex changes must preserve fixed-proxy routing and the explicit exclusions documented in the development guide. Changes to account fields must cover create, edit, bulk edit, import, API types, and persisted data compatibility.

## Security

Do not include real API keys, OAuth tokens, account exports, proxy credentials, environment files, databases, or production logs. Use synthetic fixtures in tests and reports.

Upstream compatibility tests can create charges, account restrictions, data disclosure, or repeated side effects. Use disposable fixtures, explicit budgets, verified proxy egress, redacted captures, idempotent test actions, and documented cleanup. A green test against one relay or response sample does not establish compatibility with every provider, region, account type, or future API revision.

## License

Contributions are accepted under the repository's [LGPL-3.0-or-later](LICENSE) terms. Preserve upstream attribution in [NOTICE](NOTICE). This repository does not require the upstream project's CLA.
