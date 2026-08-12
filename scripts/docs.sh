#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"

command -v go >/dev/null 2>&1 || {
  printf 'docs: go is required\n' >&2
  exit 2
}

cd "${repo_root}"
while IFS= read -r package; do
  go doc "${package}" >/dev/null
done < <(go list ./...)
printf 'docs: every package can be rendered by go doc\n'
