# Service control plane

## Authentication and authorization

Gofer's bearer authenticator hashes configured opaque tokens with SHA-256 and
retains only their digests. Credentials must contain at least 24 characters.
An authenticated principal receives explicit `threads:*`, `runs:*`,
`channels:*`, and service-specific permissions, or the administrative
capability. Permission and token slices are copied at every boundary.

The HTTP middleware is fail-closed: health is the only public gateway route;
unknown paths require administrative access. Authentication and authorization
failures use distinct 401 and 403 responses. Authentication is optional in the
baseline local configuration, but an enabled configuration without credentials
is rejected during startup validation.

## Scheduler

Scheduled tasks support one RFC3339 timestamp or a standard five-field cron
expression in an IANA timezone. A store contract owns task state and atomic
leases. The in-memory reference store is concurrency-safe, while SQLite and
PostgreSQL persist the same task definitions, dispatch history, and
compare-and-claim leases across restarts.

Workers query bounded due batches and claim with an owner and expiry. Active
leases prevent overlap, while expired `running` tasks are eligible for recovery.
One-shot tasks end as completed or failed. Cron tasks record the outcome and
advance to the first occurrence after dispatch time, avoiding unbounded catch-up
bursts after downtime.

The authenticated HTTP API scopes every task to its bearer principal and
supports create, list, read, partial update, delete, pause, resume, and manual
trigger operations under `/api/scheduled-tasks`. Read operations require
`scheduled:read`; mutations require `scheduled:write`. A manual trigger uses
the same lease path as background polling. The first dispatch can create a
dedicated thread, which is then retained for later cron occurrences.

## Channels

Provider adapters normalize inbound text and attachment metadata before calling
the channel manager. The manager resolves an active external binding, reuses a
durable binding/chat/topic thread mapping, enforces a bounded ingress queue and
global in-flight limit, serializes turns within one conversation, and atomically
deduplicates provider event IDs. Failed authentication, dispatch, or send
attempts release their claim so a provider retry can recover; successful IDs
remain until the configured TTL. SQL deployments preserve all three state
classes across restarts.

The built-in generic adapter verifies timestamped HMAC-SHA256 webhooks and signs
outbound callbacks. Provider-specific SDKs implement the same small `Sender`
contract. This keeps Slack, Telegram, Feishu, and other transports outside the
agent runtime while giving them one ownership, concurrency, and idempotency
policy. The complete wire contract and connection API are documented in
[Channels](channels.md).

## Extensions and metrics

Extensions publish validated manifests with dependencies and capabilities.
Startup uses deterministic topological order. Any failure closes already-started
dependencies in reverse order, and normal shutdown follows the same ordering.

Metrics must be declared before use with a finite allowed value set for every
label. A global series limit prevents user or thread identifiers from producing
unbounded cardinality. Counters and fixed-bucket histograms can be snapshotted or
rendered directly in Prometheus text format without another runtime dependency.
