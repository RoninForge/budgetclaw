#!/bin/sh
# budgetclaw installer
#
# Downloads the latest release binary from GitHub Releases for the
# host OS and architecture, verifies the SHA-256 checksum, and
# installs it to a directory on $PATH.
#
# Usage:
#   curl -fsSL https://roninforge.org/get | sh
#
# Environment overrides:
#   BUDGETCLAW_VERSION  pin a specific version (default: latest)
#   BUDGETCLAW_PREFIX   install dir (default: /usr/local/bin, falling
#                       back to $HOME/.local/bin if /usr/local is not
#                       writable)
#
# POSIX-compliant. Do not use bashisms.

set -eu

REPO="RoninForge/budgetclaw"
VERSION="${BUDGETCLAW_VERSION:-latest}"
PREFIX="${BUDGETCLAW_PREFIX:-}"

err() {
  printf '\033[31merror:\033[0m %s\n' "$1" >&2
  exit 1
}

info() {
  printf '\033[36m==>\033[0m %s\n' "$1"
}

need() {
  command -v "$1" >/dev/null 2>&1 || err "missing required tool: $1"
}

detect_os() {
  os="$(uname -s)"
  case "$os" in
    Darwin) echo "darwin" ;;
    Linux)  echo "linux" ;;
    *)      err "unsupported OS: $os" ;;
  esac
}

detect_arch() {
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) err "unsupported architecture: $arch" ;;
  esac
}

pick_prefix() {
  if [ -n "$PREFIX" ]; then
    echo "$PREFIX"
    return
  fi
  if [ -w /usr/local/bin ] 2>/dev/null; then
    echo "/usr/local/bin"
  elif [ -w /usr/local ] 2>/dev/null; then
    mkdir -p /usr/local/bin
    echo "/usr/local/bin"
  else
    mkdir -p "$HOME/.local/bin"
    echo "$HOME/.local/bin"
  fi
}

resolve_version() {
  if [ "$VERSION" = "latest" ]; then
    # Follow the /releases/latest redirect and extract the tag.
    url="https://github.com/$REPO/releases/latest"
    resolved="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url")"
    tag="${resolved##*/}"
    if [ -z "$tag" ] || [ "$tag" = "latest" ]; then
      err "could not resolve latest version from $url"
    fi
    echo "$tag"
  else
    case "$VERSION" in
      v*) echo "$VERSION" ;;
      *)  echo "v$VERSION" ;;
    esac
  fi
}

verify_sha256() {
  file="$1"
  expected="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"
  else
    err "need sha256sum or shasum to verify download"
  fi
  if [ "$actual" != "$expected" ]; then
    err "checksum mismatch for $file: expected $expected, got $actual"
  fi
}

main() {
  need curl
  need tar
  need uname
  need mktemp

  os="$(detect_os)"
  arch="$(detect_arch)"
  tag="$(resolve_version)"
  version="${tag#v}"
  prefix="$(pick_prefix)"

  archive="budgetclaw_${version}_${os}_${arch}.tar.gz"
  checksums="checksums.txt"
  base="https://github.com/$REPO/releases/download/$tag"

  info "installing budgetclaw $tag for $os/$arch into $prefix"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  info "downloading $archive"
  curl -fsSL "$base/$archive" -o "$tmp/$archive" \
    || err "download failed: $base/$archive"

  info "downloading checksums"
  curl -fsSL "$base/$checksums" -o "$tmp/$checksums" \
    || err "checksum download failed: $base/$checksums"

  expected="$(grep " $archive\$" "$tmp/$checksums" | awk '{print $1}')"
  [ -n "$expected" ] || err "no checksum entry found for $archive"
  verify_sha256 "$tmp/$archive" "$expected"

  info "extracting"
  tar -xzf "$tmp/$archive" -C "$tmp"

  info "installing to $prefix/budgetclaw"
  install -m 0755 "$tmp/budgetclaw" "$prefix/budgetclaw" 2>/dev/null || {
    mv "$tmp/budgetclaw" "$prefix/budgetclaw"
    chmod 0755 "$prefix/budgetclaw"
  }

  info "installed. run: budgetclaw version"

  case ":$PATH:" in
    *":$prefix:"*) ;;
    *)
      printf '\n\033[33mwarning:\033[0m %s is not on your $PATH.\n' "$prefix"
      printf 'Add this to your shell rc:\n\n  export PATH="%s:$PATH"\n\n' "$prefix"
      ;;
  esac
}

main "$@"
