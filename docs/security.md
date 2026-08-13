# Security model

Gofer executes model-selected tools and processes untrusted external content.
Its security model is therefore based on explicit trust boundaries, bounded
resources, and fail-closed defaults—not on assuming a model will follow a
prompt.

## Threat model

Treat all of the following as untrusted:

- user prompts and uploaded files;
- model responses and tool arguments;
- repository and workspace content;
- browser pages, search results, and fetched documents;
- MCP and skill tool results;
- channel messages, webhook bodies, and attachment metadata;
- persisted records created by an older or compromised process.

Deployment configuration, provider credentials, installed MCP processes, skill
packages, and sandbox images are operator-controlled inputs. They can grant
real authority and must be reviewed accordingly.

## Execution boundary

All model-requested side effects pass through typed tool schemas, policy
middleware, workspace resolution, and a sandbox decision.

Host execution is disabled by default. Enabling
`sandbox.allow_host_execution` gives model-selected shell commands the same OS
authority as the Gofer process and is appropriate only for a fully trusted
environment.

The Docker driver is the preferred boundary for untrusted commands. Its
baseline is read-only, capability-free, PID/CPU/memory bounded, and
network-isolated. Mounting a host Docker socket effectively grants host-level
control and must be treated as such.

Command input, timeout, and output are bounded. Long output is stored below a
private thread output directory and replaced in model context by a typed,
bounded synopsis.

## Filesystem boundary

Every thread receives a separate workspace. Virtual paths are normalized;
traversal and unsafe symlink behavior are rejected before host filesystem
access.

Existing files must be read at their current content revision before
`write_file` or `str_replace`. A matching revision is single-use, and same-path
mutations are serialized across lead and child agents. This prevents stale
model context and concurrent agents from silently clobbering changes.

Uploads are immutable user input. Optional document conversion is disabled by
default, runs without a shell, and has independent input, output, and timeout
limits. Artifact downloads disable MIME sniffing and force active content such
as HTML and SVG to download.

## Network boundary

Browser navigation, browser subrequests, web search endpoints, fetched URLs,
webhook callbacks, and HTTP MCP servers use explicit outbound policy.

Private and reserved addresses are blocked by default. Gofer validates URL
shape, every DNS answer, redirects, and the address used for dialing to reduce
SSRF and DNS-rebinding risk. Enable private-address access only for a narrowly
defined trusted integration.

Browser sessions are thread-scoped, bounded, and expire after inactivity.
Remote Chrome endpoints are trusted infrastructure and are not protected from
the Gofer process itself.

## Prompt-injection boundary

Prompt injection is handled as untrusted-data flow, not as a promise that a
model cannot be influenced.

- genuine user content is temporarily wrapped with explicit boundaries;
- reserved authority markers in untrusted content are neutralized;
- browser, web, and MCP results are classified by tool metadata and sanitized
  before output budgeting and persistence;
- durable user text remains unchanged for auditability;
- local file and shell output remains byte-preserved but still passes tool and
  policy boundaries before it can cause another side effect.

Tool loop limits, provider transcript repair, terminal-response checks, and
length/safety stop handling prevent malformed or truncated model output from
executing hidden tool intent.

## Authentication and tenant isolation

Bearer authentication is optional for local loopback use and should be enabled
for any shared listener. Configured plaintext tokens are hashed with SHA-256 in
memory; credentials must contain at least 24 characters. `/healthz` and
`/metrics` remain outside bearer middleware and must be protected at the
listener, firewall, or reverse proxy when their output should not be public.

Permissions are checked before side effects. Threads reserve internal owner
metadata that clients cannot set. Runs, files, artifacts, memory, feedback,
schedules, and channel bindings verify that owner at every resource boundary.
Unknown protected routes require administrative access rather than falling
through permissively.

Authentication does not provide TLS. Terminate HTTPS at a trusted reverse
proxy and protect traffic between the proxy, Gofer, PostgreSQL, model providers,
MCP servers, Chrome, and sandbox infrastructure.

## Secrets

- Keep secrets in environment variables or a secret manager.
- Never commit expanded configuration files.
- Do not pass credentials through model-visible tool arguments.
- Restrict process, container, database, and workspace access at the OS level.
- Rotate provider, channel, MCP, and bearer credentials after suspected
  exposure.
- Review skills and MCP server definitions before enabling them.

Gofer avoids logging or returning configured model and provider credentials,
but operators remain responsible for log collection, process inspection, core
dumps, and infrastructure-level secret exposure.

## Production checklist

1. Bind to a private interface or place Gofer behind authenticated HTTPS;
   explicitly restrict `/healthz` and `/metrics` if required.
2. Enable bearer authentication with least-privilege principals.
3. Keep host execution disabled and use a reviewed sandbox image.
4. Keep sandbox network disabled unless the task explicitly needs it.
5. Leave private-address browsing and fetching disabled by default.
6. Configure channel allowlists, signatures, and one-time binding codes.
7. Back up and access-control both SQL state and workspace files.
8. Monitor `/metrics`, failed runs, sandbox capacity, and channel retries.
9. Run supported releases and review migration notes before upgrading.
10. Test incident recovery and credential rotation before production use.

Report vulnerabilities privately according to [SECURITY.md](../SECURITY.md).
