# DeerFlow compatibility

Gofer implements DeerFlow's backend concepts through Go-native interfaces and
state machines. Compatibility means equivalent durable behavior and compatible
HTTP/SSE shapes where clients depend on them; it does not mean source-level
compatibility with DeerFlow's Python packages.

| Capability group | Gofer implementation |
| --- | --- |
| Durable agent runtime | Event-journaled runs, replay, cancellation, resumable SSE, checkpoints, context compaction, and terminal recovery. |
| Models | Normalized streaming OpenAI Chat Completions and native Anthropic Messages adapters with tool calls, reasoning, usage, length, and safety handling. |
| Tools and execution | Typed registry, policy middleware, workspace and artifact tools, MCP, skills, browser, web research, local/Docker sandboxes, output budgeting, and read-before-write. |
| Coordination | Parallel bounded subagents, goals, todos, scoped memory, human input, scheduling, and run/task control. |
| Conversation APIs | DeerFlow/LangGraph-shaped threads, runs, state, messages, streaming/wait/join, branches, feedback, uploads, outputs, models, assistants, and suggestions. |
| Channels | Signed generic webhook, Slack, Telegram, Discord, Feishu/Lark, DingTalk, WeCom, WeChat, GitHub automation, and Buzz/Nostr with durable binding and command behavior. |
| Operations | SQLite/PostgreSQL, RBAC bearer auth, metrics, graceful shutdown, CI quality gates, cross-platform archives, and multi-architecture containers. |

Gofer deliberately does not embed DeerFlow's Next.js interface, Python class
extension ABI, or vendor-hosted sandbox implementations such as E2B and
BoxLite. Web clients use the HTTP/SSE contracts, Python-only extensions use MCP
or external services, and deployment-specific execution backends implement the
Go sandbox interface. Those boundaries keep the service a single inspectable Go
binary without weakening its protocol or durability guarantees.

The reference point for the completed baseline is DeerFlow `88252e9b`. Later
upstream additions are evaluated as new compatibility work rather than silently
changing this contract.
