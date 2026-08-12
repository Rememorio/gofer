#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"

command -v go >/dev/null 2>&1 || {
  printf 'quality: go is required\n' >&2
  exit 2
}
command -v golangci-lint >/dev/null 2>&1 || {
  printf 'quality: golangci-lint is required\n' >&2
  exit 2
}

cd "${repo_root}"
golangci-lint config verify

format_diff="$(golangci-lint fmt --diff)"
if [ -n "${format_diff}" ]; then
  printf '%s\n' "${format_diff}"
  printf 'quality: Go source files are not formatted\n' >&2
  exit 1
fi

go mod tidy -diff
golangci-lint run ./...
scripts/docs.sh
scripts/complexity.sh
