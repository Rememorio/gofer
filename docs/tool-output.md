# Tool output budgets

Tool calls can return enough text to consume a model context window in one
step. Gofer applies a result budget immediately after tool execution and before
the result enters the durable event journal or the next model request.

```text
tool result
    |
    +--> raw observers (artifact delivery facts)
    |
    +--> remote-content guardrail (when classified as untrusted)
    |
    +--> budget transform
            | small: unchanged
            | large: complete result -> .tool-results
            |        model result   -> typed synopsis + raw sample + read_file path
            ` storage failure: bounded head + tail
    |
    `--> durable tool event and model context
```

The synopsis is deterministic and does not call a model. It recognizes JSON,
XML, CSV/TSV, YAML, source code, and plain text, and extracts bounded structural
signals such as keys, columns, declarations, line counts, and warning/error
counts. Inputs above the synopsis parser limit receive a raw bounded sample so
parsing itself cannot become an amplification path.

## Configuration

The defaults match DeerFlow's result-budget behavior:

```yaml
tool_output:
  enabled: true
  externalize_min_chars: 12000
  preview_head_chars: 2000
  preview_tail_chars: 1000
  fallback_max_chars: 30000
  fallback_head_chars: 8000
  fallback_tail_chars: 3000
  storage_subdir: .tool-results
  exempt_tools: [read_file, read_file_tool]
  tool_overrides: {}
```

Limits count Unicode characters. A result triggers externalization only when
it is strictly larger than its global or per-tool threshold. Setting an
externalization threshold to `0` disables externalization for that scope;
fallback truncation remains active when `fallback_max_chars` is positive.
Setting the fallback maximum to `0` disables fallback truncation.

`storage_subdir` must be one directory name. Gofer excludes both the default
and a configured custom name from workspace snapshots and terminal artifact
verification. Spill files therefore remain implementation feedback, not
deliverables. They are created atomically with private file permissions and
never overwrite an existing path. Workspace discovery and artifact APIs also
hide and reject these paths; an agent can only inspect a spill file by using
the exact `read_file` path supplied in its synopsis.

When the workspace cannot persist a complete result, including when the result
exceeds the configured workspace output size, Gofer retains a head-and-tail
sample whose total length never exceeds `fallback_max_chars`. Historical tool
results from older runs receive the same fallback guard before model calls.
The `read_file` exemption prevents a complete spilled result from being
immediately externalized again when an agent inspects it. It also preserves the
full-content revision used by the default read-before-write gate. Configuration
validation therefore requires `read_file` in `exempt_tools` whenever both the
budget and that gate are enabled.
