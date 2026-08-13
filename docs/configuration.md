# Configuration

Gofer loads one strict, versioned YAML document. Copy `config.example.yaml` as
the starting point; it contains every supported top-level section and the
current safe defaults.

```sh
cp config.example.yaml config.yaml
gofer serve --config config.yaml
```

Values such as `$OPENAI_API_KEY` are expanded from the process environment.
Startup fails if a referenced variable is absent. Secrets should stay in the
environment or a secret manager rather than being committed to YAML.

## Minimal configuration

```yaml
config_version: 1

server:
  address: 127.0.0.1:8001

storage:
  driver: sqlite
  dsn: .gofer/gofer.db

workspace:
  root: .gofer/workspaces

models:
  - name: primary
    provider: openai
    model: gpt-5
    api_key: $OPENAI_API_KEY
```

Omitted fields receive validated defaults. Unknown fields and additional YAML
documents are rejected.

## Top-level sections

| Section | Purpose |
| --- | --- |
| `server` | HTTP listen address |
| `models` | Named model aliases and provider credentials |
| `storage` | `memory`, `sqlite`, or `postgres` durable state |
| `workspace` | Per-thread file roots and transfer limits |
| `runtime` | Turn, parallel tool, subagent, and context limits |
| `sandbox` | Local or Docker command execution boundary |
| `uploads` | Batch limits and optional document conversion |
| `browser` | Thread-scoped Chrome automation |
| `web` | Search and bounded document retrieval |
| `skills` / `mcp` | Extension discovery and external tool servers |
| `memory` | Scoped recall and memory tools |
| `auth` | Bearer principals and permissions |
| `scheduler` | Leased one-shot and cron dispatch |
| `channels` | Messaging providers and durable bindings |
| `title` / `suggestions` / `input_polish` | Optional model-backed conversation helpers |
| `loop_detection` / `read_before_write` / `tool_output` | Runtime safety and context limits |

## Model providers

Model names are stable aliases exposed through the API. Multiple aliases may
use different protocols in one process.

### OpenAI-compatible Chat Completions

```yaml
models:
  - name: primary
    provider: openai
    model: gpt-5
    api_key: $OPENAI_API_KEY
    # base_url: https://api.openai.com/v1
    options:
      display_name: GPT-5
      supports_thinking: true
      supports_vision: true
```

Set `base_url` for OpenRouter, vLLM, or another compatible endpoint. Provider
credentials are never returned by discovery APIs.

### Native Anthropic Messages

```yaml
models:
  - name: claude
    provider: anthropic
    model: claude-sonnet-4-6
    api_key: $ANTHROPIC_API_KEY
    max_tokens: 8192
    options:
      display_name: Claude Sonnet
      supports_thinking: true
      supports_vision: true
```

Anthropic accepts either `api_key` for `X-Api-Key` or `auth_token` for bearer
authentication, never both.

## Storage and workspaces

```yaml
storage:
  driver: sqlite
  dsn: .gofer/gofer.db

workspace:
  root: .gofer/workspaces
  max_read_bytes: 1048576
  max_write_bytes: 81920
  max_upload_bytes: 33554432
```

- `memory` is disposable and intended for tests or short-lived development.
- `sqlite` is the single-process default and needs no CGO.
- `postgres` uses a pgx connection string and supports shared deployments.

Workspace, upload, and output files live below a separate per-thread root.
Database backups do not include those files; back up both locations.

## Command sandbox

The local driver is fail-closed:

```yaml
sandbox:
  driver: local
  allow_host_execution: false
```

Only set `allow_host_execution: true` when the entire Gofer process and every
model request are trusted. For untrusted command execution, use the Docker
driver:

```yaml
sandbox:
  driver: docker
  image: gofer-sandbox:latest
  network_enabled: false
  memory: 1g
  cpus: 2
  pids_limit: 256
```

Commands remain bounded by script size, output size, and timeout. Network is
off unless explicitly enabled.

## Browser and web research

Browser automation is optional and thread-scoped:

```yaml
browser:
  enabled: true
  headful: false
  allow_private_addresses: false
  max_sessions: 32
```

Use `executable_path` for local Chrome or `remote_url` for a trusted remote
CDP endpoint. Navigation and intercepted requests share the same address
policy.

Web search supports Brave and SearXNG; fetching accepts bounded textual HTTP(S)
documents:

```yaml
web:
  search:
    enabled: true
    provider: brave
    api_key: $BRAVE_SEARCH_API_KEY
    max_results: 5
    allow_private_addresses: false
  fetch:
    enabled: true
    max_response_bytes: 2097152
    max_content_chars: 20000
    allow_private_addresses: false
```

Redirects and DNS answers are revalidated to resist SSRF and rebinding.

## Skills and MCP

```yaml
skills:
  enabled: true
  root: skills
  projection_root: .gofer/skills

mcp:
  enabled: true
  servers:
    - name: local-tools
      transport: stdio
      command: local-mcp-server
      arguments: [serve]
```

Skills are strict `SKILL.md` packages discovered progressively and projected
read-only for sandboxes. MCP supports stdio and Streamable HTTP. Treat both as
trusted code/configuration: their tool results are untrusted model input, but
their processes and credentials operate with deployment-granted authority.

## Authentication

Authentication is optional for loopback-only development and should be enabled
for shared or network-accessible deployments.

```yaml
auth:
  enabled: true
  tokens:
    - secret: $GOFER_ADMIN_TOKEN
      principal_id: operator
      permissions: [admin]
```

Tokens must contain at least 24 characters. Gofer retains SHA-256 digests, not
the plaintext values. Permissions include resource-scoped actions such as
`threads:read`, `threads:write`, `runs:create`, `runs:read`, `channels:write`,
`memory:read`, and `scheduled:write`; `admin` grants all actions.

Send credentials with:

```text
Authorization: Bearer <token>
```

Bearer middleware protects API routes, not `/healthz` or `/metrics`. Restrict
those operational endpoints with the listener, firewall, or reverse proxy when
they should not be publicly reachable.

## Upload conversion

Document conversion is off by default because parsers process untrusted input.
When enabled, `converter_command` executes directly without a shell, receives a
private temporary input path through `{input}`, and must write UTF-8 Markdown to
standard output. File, batch, output, timeout, outline, and context sizes remain
independently bounded.

## Channels

Each provider has its own credentials, transport allowlist, retry policy, and
connection behavior. See [Channels](channels.md) for complete provider examples
and the binding-code flow.

## Validation source of truth

`config.example.yaml` documents supported settings. `internal/config` defines
the exact schema, defaults, ranges, and cross-field validation. A deployment
should validate configuration by starting the same Gofer binary it will run;
there is no permissive compatibility mode.
