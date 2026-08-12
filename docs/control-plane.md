# Service control plane

## Authentication and authorization

Gofer's bearer authenticator hashes configured opaque tokens with SHA-256 and
retains only their digests. Credentials must contain at least 24 characters.
An authenticated principal receives explicit `threads:*` and `runs:*`
permissions, or the administrative capability. Permission and token slices are
copied at every boundary.

The HTTP middleware is fail-closed: health is the only public gateway route;
unknown paths require administrative access. Authentication and authorization
failures use distinct 401 and 403 responses. Authentication is optional in the
baseline local configuration, but an enabled configuration without credentials
is rejected during startup validation.

## Scheduler

Scheduled tasks support one RFC3339 timestamp or a standard five-field cron
expression in an IANA timezone. A store contract owns task state and atomic
leases. The reference store is concurrency-safe; durable adapters can implement
the same compare-and-claim operations.

Workers query bounded due batches and claim with an owner and expiry. Active
leases prevent overlap, while expired `running` tasks are eligible for recovery.
One-shot tasks end as completed or failed. Cron tasks record the outcome and
advance to the first occurrence after dispatch time, avoiding unbounded catch-up
bursts after downtime.

## Channels

Provider adapters normalize inbound text and attachment metadata before calling
the channel manager. The manager resolves the external provider identity to an
internal user before dispatch, derives a provider/workspace/user/topic thread
key, applies a global in-flight bound, and atomically deduplicates provider event
IDs. Failed authentication, dispatch, or send attempts release their claim so a
provider retry can recover; successful IDs remain until the configured TTL.

Provider-specific SDKs implement the small `Sender` contract. This keeps Slack,
Telegram, Feishu, and other transports outside the agent runtime and gives them
one identity and idempotency policy.

## Extensions and metrics

Extensions publish validated manifests with dependencies and capabilities.
Startup uses deterministic topological order. Any failure closes already-started
dependencies in reverse order, and normal shutdown follows the same ordering.

Metrics must be declared before use with a finite allowed value set for every
label. A global series limit prevents user or thread identifiers from producing
unbounded cardinality. Counters and fixed-bucket histograms can be snapshotted or
rendered directly in Prometheus text format without another runtime dependency.
