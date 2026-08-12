# Channels

Gofer's channel subsystem turns external messaging events into ordinary durable
agent runs. The core is provider-neutral: adapters authenticate provider input,
normalize it into a `channel.Message`, and register a small outbound `Sender`.
Identity, conversation, concurrency, and retry semantics remain identical for
every provider.

## Durable model

An active binding assigns the tuple `(provider, workspace_id,
external_user_id)` to one Gofer user. A different owner cannot claim that
identity until its current owner revokes it. Revocation retains the connection
record; a later owner transfer clears the old conversation mappings so thread
history cannot cross owners.

Conversation mappings are scoped by binding, chat, and optional topic. The
first event creates a normal owner-scoped Gofer thread, and later events reuse
it. Mapping insertion is atomic, so concurrent first messages converge on one
thread. Deleting the thread also removes its channel mapping.

SQLite and PostgreSQL persist bindings, mappings, and delivery claims. The
in-memory store implements the same contracts for ephemeral deployments.
Delivery claims use a TTL and an atomic insert. A successful dispatch retains
the claim until expiry; an authentication, run, or send failure releases it so
a provider retry can recover. This behavior survives process restarts when a
SQL store is configured.

The manager also provides two separate concurrency controls:

- a bounded queue prevents webhook bursts from creating unbounded goroutines;
- a keyed lock serializes turns for one conversation while unrelated chats can
  run concurrently.

## Signed generic webhook

The built-in `webhook` adapter is a complete transport and a reference contract
for future provider SDK adapters. Enable it with an outbound callback and at
least one approved identity:

```yaml
channels:
  enabled: true
  max_inflight: 32
  queue_capacity: 128
  dedupe_ttl_seconds: 86400
  bindings:
    - user_id: local
      provider: webhook
      workspace_id: example
      external_user_id: user-123
  webhook:
    enabled: true
    secret: ${GOFER_CHANNEL_WEBHOOK_SECRET}
    outbound_url: https://messaging.example.com/gofer/replies
    timeout_seconds: 10
    max_attempts: 3
    max_body_bytes: 1048576
    clock_skew_seconds: 300
    allow_private_addresses: false
```

Inbound events use:

```text
POST /api/channels/webhook/{workspace_id}/events
Content-Type: application/json
X-Gofer-Timestamp: <Unix seconds>
X-Gofer-Signature: sha256=<lowercase hex HMAC>
```

The signature input is the ASCII timestamp, one period, and the exact raw body:

```text
HMAC-SHA256(secret, timestamp + "." + raw_body)
```

Timestamps outside the configured clock-skew window are rejected. Signatures
are compared in constant time, the body is size-limited before decoding, and
unknown JSON fields fail closed. A valid event body has this shape:

```json
{
  "id": "provider-event-id",
  "external_user_id": "user-123",
  "chat_id": "conversation-456",
  "topic_id": "optional-topic-789",
  "text": "Hello",
  "attachments": [
    {
      "name": "report.pdf",
      "media_type": "application/pdf",
      "url": "https://messaging.example.com/files/report.pdf",
      "size": 12345
    }
  ],
  "metadata": {
    "event_type": "message"
  }
}
```

Acceptance returns HTTP 202 after the event enters the bounded queue. A full
queue returns HTTP 503 with `Retry-After: 1`. Authentication and structural
failures return 401 and 400 respectively.

The outbound callback receives a normalized `channel.Reply` JSON document. It
uses the same timestamp and signature headers, and `Idempotency-Key` contains
the inbound event ID. Network errors, HTTP 429, and 5xx responses receive
bounded exponential retries. Other 4xx responses are permanent. Callback
requests use the shared DNS-aware network guard; private and loopback addresses
require an explicit opt-in.

Generic attachment URLs are recorded as untrusted message context and are not
downloaded by the core. Provider adapters can instead authenticate, download,
and materialize attachments through Gofer's upload boundary before dispatch.

## Connection API

When channels are enabled, authenticated clients can inspect and manage their
own bindings:

```text
GET    /api/channels
GET    /api/channel-connections
POST   /api/channel-connections
DELETE /api/channel-connections/{connection_id}
```

Status and list operations require `channels:read`; mutations require
`channels:write`. The bearer principal is always the binding owner—clients
cannot submit another `user_id`. Operators can also bootstrap bindings in the
configuration file, which is applied idempotently at startup.

Channel replies participate in structured human input. If a run calls
`ask_clarification`, the text renderer sends the question and choices through
the same provider. The next visible message in that mapped conversation resumes
the durable thread in a new run.
