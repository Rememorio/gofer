#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"

command -v golangci-lint >/dev/null 2>&1 || {
  printf 'complexity: golangci-lint is required\n' >&2
  exit 2
}

cd "${repo_root}"
golangci-lint config verify --config .golangci-complexity.yml
golangci-lint run --config .golangci-complexity.yml ./...
printf 'complexity: no function exceeds the configured thresholds\n'
