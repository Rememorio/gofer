# Tool Loop Detection

An agent can remain below its overall turn limit while repeatedly issuing the
same ineffective operation. Gofer enables a per-run loop detector by default
so this failure mode has an early warning and a deterministic terminal bound.

```yaml
loop_detection:
  enabled: true
  warn_threshold: 3
  hard_limit: 5
  window_size: 20
  tool_freq_warn: 30
  tool_freq_hard_limit: 50
  tool_freq_overrides:
    browser_snapshot:
      warn: 12
      hard_limit: 20
```

## Detection Layers

The repetition layer hashes each model response's complete tool-call set. Tool
call IDs and ordering are ignored, JSON arguments are canonicalized, and call
multiplicity is preserved. This identifies semantically equivalent parallel
calls without treating provider-generated IDs as meaningful differences.

Signatures use salient resource fields for general tools. `read_file` line
ranges are grouped into 200-line buckets so small pagination changes still
reveal a loop. Mutating calls such as `write_file` and `str_replace` remain
content-sensitive to avoid blocking genuine iterative edits.

The frequency layer counts tool names in a separate bounded window. It catches
loops whose arguments continually change, and accepts stricter or looser
thresholds for selected tools through `tool_freq_overrides`.

## Warning and Hard Limit

At a warning threshold, the detector queues a framework-generated user message
for the next model request. It is appended after the completed tool results,
is tagged as internal loop context, and is never added to durable conversation
history. A repeated signature is warned once while it remains in the sliding
window; it may be warned again after naturally leaving that window.

At a hard threshold, the current model response is converted into a terminal
response before assistant journaling and tool execution. Its tool calls are
removed, provider usage is preserved, and a short forced-stop explanation is
recorded. This ordering guarantees that Gofer never persists an assistant tool
call without a matching result. The terminal run event reports
`stop_reason: loop_capped`; delegated agents expose the same reason in their
result metadata.

All detector state belongs to one lead or child run and is bounded by the
configured windows. It is neither durable nor shared between runs, preventing
an earlier task from creating false positives in a later one. The feature is
reported by `GET /api/features` as `loop_detection.enabled`.
