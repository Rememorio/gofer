# Roadmap

Gofer was implemented in vertical, independently validated modules. The
planned Go-native core and platform baseline is complete; future entries track
new upstream behavior rather than unfinished foundation work.

1. **Foundation (complete):** CLI, build metadata, documentation, CI, lint, complexity,
   coverage, race, security, and portability gates.
2. **Durable core (complete):** configuration, domain identifiers, typed events, threads,
   runs, checkpoints, and an in-memory reference store.
3. **Agent loop (complete):** normalized model API, streaming, middleware, tool calls,
   OpenAI-compatible and native Anthropic providers, input/result guardrails,
   loop detection, interrupted-history repair, terminal-response, model-length,
   and safety-finish recovery, budgets, cancellation, and deterministic replay.
4. **Tools and extensions (complete):** built-ins, MCP client integration, skills, file
   workspace, read-before-write version gates, artifacts, and policy enforcement.
5. **Execution (complete):** local and container sandboxes with resource and path limits.
6. **Coordination (complete):** sub-agents, task limits, goals, todos, summarization, and
   long-term memory.
7. **Application API (complete):** DeerFlow-compatible thread/run REST endpoints, SSE,
   structured human input, uploads, artifacts, models, skills, and configuration.
8. **Platform (complete):** SQLite/PostgreSQL durability, scheduling, channels,
   authentication, authorization, observability, and operational tooling.
9. **Release closure (complete):** end-to-end contract suites, Docker images, deployment
   examples, migration notes, and reproducible releases.
