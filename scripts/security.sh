#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"

command -v govulncheck >/dev/null 2>&1 || {
  printf 'security: govulncheck is required\n' >&2
  exit 2
}

cd "${repo_root}"
govulncheck ./...
