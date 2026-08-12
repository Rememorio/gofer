# Uploads and document ingestion

Gofer stores uploads below an isolated thread workspace and exposes only stable
virtual paths such as `/mnt/user-data/uploads/report.pdf`. Multipart requests
are bounded independently by file count, per-file size, and aggregate size.
Every original is written under a collision-free basename; a rejected request
removes every file it wrote, so callers never observe a partial batch.

## Document conversion

Office and PDF conversion is opt-in:

```yaml
uploads:
  max_files: 10
  max_total_bytes: 104857600
  auto_convert_documents: false
  converter_command: [markitdown, "{input}"]
  conversion_timeout_seconds: 120
  max_converted_bytes: 1048576
  max_context_files: 10
  max_context_chars: 50000
  max_outline_entries: 50
  max_preview_lines: 5
```

When enabled, the command is executed directly as an argv array; Gofer never
passes it through a shell. Exactly one argument must contain `{input}`. The
converter receives a private temporary copy retaining the original extension
and must write UTF-8 Markdown to stdout. Execution has a hard deadline, stdout
is bounded while it is produced, and the child receives a minimal environment
without model credentials or other service secrets. A conversion failure is
fail-soft: the original upload remains available and its response simply omits
the Markdown fields.

The converter is a privileged host-side document parser and may itself contain
format vulnerabilities. Automatic conversion therefore defaults to disabled.
Operators should use a trusted, patched converter and an operating-system or
container boundary appropriate to their deployment.

Converted Markdown is stored at a deterministic protected path below
`/mnt/user-data/uploads/.gofer-converted`. Keeping derived data separate means
that `report.pdf` can never overwrite a user-provided `report.md`. Deleting an
original also deletes its companion. Upload list and response records expose
`markdown_file`, `markdown_virtual_path`, and `markdown_artifact_url` when the
companion exists.

## Agent context and discovery

A client associates newly uploaded files with the next user message through
DeerFlow-compatible metadata:

```json
{
  "role": "user",
  "content": "Compare the attached reports.",
  "additional_kwargs": {
    "files": [
      {"filename": "report.pdf", "size": 42000, "status": "uploaded"}
    ]
  }
}
```

Gofer treats this metadata as an untrusted hint. It verifies every basename
against the current thread workspace, uses actual filesystem metadata, removes
duplicates, and ignores missing or unsafe entries. A bounded
`<current_uploads>` section is added only to model requests; the durable user
message remains byte-for-byte unchanged. Document headings and previews are
individually truncated, neutralized as untrusted data, and subject to one
overall `max_context_chars` limit before inclusion.

The `list_uploaded_files` tool discovers earlier uploads and optionally returns
outlines:

```json
{"max_results": 20, "include_outline": true}
```

`include_outline` may instead be a list of selected filenames. Standard
Markdown headings, structural bold headings, and split-bold numbered headings
are recognized. Results are marked as untrusted tool output so document text
passes through the same structural guardrail before another model turn.

## HTTP resources

```text
POST   /api/threads/{thread_id}/uploads
GET    /api/threads/{thread_id}/uploads/limits
GET    /api/threads/{thread_id}/uploads/list
DELETE /api/threads/{thread_id}/uploads/{filename}
GET    /api/threads/{thread_id}/artifacts/{virtual_path}
```

Artifact delivery remains range-capable and applies download hardening to
active content. With authentication enabled, reads require `resources:read`
and mutations require `resources:write`; every lookup also verifies thread
ownership.
