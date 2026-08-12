# Deployment and releases

Gofer ships as a statically linked binary and as a multi-architecture container
image. Both release paths embed the semantic version, source commit, and commit
timestamp reported by `gofer version --json`.

## Container image

Build the local image with:

```sh
docker build -t gofer:local .
```

The runtime image runs as UID/GID 65532, listens on port 8001, and stores mutable
state below `/var/lib/gofer`. It includes the Docker CLI for deployments that
mount a remote or local Docker engine for Gofer's container sandbox; mounting a
host Docker socket grants host-level control and must be an explicit operator
decision.

The Compose example is read-only apart from a named state volume and a bounded
temporary filesystem:

```sh
cd deploy
cp config.example.yaml config.yaml
export OPENAI_API_KEY=your-key
docker compose up --build
```

Edit `deploy/config.yaml` before production use. Enable bearer authentication,
keep the local sandbox disabled unless the entire service container is trusted,
and prefer Gofer's Docker sandbox for model-requested commands.

## Binary releases

Create deterministic archives from a clean checkout:

```sh
make release VERSION=1.0.0
```

The release script builds CGO-free binaries for Linux, macOS, and Windows on
amd64 and arm64. Archives include the binary, license, README, and example
configuration; `dist/SHA256SUMS` covers every archive. Build metadata is derived
from the selected Git commit, and GNU tar builds normalize order, ownership, and
timestamps for reproducibility.

Pushing a validated `vX.Y.Z` tag runs the release workflow. It publishes the
archives and checksums to a GitHub release, then builds the same source as
linux/amd64 and linux/arm64 images under `ghcr.io/rememorio/gofer`.

## Database upgrades

Back up the database before changing versions. Gofer's SQL bootstrap is
forward-only and idempotent: startup creates missing tables and indexes without
dropping user data. SQLite deployments should stop the service and copy the
database file; PostgreSQL deployments should use their normal consistent backup
mechanism. Do not run an older binary against a database after a newer release
has introduced schema that the older version does not understand.
