# Prompt-Injection Guardrails

Gofer treats user text and content returned from remote systems as data, never
as framework authority. An always-on runtime middleware applies the same
structural defenses used by DeerFlow while keeping the durable conversation
faithful to what the user submitted.

## Trust Boundaries

| Source | Model-facing transformation | Durable representation |
| --- | --- | --- |
| Current user text | Escape reserved framework tags, neutralize forged boundary tokens, then add explicit user-input boundaries | Original text |
| First-party web results | Recursively neutralize JSON object keys and string values | Sanitized or budgeted result |
| Browser snapshots and actions | Recursively neutralize JSON object keys and string values | Sanitized or budgeted result |
| MCP tool results | Recursively neutralize JSON object keys and string values | Sanitized or budgeted result |
| Local file, shell, memory, and control tools | No content rewrite | Original or budgeted result |

Reserved tags include Gofer and DeerFlow authority blocks such as `<system>`,
`<instruction>`, `<memory>`, `<system-reminder>`, `<conversation_summary>`, and
`<browser_snapshot>`. Only the reserved finite set is escaped; ordinary HTML
such as `<article>` remains readable. Literal `--- BEGIN USER INPUT ---` and
`--- END USER INPUT ---` tokens inside untrusted text become inert bracketed
forms.

Input transformation is temporary and idempotent. The runtime clones the
model-facing message content, preserves non-text blocks in multimodal messages,
and never writes wrapped text back to the thread journal. This keeps branching,
feedback, regeneration, and UI history tied to the genuine input.

## Tool Result Ordering

Tool definitions carry an internal `UntrustedOutput` classification. Browser
snapshot-producing tools set it explicitly, and every discovered MCP tool is
classified as untrusted because its server and downstream data are outside the
local runtime boundary. First-party web tool names remain covered for DeerFlow
compatibility.

The runtime applies result boundaries in this order:

1. Raw-result observers receive an isolated copy for host bookkeeping.
2. Remote-result guardrails recursively sanitize JSON keys and string values.
3. The output budget may externalize or summarize the sanitized result.
4. Only the transformed result reaches events, conversation history, and the
   next model request.

JSON traversal is bounded. Malformed JSON, excessive nesting, or two object
keys that collide after sanitization fail closed instead of silently losing
data. Historical remote results are sanitized again at the model boundary as a
compatibility defense for journals created before this guardrail existed.

## Scope

These guardrails remove structural tokens that imitate trusted runtime blocks;
they do not attempt to detect every natural-language prompt injection. Tool
authorization, sandboxing, network policy, workspace isolation, and artifact
delivery checks remain independent enforcement layers. Applications should
still grant the model only the tools and credentials required for its task.
