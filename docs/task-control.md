# Long-running task control

Gofer models long-running work with four separate contracts that compose at
runtime: goal state, child execution, long-term memory, and context compaction.
None relies on process-global mutable state, and every storage boundary can be
replaced by a durable adapter.

## Goals and todos

A thread has at most one active goal. Creating a goal is explicit and may
include a token budget. The goal can finish as `complete` or `blocked`; a goal
cannot be completed while a todo remains unfinished. Todo replacement is
atomic, keeps stable ordering, and permits at most one `in_progress` item.

The control store uses compare-and-swap versions. Concurrent writers either
apply to the latest snapshot or receive a conflict after bounded retries,
which prevents lost plan updates. `control_read`, `goal_create`, `goal_update`,
and `todo_write` expose the same state machine to an agent. The service selects
a durable SQLite/PostgreSQL adapter automatically and exposes the same state at
the thread goal, control, and todo HTTP endpoints. Goal replacement or clearing
through HTTP is rejected while a run is active.

## Child agents

The subagent manager enforces maximum depth, total children, and simultaneous
executions independently. Spawn is asynchronous; get, list, wait, and cancel
are explicit operations. Each task owns a cancellation function, all goroutines
belong to the manager, and close waits for executor shutdown.

Queued, running, and terminal transitions from every child enter one monotonic
event sequence. This makes parallel work observable without allowing children
to write directly into the parent conversation.

## Memory

Memory entries always require a user scope and may refine it with thread and
agent identifiers. Retrieval includes user-wide entries plus matching narrower
entries, but never data from another user or a different refinement. Results
are bounded, deterministic, expiration-aware, and ranked by query terms and
tags. Recalled text is inserted as explicitly untrusted context data.

## Context compaction

Compaction activates only after an estimated prompt budget is exceeded. It
retains recent messages and moves the boundary backward when necessary to keep
an assistant tool call together with all following tool results. The discarded
prefix is passed through a replaceable summarizer and reintroduced as a marked
system summary. This package owns policy and message integrity; a model adapter
can supply a provider-specific token estimator and summarizer.
