#!/usr/bin/env bash
# Updates an asset mermaid.go embeds to its latest release, or to the release
# MERMAID_VERSION or MERMAN_VERSION names:
#
#   scripts/update-assets.sh mermaidjs   # mermaid.min.js
#   scripts/update-assets.sh mermanwasm  # mermanwasm/merman_wasi.wasm and its notices
#
# Needs curl, jq, tar with xz support, openssl, and sha256sum or shasum.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "$*" >&2; exit 1; }

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mermaidjs() {
	local version="${MERMAID_VERSION:-$(curl -fsSL https://registry.npmjs.org/mermaid/latest | jq -r .version)}"
	[[ -n "$version" && "$version" != null ]] || die "could not resolve the mermaid release"
	local meta tarball integrity actual
	meta="$(curl -fsSL "https://registry.npmjs.org/mermaid/$version")"
	tarball="$(jq -r .dist.tarball <<<"$meta")"
	integrity="$(jq -r .dist.integrity <<<"$meta")"
	curl -fsSL -o "$tmp/mermaid.tgz" "$tarball"
	# The registry's integrity is a Subresource Integrity string over the tarball.
	actual="sha512-$(openssl dgst -sha512 -binary "$tmp/mermaid.tgz" | openssl base64 -A)"
	[[ "$actual" == "$integrity" ]] || die "mermaid $version: tarball integrity $actual, registry says $integrity"
	tar -xzf "$tmp/mermaid.tgz" -C "$tmp" package/dist/mermaid.min.js
	install -m 0644 "$tmp/package/dist/mermaid.min.js" mermaid.min.js
	echo "mermaid.min.js: mermaid $version"
}

mermanwasm() {
	local version="${MERMAN_VERSION:-$(curl -fsSL https://api.github.com/repos/Latias94/merman/releases/latest | jq -r .tag_name)}"
	[[ -n "$version" && "$version" != null ]] || die "could not resolve the merman release"
	local name=merman-sdk-wasi-standard-wasm32-wasip1
	local base="https://github.com/Latias94/merman/releases/download/$version"
	curl -fsSL -o "$tmp/$name.tar.xz" "$base/$name.tar.xz"
	curl -fsSL -o "$tmp/$name.tar.xz.sha256" "$base/$name.tar.xz.sha256"
	local want got
	want="$(cut -d' ' -f1 "$tmp/$name.tar.xz.sha256")"
	got="$(sha256 "$tmp/$name.tar.xz")"
	[[ "$got" == "$want" ]] || die "$name $version: sha256 $got, release says $want"
	tar -xJf "$tmp/$name.tar.xz" -C "$tmp"
	local sdk="$tmp/$name"
	# The SDK's manifest records the module's checksum; check the module against it.
	want="$(jq -r '[..|objects|select(has("lib/merman_wasi.wasm"))|.["lib/merman_wasi.wasm"].sha256][0]' "$sdk/manifest.json")"
	got="$(sha256 "$sdk/lib/merman_wasi.wasm")"
	[[ "$got" == "$want" ]] || die "merman_wasi.wasm $version: sha256 $got, manifest says $want"

	rm -rf mermanwasm/THIRD_PARTY_LICENSES
	cp -R "$sdk/THIRD_PARTY_LICENSES" mermanwasm/
	for f in lib/merman_wasi.wasm LICENSE-MIT LICENSE-APACHE THIRD_PARTY_NOTICES.md SOURCE_AVAILABILITY.md; do
		install -m 0644 "$sdk/$f" mermanwasm/
	done
	sed -i.bak -e "s/^const Version = \".*\"/const Version = \"${version#v}\"/" \
		-e "s/^const SHA256 = \".*\"/const SHA256 = \"$got\"/" mermanwasm/mermanwasm.go
	# Keep CI's merman-cli on the same release, so both merman backends are
	# tested against one version.
	sed -i.bak -e "s/MERMAN_VERSION: v[0-9][^ ]*/MERMAN_VERSION: $version/" .github/workflows/ci.yml
	rm -f mermanwasm/mermanwasm.go.bak .github/workflows/ci.yml.bak
	go test -run TestModuleMatchesTheRelease ./mermanwasm/
	echo "mermanwasm: merman $version"
}

case "${1:-}" in
mermaidjs | mermanwasm) "$1" ;;
*) die "usage: $0 mermaidjs|mermanwasm" ;;
esac
