# Command Sandbox

Gofer exposes shell access through a bounded `bash` tool. Every command has a
wall-clock timeout, a script-size limit, a shared stdout/stderr memory budget,
process-tree cancellation, stable virtual workspace paths, and request-scoped
environment injection. Injected values are removed from captured output before
it is returned to the model.

## Drivers

The `local` driver runs the configured shell directly on the Gofer host. It is
a workflow boundary, not an operating-system security boundary, and therefore
fails closed unless `allow_host_execution: true` is explicitly configured.
Only a small allowlist of non-secret host environment variables is inherited.

The `docker` driver starts a fresh container for every command. Its baseline is
read-only, drops all Linux capabilities, enables `no-new-privileges`, limits
CPU, memory and processes, mounts uploads and Skills read-only, and disables
networking unless explicitly enabled. Request-scoped environment values are
passed through a mode-`0600` temporary env file rather than command-line
arguments. Containers are force-removed after timeout or cancellation.

```yaml
sandbox:
  driver: docker
  image: ghcr.io/example/gofer-sandbox:latest
  docker_binary: docker
  network_enabled: false
  command_timeout_seconds: 600
  max_timeout_seconds: 3600
  max_output_bytes: 1048576
  max_script_bytes: 65536
  memory: 1g
  cpus: 2
  pids_limit: 256
```

The agent always sees `/mnt/user-data/workspace`,
`/mnt/user-data/uploads`, and `/mnt/user-data/outputs`. Local execution safely
rewrites those roots to the thread's host directories and masks the reverse
mapping in output; Docker mounts them at the virtual paths directly.
