#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

cd "${repo_root}"
go test ./...
go vet ./...
go build -trimpath -o "${tmp_dir}/gofer" .
"${tmp_dir}/gofer" version --json >"${tmp_dir}/version.json"
grep -q '"version"' "${tmp_dir}/version.json"
scripts/coverage.sh
