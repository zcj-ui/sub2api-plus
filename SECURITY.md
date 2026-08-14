# Security Policy

## Release Maturity

The `0.2.x` series is a technical preview and has not completed independent security, load, or disaster-recovery qualification. Do not expose it as a production service or use live high-value credentials while evaluating it. This operational warning does not change the repository's LGPL-3.0-or-later license. Review [DISCLAIMER.md](DISCLAIMER.md) and the [complete operational notice](docs/legal/admin-compliance.en.md).

Passing CI, dependency scans, focused tests, or a maintainer review does not establish that a deployment is hardened. Operators must independently validate authentication, authorization, proxy egress, secret storage, log redaction, trusted-forwarding headers, database and Redis exposure, backups, restoration, monitoring, rate limits, and incident shutdown procedures. Example deployments and default settings are evaluation fixtures.

## Supported Versions

Security fixes target the latest stable release and the current `dev` branch. Upgrade older deployments before requesting support.

“Supported” in this policy identifies branches that receive repository security fixes. It is not a promise of uptime, response time, long-term support, data recovery, upstream-account recovery, production suitability, or compatibility with every relay and model provider.

## Reporting

Use GitHub's private vulnerability reporting for `zcj-ui/sub2api-plus`. Do not open a public Issue containing an exploitable vulnerability, API key, OAuth token, account export, proxy credential, database dump, or production log.

Include:

- affected version and commit;
- deployment mode and operating system;
- minimal reproduction with synthetic data;
- expected and observed behavior;
- impact and any temporary mitigation.

## Operational Scope

Credential rotation, proxy ownership, upstream account policy, TLS termination, database access, backups, and administrator authentication remain the deployment operator's responsibility. See [deploy/EDGE_SECURITY.md](deploy/EDGE_SECURITY.md).
