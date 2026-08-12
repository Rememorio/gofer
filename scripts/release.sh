#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/release.sh --version VERSION [options]

Build cross-platform Gofer archives and SHA256SUMS from the checked-out commit.

Options:
  --version VERSION  Semantic version without a leading v (required).
  --commit REF       Commit to build. Defaults to HEAD.
  --output-dir DIR   Artifact directory. Defaults to dist.
  -h, --help         Show this help text.
EOF
}

die() {
  printf 'release: %s\n' "$*" >&2
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
version=""
commit="HEAD"
output_dir="${repo_root}/dist"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      shift
      [ "$#" -gt 0 ] || die "--version requires a value"
      version="$1"
      ;;
    --version=*) version="${1#--version=}" ;;
    --commit)
      shift
      [ "$#" -gt 0 ] || die "--commit requires a ref"
      commit="$1"
      ;;
    --commit=*) commit="${1#--commit=}" ;;
    --output-dir)
      shift
      [ "$#" -gt 0 ] || die "--output-dir requires a directory"
      output_dir="$1"
      ;;
    --output-dir=*) output_dir="${1#--output-dir=}" ;;
    -h|--help)
      usage
      exit 0
      ;;
    *) die "unknown argument: $1" ;;
  esac
  shift
done

[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] ||
  die "--version must be a semantic version without a leading v"

for executable in git go tar zip; do
  command -v "${executable}" >/dev/null 2>&1 || die "${executable} is required"
done

cd "${repo_root}"
[ -z "$(git status --porcelain)" ] || die "the working tree must be clean before building a release"

commit_sha="$(git rev-parse "${commit}^{commit}")" || die "cannot resolve commit ${commit}"
head_sha="$(git rev-parse HEAD)"
[ "${commit_sha}" = "${head_sha}" ] || die "release commit must match the checked-out HEAD"

mkdir -p "${output_dir}"
output_dir="$(cd -- "${output_dir}" && pwd)"
[ -z "$(find "${output_dir}" -mindepth 1 -maxdepth 1 -print -quit)" ] ||
  die "output directory must be empty: ${output_dir}"

staging_dir="$(mktemp -d)"
trap 'rm -rf "${staging_dir}"' EXIT

build_date="$(git show -s --format=%cI "${commit_sha}")"
source_epoch="$(git show -s --format=%ct "${commit_sha}")"
host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
ldflags="-s -w -buildid="
ldflags+=" -X github.com/Rememorio/gofer/internal/buildinfo.version=${version}"
ldflags+=" -X github.com/Rememorio/gofer/internal/buildinfo.commit=${commit_sha}"
ldflags+=" -X github.com/Rememorio/gofer/internal/buildinfo.date=${build_date}"
targets=(darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64)
artifacts=()

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  package_name="gofer_${version}_${goos}_${goarch}"
  package_dir="${staging_dir}/${package_name}"
  binary_name="gofer"
  archive_name="${package_name}.tar.gz"
  if [ "${goos}" = "windows" ]; then
    binary_name="gofer.exe"
    archive_name="${package_name}.zip"
  fi

  mkdir -p "${package_dir}"
  printf 'Building %s/%s\n' "${goos}" "${goarch}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -buildvcs=false -trimpath -ldflags "${ldflags}" \
    -o "${package_dir}/${binary_name}" .
  cp README.md LICENSE config.example.yaml "${package_dir}/"

  if touch -d "@${source_epoch}" "${package_dir}" "${package_dir}"/* 2>/dev/null; then
    :
  fi
  if [ "${goos}" = "${host_os}" ] && [ "${goarch}" = "${host_arch}" ]; then
    version_json="$("${package_dir}/${binary_name}" version --json)"
    printf '%s\n' "${version_json}" | grep -Fq '"version": "'"${version}"'"' ||
      die "native artifact reports an unexpected version"
    printf '%s\n' "${version_json}" | grep -Fq '"commit": "'"${commit_sha}"'"' ||
      die "native artifact reports an unexpected commit"
  fi

  if [ "${goos}" = "windows" ]; then
    (
      cd "${staging_dir}"
      find "${package_name}" -type f -print | LC_ALL=C sort | zip -q -X "${output_dir}/${archive_name}" -@
    )
  elif tar --version 2>/dev/null | grep -q 'GNU tar'; then
    tar --sort=name --mtime="@${source_epoch}" --owner=0 --group=0 --numeric-owner \
      -C "${staging_dir}" -czf "${output_dir}/${archive_name}" "${package_name}"
  else
    tar -C "${staging_dir}" -czf "${output_dir}/${archive_name}" "${package_name}"
  fi
  artifacts+=("${archive_name}")
done

(
  cd "${output_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${artifacts[@]}" >SHA256SUMS
  else
    shasum -a 256 "${artifacts[@]}" >SHA256SUMS
  fi
)

printf 'Release artifacts written to %s\n' "${output_dir}"
