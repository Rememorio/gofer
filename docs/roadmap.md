# Roadmap

Gofer is implemented in vertical, independently validated modules. Each major
module is committed and pushed separately.

1. **Foundation:** CLI, build metadata, documentation, CI, lint, complexity,
   coverage, race, security, and portability gates.
2. **Durable core:** configuration, domain identifiers, typed events, threads,
   runs, checkpoints, and an in-memory reference store.
3. **Agent loop:** normalized model API, streaming, middleware, tool calls,
   input/result guardrails, loop detection, interrupted-history repair,
   terminal-response recovery, budgets, cancellation, and deterministic replay.
4. **Tools and extensions:** built-ins, MCP client integration, skills, file
   workspace, read-before-write version gates, artifacts, and policy enforcement.
5. **Execution:** local and container sandboxes with resource and path limits.
6. **Coordination:** sub-agents, task limits, goals, todos, summarization, and
   long-term memory.
7. **Application API:** DeerFlow-compatible thread/run REST endpoints, SSE,
   uploads, artifacts, models, skills, and configuration.
8. **Platform:** SQLite/PostgreSQL durability, scheduling, channels,
   authentication, authorization, observability, and operational tooling.
9. **Release closure:** end-to-end contract suites, Docker images, deployment
   examples, migration notes, and reproducible releases.
