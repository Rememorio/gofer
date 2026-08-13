# Contributing to Gofer

Thank you for improving Gofer. Focused bug fixes, tests, documentation, and
well-scoped capabilities are welcome.

## Before you start

- Search existing issues and recent commits for related work.
- Open an issue before a broad protocol change, new dependency, or architectural
  rewrite so the contract can be agreed first.
- Report vulnerabilities privately through [SECURITY.md](SECURITY.md), not a
  public issue.

## Development setup

Gofer targets the Go version declared in `go.mod`.

```sh
git clone https://github.com/Rememorio/gofer.git
cd gofer
go mod download
go test ./...
```

Copy `config.example.yaml` only when running the service. Do not commit local
configuration, credentials, databases, workspaces, binaries, or generated
coverage files.

## Design expectations

- Keep `main.go` thin; process behavior belongs in `internal/cli` and service
  assembly in `internal/app`.
- Keep the durable runtime independent from HTTP, storage, model, and sandbox
  implementations.
- Own interfaces at consumer boundaries and prefer concrete types elsewhere.
- Persist typed events before publishing them.
- Route model-requested side effects through validation, policy, workspace, and
  sandbox boundaries.
- Give every goroutine an owner, cancellation path, and cleanup strategy.
- Preserve error causes for `errors.Is` and `errors.As`; do not panic on
  external input.
- Avoid speculative abstractions and unrelated refactors.

Read [Architecture](docs/architecture.md) and [Security](docs/security.md)
before changing runtime, tool, persistence, or execution boundaries.

## Tests

Every behavior change needs focused tests. Protocol changes need contract tests;
bug fixes need regression tests. Exercise cancellation, malformed input,
timeouts, replay, concurrency, and cleanup when relevant.

Run the local gates before committing:

```sh
scripts/smoke.sh
scripts/quality.sh
scripts/race.sh       # required for concurrency changes
scripts/security.sh   # required for dependency or execution-boundary changes
```

Internal-package coverage must remain at or above 85%. The quality gate checks
formatting, static analysis, exported API documentation, cognitive and
cyclomatic complexity, shell scripts, workflows, and portability.

## Documentation

Update user-facing documentation when configuration, API behavior, deployment,
security, or compatibility changes. Keep README concise; detailed reference
material belongs under `docs/`. Avoid local machine paths and environment-
specific instructions.

## Commits

Use cohesive [Conventional Commit](https://www.conventionalcommits.org/)
messages, for example:

```text
feat: add provider retry policy
fix: preserve cancelled run events
docs: simplify deployment guide
test: cover concurrent channel binding
```

Do not add generated authorship trailers or tool branding.

## Pull requests

Describe the user-visible contract, the reason for the change, validation run,
and migration impact. Keep changes small enough to review. Address review
feedback with a follow-up commit when it changes behavior, and keep CI green.
