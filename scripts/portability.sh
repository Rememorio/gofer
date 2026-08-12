#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

command -v actionlint >/dev/null 2>&1 || {
  printf 'portability: actionlint is required\n' >&2
  exit 2
}
command -v shellcheck >/dev/null 2>&1 || {
  printf 'portability: shellcheck is required\n' >&2
  exit 2
}

cd "${repo_root}"
actionlint
shellcheck scripts/*.sh

targets=(linux/amd64 linux/arm64 darwin/arm64 windows/amd64)
for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  output="${tmp_dir}/gofer-${goos}-${goarch}"
  if [ "${goos}" = "windows" ]; then
    output="${output}.exe"
  fi
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -o "${output}" .
done
