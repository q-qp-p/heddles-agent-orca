#!/usr/bin/env bash
# Vendors NVIDIA NeMo Switchyard's Go binding and its WebAssembly guest into
# third_party/switchyard, and checks that what is committed is what the pinned
# upstream commit builds to.
#
#   hack/switchyard-update.sh update <sha> [branch]   vendor <sha> and rebuild the guest
#   hack/switchyard-update.sh verify                  rebuild from UPSTREAM and compare
#
# The Go source is copied verbatim. The guest (bindings/go/internal/wasm/
# switchyard.wasm) is not committed upstream; it is built by upstream's own
# scripts/build-go-wasm.sh inside a digest-pinned Rust image, so it needs Docker
# and no local Rust. The result is stored in Git LFS. Fetching uses codeload, not
# `gh api`, which NVIDIA's SAML blocks.
#
# Day to day builds never run this: the committed guest is embedded as is.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor="$root/third_party/switchyard"
upstream_file="$vendor/UPSTREAM"
repo="NVIDIA-NeMo/Switchyard"
# Pinned by digest so the toolchain is the same on every machine. The image's
# default toolchain is 1.99.1; the build is pointed at exactly that version
# (upstream asks for "1.99", which rustup would resolve to whatever is newest).
rust_image="rust:1.99.1-bookworm@sha256:e9da6f6c3ed5095fd5d33bf7cdf309830a178a8147888f1b2f76d5769bd9306c"
rust_toolchain="1.99.1"
wasm_rel="bindings/go/internal/wasm/switchyard.wasm"

# Scratch directory for downloads and the guest build; removed on exit.
tmp=""
trap 'rm -rf "${tmp:-}" 2>/dev/null || true' EXIT

die() { echo "switchyard-update: $*" >&2; exit 1; }

sha256_of() { # sha256_of <file>
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

read_upstream() { # read_upstream <key>
  [[ -f "$upstream_file" ]] || die "$upstream_file is missing; run: $0 update <sha>"
  sed -n "s/^$1=//p" "$upstream_file"
}

# fetch <sha> <dir>: download and unpack the commit into <dir>/src; prints the tarball's sha256.
fetch() {
  local sha="$1" dir="$2"
  mkdir -p "$dir/src"
  curl -fsSL -o "$dir/switchyard.tar.gz" "https://codeload.github.com/$repo/tar.gz/$sha" ||
    die "could not download $repo@$sha"
  tar -xzf "$dir/switchyard.tar.gz" -C "$dir/src" --strip-components=1
  sha256_of "$dir/switchyard.tar.gz"
}

# build_guest <srcdir>: build switchyard.wasm in the pinned Rust image, linux/amd64
# like CI so the bytes do not depend on the machine that ran this.
build_guest() {
  local src="$1"
  command -v docker >/dev/null 2>&1 || die "docker is required to build the guest"
  # Exact toolchain, upstream script otherwise unchanged.
  sed -i.bak "s/^channel = .*/channel = \"$rust_toolchain\"/" "$src/rust-toolchain.toml"
  rm -f "$src/rust-toolchain.toml.bak"
  # The container builds as root; on Linux that leaves root-owned files the host
  # cannot delete, so ownership goes back to the caller before it exits.
  docker run --rm --platform linux/amd64 \
    -v "$src":/switchyard -w /switchyard \
    -e CARGO_HOME=/usr/local/cargo \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "$rust_image" bash -c 'status=0; bash scripts/build-go-wasm.sh || status=$?; chown -R "$HOST_UID:$HOST_GID" /switchyard; exit $status'
  [[ -s "$src/$wasm_rel" ]] || die "the build produced no $wasm_rel"
}

cmd_update() {
  local sha="${1:?usage: $0 update <sha> [branch]}" branch="${2:-}"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || die "pass the full 40 character commit SHA"
  [[ -n "$branch" ]] || branch="$(read_upstream branch 2>/dev/null || true)"
  tmp="$(mktemp -d)"
  local tarball_sha; tarball_sha="$(fetch "$sha" "$tmp")"
  build_guest "$tmp/src"

  mkdir -p "$vendor"
  rm -rf "$vendor/bindings"
  mkdir -p "$vendor/bindings/go"
  # Verbatim copy, minus the guest (stored beside it, below) and build output.
  (cd "$tmp/src/bindings/go" && tar -cf - --exclude=./internal/wasm .) | (cd "$vendor/bindings/go" && tar -xf -)
  mkdir -p "$vendor/bindings/go/internal/wasm"
  cp "$tmp/src/$wasm_rel" "$vendor/$wasm_rel"
  cp "$tmp/src/LICENSE" "$tmp/src/NOTICE" "$vendor/"

  local wasm_sha; wasm_sha="$(sha256_of "$vendor/$wasm_rel")"
  printf '%s  %s\n' "$wasm_sha" "$wasm_rel" > "$vendor/SWITCHYARD_WASM.sha256"
  cat > "$upstream_file" <<UP
repo=$repo
branch=$branch
sha=$sha
tarball_sha256=$tarball_sha
rust_image=$rust_image
rust_toolchain=$rust_toolchain
wasm_sha256=$wasm_sha
UP
  echo "vendored $repo@$sha; guest sha256 $wasm_sha"
}

cmd_verify() {
  local sha tarball_sha want
  sha="$(read_upstream sha)"; tarball_sha="$(read_upstream tarball_sha256)"; want="$(read_upstream wasm_sha256)"
  tmp="$(mktemp -d)"
  local got_tar; got_tar="$(fetch "$sha" "$tmp")"
  [[ "$got_tar" == "$tarball_sha" ]] || die "the upstream tarball for $sha changed: $got_tar, UPSTREAM records $tarball_sha"

  # The vendored Go source is exactly upstream's.
  # (Compared with the guest taken out: it is checked separately below, and
  # internal/ exists in the vendored tree only to hold it.)
  cp -R "$vendor/bindings/go" "$tmp/vendored"
  rm -rf "$tmp/vendored/internal/wasm"
  rmdir "$tmp/vendored/internal" 2>/dev/null || true
  diff -r "$tmp/src/bindings/go" "$tmp/vendored" >/dev/null ||
    die "third_party/switchyard/bindings/go differs from upstream $sha (rerun: make switchyard-update SHA=$sha)"
  diff -q "$tmp/src/LICENSE" "$vendor/LICENSE" >/dev/null || die "LICENSE differs from upstream"
  diff -q "$tmp/src/NOTICE" "$vendor/NOTICE" >/dev/null || die "NOTICE differs from upstream"

  # The committed guest matches both the recorded checksum and a fresh build.
  [[ "$(sha256_of "$vendor/$wasm_rel")" == "$want" ]] || die "the committed guest does not match UPSTREAM's wasm_sha256"
  [[ "$(awk '{print $1}' "$vendor/SWITCHYARD_WASM.sha256")" == "$want" ]] || die "SWITCHYARD_WASM.sha256 does not match UPSTREAM"
  build_guest "$tmp/src"
  got="$(sha256_of "$tmp/src/$wasm_rel")"
  [[ "$got" == "$want" ]] || die "rebuilding $sha gives guest $got, but $want is committed: the build is not reproducible or the guest was changed"
  echo "OK: third_party/switchyard matches $repo@$sha and its guest rebuilds to $want"
}

case "${1:-}" in
  update) shift; cmd_update "$@" ;;
  verify) cmd_verify ;;
  *) die "usage: $0 update <sha> [branch] | verify" ;;
esac
