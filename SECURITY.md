# Security policy

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting flow:

<https://github.com/Rememorio/gofer/security/advisories/new>

Include the affected version or commit, deployment assumptions, reproduction
steps, impact, and any suggested mitigation. Remove real credentials, private
keys, user content, and production data from the report.

The maintainers will assess the report privately and coordinate remediation and
disclosure based on severity and affected deployments. Please allow time for a
fix before publishing details.

## Supported versions

Security fixes target the latest published release and the current `main`
branch. Older snapshots may not receive backports. Upgrade to a supported
version before reporting behavior already corrected in a newer release.

## Scope

Reports are especially useful when they involve:

- authentication or owner-scope bypass;
- command, path, symlink, or sandbox escape;
- SSRF, DNS rebinding, or unintended private-network access;
- credential disclosure through logs, APIs, tools, or persisted state;
- webhook signature or channel identity bypass;
- unsafe model tool execution caused by malformed or truncated protocol data;
- denial of service that bypasses documented resource bounds.

Prompt injection without a boundary bypass is generally a model-behavior issue,
but it is in scope when it leads to unauthorized tool execution, cross-tenant
access, credential exposure, or violation of an enforced policy.

See [Security model](docs/security.md) for documented trust boundaries and safe
deployment defaults.
