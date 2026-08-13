# Documentation

Start with the guide that matches what you are trying to do.

## Use Gofer

| Guide | Contents |
| --- | --- |
| [Getting started](getting-started.md) | Requirements, installation, first run, streaming, and next steps |
| [Configuration](configuration.md) | Top-level settings, model providers, storage, tools, auth, and extensions |
| [API](api.md) | HTTP conventions and endpoint groups for clients and integrations |
| [Channels](channels.md) | Native chat providers, connection lifecycle, commands, and signed webhooks |

## Operate Gofer

| Guide | Contents |
| --- | --- |
| [Deployment and releases](deployment.md) | Container deployment, release artifacts, persistence, and upgrades |
| [Security model](security.md) | Threat model, execution and network boundaries, tenant isolation, and checklist |
| [Compatibility](compatibility.md) | DeerFlow-aligned behavior, reference baseline, and explicit non-goals |

## Contribute

| Guide | Contents |
| --- | --- |
| [Architecture](architecture.md) | Dependency direction, durable lifecycle, core packages, and invariants |
| [Contributing](../CONTRIBUTING.md) | Development workflow, tests, style, and commits |
| [Security policy](../SECURITY.md) | Private vulnerability reporting and supported versions |

`config.example.yaml` remains the canonical, versioned configuration example.
Documentation explains the intent and safe defaults; the Go configuration
types and validation rules define the accepted schema.
