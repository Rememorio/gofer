#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
threshold="${GOFER_COVERAGE_THRESHOLD:-85}"
profile="$(mktemp)"
trap 'rm -f "${profile}"' EXIT

cd "${repo_root}"
go test ./internal/... -covermode=atomic -coverprofile="${profile}"
total="$(go tool cover -func="${profile}" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
awk -v total="${total}" -v threshold="${threshold}" 'BEGIN {
  if (total + 0 < threshold + 0) {
    printf "coverage: %.1f%% is below %.1f%%\n", total, threshold > "/dev/stderr"
    exit 1
  }
  printf "coverage: %.1f%% meets %.1f%% threshold\n", total, threshold
}'
