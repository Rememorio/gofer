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

## Native providers

Gofer includes direct Slack, Telegram, Discord, Feishu/Lark, and DingTalk
adapters. They implement the
same `Sender` and inbound-source contracts as the generic webhook, so the
manager owns startup, shutdown, queueing, authentication, deduplication, and
conversation serialization uniformly. A provider is fully connected before
startup succeeds. Connections retry with capped exponential backoff and stop
with the service context.

Slack uses Socket Mode and therefore needs no public callback URL. Configure a
bot token and an app-level token with `connections:write`; the app must also
subscribe to the desired message and mention events. Socket envelopes are
acknowledged only after their normalized event enters Gofer's queue. Replies
preserve Slack threads, escape reserved mrkdwn characters, and honor Slack's
message length limit.

```yaml
channels:
  enabled: true
  slack:
    enabled: true
    bot_token: ${SLACK_BOT_TOKEN}
    app_token: ${SLACK_APP_TOKEN}
    allowed_users: [U0123456789]
    request_timeout_seconds: 20
    max_attempts: 3
```

Telegram uses Bot API long polling. Private chats map all messages to one Gofer
thread; group chats use the Telegram topic, replied-to message, or root message
as their topic. The polling offset advances only after queue acceptance, so
backpressure does not silently discard an update. Text is split by Telegram's
UTF-16 length rule, and the first chunk replies to the inbound message.

```yaml
channels:
  enabled: true
  telegram:
    enabled: true
    bot_token: ${TELEGRAM_BOT_TOKEN}
    allowed_users: ["123456789"]
    poll_timeout_seconds: 30
    request_timeout_seconds: 45
    max_attempts: 3
```

Discord uses Gateway v10 with heartbeats, session resume, and the Message
Content intent. `mention_only` filters ordinary guild messages unless their
channel appears in `allowed_channels`; direct messages and established Discord
threads remain routable. With `thread_mode`, a message starts a Discord thread
when the API permits it and Gofer replies there. Outbound mentions are disabled
to prevent model-generated mass pings.

```yaml
channels:
  enabled: true
  discord:
    enabled: true
    bot_token: ${DISCORD_BOT_TOKEN}
    allowed_guilds: ["123456789012345678"]
    allowed_channels: []
    mention_only: true
    thread_mode: true
    request_timeout_seconds: 20
    max_attempts: 3
```

Feishu and international Lark tenants use the official SDK long connection, so
no public callback URL is required. Events are accepted only after the SDK
reports that the connection is ready. Group messages keep their root or thread
identifier; direct messages use one conversation. Replies use idempotency keys
and are split at the platform's UTF-16 text limit. Set `domain` to
`https://open.larksuite.com` only for an international Lark tenant.

```yaml
channels:
  enabled: true
  feishu:
    enabled: true
    app_id: ${FEISHU_APP_ID}
    app_secret: ${FEISHU_APP_SECRET}
    domain: https://open.feishu.cn
    allowed_users: [ou_example]
    request_timeout_seconds: 20
    max_attempts: 3
```

DingTalk uses Stream Mode for inbound events and OpenAPI for outbound Markdown
messages. Gofer retains only a bounded, expiring reply route for each accepted
message, refreshes access tokens before expiry, and retries once a stale token
has been invalidated. Direct messages route to the sender; group messages route
to their open conversation ID.

```yaml
channels:
  enabled: true
  dingtalk:
    enabled: true
    client_id: ${DINGTALK_CLIENT_ID}
    client_secret: ${DINGTALK_CLIENT_SECRET}
    allowed_users: [manager123]
    request_timeout_seconds: 30
    max_attempts: 3
```

`allowed_users` and `allowed_guilds` are transport-level allowlists; an empty
list allows the provider event to reach Gofer's binding resolver. An active
binding is still required for the external user, so an allowlist never grants
access to another Gofer account. Provider attachment references are normalized
as untrusted manifests. This release does not download them or upload reply
files; content crosses the workspace boundary only through Gofer's existing
upload and document-ingestion APIs.

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
