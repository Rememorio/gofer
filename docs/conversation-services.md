# Conversation services

Gofer provides three bounded user-interface services around the main agent
loop: automatic titles, pre-send input polishing, and follow-up suggestions.
They use the same validated model aliases and provider adapters as agent runs,
but never receive tools and cannot execute side effects.

## Automatic titles

When a thread has no title, Gofer assigns one after its first user turn. The
default is a deterministic local title derived from the user text, bounded by
`title.max_words` and `title.max_chars`. If `title.model_name` names a
configured model alias, a short tool-free model call can produce a better
title; an error, safety stop, empty response, or interrupted first run falls
back to the local title.

The store operation is conditional and atomic: it writes only while the title
is still empty. A user rename that wins the race is preserved, and later runs
do not spend another title model call for an already named thread. SQLite,
PostgreSQL, and the in-memory store expose the same contract.

## Input polishing

`POST /api/input-polish` accepts `text`, optional `locale`, and optional
`thread_id`, then returns:

```json
{"rewritten_text":"...","changed":true}
```

The endpoint trims and bounds the draft, asks the selected model to rewrite
rather than answer it, and preserves an initial slash command if the model
drops it. The operation does not create a run or persist a message. Disabled
input polishing returns `404`; provider failures return `503`. With bearer
authentication it requires `runs:create`.

## Follow-up suggestions

Clients can discover configuration and request suggestions at:

```text
GET  /api/suggestions/config
POST /api/threads/{thread_id}/suggestions
```

The request accepts up to 20 recent `{role, content}` messages, `n` from 1 to
5, and an optional configured `model_name`. The requested count is capped by
`suggestions.max_suggestions`. Conversation text is serialized as untrusted
JSON data under a fixed system instruction. Only a JSON string array is
accepted; blank and duplicate entries are removed and individual suggestions
are bounded. Model or parse failures intentionally return an empty array so a
decorative UI feature cannot break the conversation.

Suggestion requests are owner-scoped and require `threads:read`; configuration
discovery requires `resources:read`.
