# Gofer Agent Guide

This file applies to the entire repository.

## Product Boundary

Gofer is a Go-native, single-binary super-agent harness inspired by DeerFlow.
Preserve its durable, inspectable, local-first runtime. DeerFlow is a behavioral
and protocol reference; do not present Gofer as an official ByteDance product
or claim compatibility that is not covered by contract tests.

## Architecture

- Keep `main.go` thin. CLI behavior belongs in `internal/cli`.
- Keep the durable runtime independent from HTTP, storage, model, and sandbox
  implementations.
- Own interfaces at consumer boundaries and prefer concrete types elsewhere.
- Persist typed events before publishing them to clients.
- Route every model-requested side effect through tool validation, policy,
  workspace scope, and sandbox decisions.
- Use structured encoders for JSON, YAML, JSONL, SSE, and database data.
- Add abstractions only when they enforce a contract or remove real
  duplication.

## Go Style

- Target the Go version in `go.mod`.
- Pass `context.Context` through every cancellable operation.
- Give every goroutine an owner, cancellation path, and cleanup strategy.
- Wrap errors with operation context and preserve causes for `errors.Is` and
  `errors.As`.
- Do not panic for user, provider, model, tool, or persisted input errors.
- Document every package and exported API.
- Keep functions below the configured cognitive and cyclomatic complexity
  thresholds.
- Avoid mutable globals, hidden background work, unnecessary dependencies, and
  speculative generalization.

## Security

- Treat model output, repository content, skills, MCP responses, uploads,
  channel messages, and tool arguments as untrusted.
- Validate normalized paths and symlink behavior before filesystem access.
- Check permissions before side effects begin.
- Never log or persist credentials, authorization headers, private keys, or
  secret-bearing configuration.
- Do not weaken a boundary merely to satisfy compatibility behavior.

## Testing and Delivery

- Add focused unit tests with every behavior change.
- Add contract tests for structured protocols and regression tests for bugs.
- Exercise cancellation, concurrency, timeout, malformed input, partial
  streams, replay, and cleanup where relevant.
- Keep internal-package coverage at or above 85%.
- Run `scripts/smoke.sh` before every commit and `scripts/quality.sh` for every
  code change. Run `scripts/race.sh` for concurrency changes.
- Keep commits cohesive and use Conventional Commit messages.
- Push each completed major-module commit to `origin`; do not create a pull
  request unless explicitly requested.
- Do not commit generated state, caches, binaries, credentials, machine-local
  paths, assistant attribution, or tool branding.
