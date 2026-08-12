# Read Before Write

Long-running agents often fail by appending the same section repeatedly or by
overwriting work they have not inspected. Gofer enables a deterministic file
version gate by default: modifying an existing file requires a `read_file` of
that file's current version earlier in the model-visible conversation.

```yaml
read_before_write:
  enabled: true
```

The gate applies to `write_file` in overwrite or append mode and to
`str_replace`. Creating a missing file is unaffected. A blocked call returns a
normal tool error with `code: read_before_write` and `recoverable: true`, so the
run continues and the model can read the file before retrying.

## Revision Contract

Every successful `read_file` result includes a `sha256:` revision of the
complete bounded file contents. A ranged read still carries the full-file
revision even though only the selected lines enter model context. The runtime
reconstructs marks from the exact tool-call/result history supplied to each
model turn.

A current mark authorizes one successful modification. The write consumes it,
so consecutive edits require consecutive reads. A mark is rejected when:

- the file changed after it was read, including through a shell command;
- an earlier successful write already consumed it;
- conversation compaction removed the read result;
- the read failed, its result was malformed, or its revision was invalid.

`read_file` must stay exempt from tool-output externalization while the gate is
enabled, because the revision is part of its structured result. Gofer validates
this cross-setting invariant at startup.

## Concurrency and Failure Behavior

The gate wraps actual tool execution rather than performing a detached policy
check. It holds a context-aware lock for the normalized thread-and-path scope
across revision comparison and the write itself. Two concurrent parent or
child agents may both hold the old revision, but after one changes the file the
other observes a mismatch and is blocked. Within one agent, a successful write
also consumes the in-memory mark before releasing the path lock.

Missing files pass through so `write_file` can create them and `str_replace`
can return its native not-found error. If the gate cannot inspect a file—for
example because it is binary, too large for the configured read boundary, or
the workspace is unavailable—it fails open and lets the underlying tool report
the authoritative outcome. Invalid paths and arguments likewise remain the
tool registry's responsibility.

The feature is reported by `GET /api/features` as
`read_before_write.enabled`. Disabling it removes the interceptor from both the
lead-agent and subagent runtime chains.
