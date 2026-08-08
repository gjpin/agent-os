#!/bin/bash
# Bash 3.2-compatible installer for verified agent-os release binaries.
set -eu

umask 077

repository="${AGENT_OS_RELEASE_REPOSITORY:-zero/agent-os}"
version="${1:-${AGENT_OS_VERSION:-latest}}"
install_dir="${AGENT_OS_INSTALL_DIR:-${HOME}/.local/bin}"

case "$(uname -s)" in
  Linux) os="linux";;
  Darwin) os="darwin";;
  *) echo "agent-os: unsupported operating system" >&2; exit 1;;
esac

case "$(uname -m)" in
  x86_64|amd64)
    arch="amd64"
    ;;
  arm64|aarch64)
    arch="arm64"
    ;;
  *) echo "agent-os: unsupported architecture" >&2; exit 1;;
esac

if [ "$os" = "linux" ] && [ "$arch" != "amd64" ]; then
  echo "agent-os: only linux/amd64 releases are published" >&2
  exit 1
fi
if [ "$os" = "darwin" ] && [ "$arch" != "arm64" ]; then
  echo "agent-os: only darwin/arm64 releases are published" >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "agent-os: curl is required" >&2
  exit 1
fi
if command -v shasum >/dev/null 2>&1; then
  checksum_tool="shasum"
elif command -v sha256sum >/dev/null 2>&1; then
  checksum_tool="sha256sum"
else
  echo "agent-os: shasum or sha256sum is required" >&2
  exit 1
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/agent-os.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
chmod 700 "$tmp_dir"

if [ "$version" = "latest" ]; then
  release_tag="$(curl -fsSL --retry 3 -- "https://api.github.com/repos/${repository}/releases/latest" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  if [ -z "$release_tag" ]; then
    echo "agent-os: could not resolve the latest release tag" >&2
    exit 1
  fi
else
  release_tag="$version"
fi
artifact_version="${release_tag#v}"
binary="agent-os_${artifact_version}_${os}_${arch}"
base_url="https://github.com/${repository}/releases/download/${release_tag}"

curl -fsSL --retry 3 -o "${tmp_dir}/checksums.txt" -- "${base_url}/checksums.txt"
curl -fsSL --retry 3 -o "${tmp_dir}/${binary}" -- "${base_url}/${binary}"

expected="$(awk -v file="${binary}" '$2 == file { print $1; exit }' "${tmp_dir}/checksums.txt")"
if [ -z "$expected" ]; then
  echo "agent-os: checksum entry for ${binary} is missing" >&2
  exit 1
fi
if [ "$checksum_tool" = "shasum" ]; then
  actual="$(shasum -a 256 "${tmp_dir}/${binary}" | awk '{print $1}')"
else
  actual="$(sha256sum "${tmp_dir}/${binary}" | awk '{print $1}')"
fi
if [ "$actual" != "$expected" ]; then
  echo "agent-os: checksum verification failed" >&2
  exit 1
fi

mkdir -p "$install_dir"
chmod 755 "${tmp_dir}/${binary}"
cp "${tmp_dir}/${binary}" "${install_dir}/agent-os"
chmod 755 "${install_dir}/agent-os"
echo "installed ${install_dir}/agent-os"
