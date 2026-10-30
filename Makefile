# Updates the assets the subpackages embed to their latest releases:
#
#   make update                                 # both
#   make update-mermaidjs MERMAID_VERSION=12.2.0  # pin a release instead
#   make update-mermanwasm MERMAN_VERSION=v0.9.3
#
# Needs curl, jq, tar with xz support and sha256sum. update-mermanwasm also
# moves CI's merman-cli pin to the same release, so both merman backends are
# tested against one version.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.ONESHELL:

# Resolved only by the target that uses them.
MERMAID_VERSION ?= $(shell curl -fsSL https://registry.npmjs.org/mermaid/latest | jq -r .version)
MERMAN_VERSION ?= $(shell curl -fsSL https://api.github.com/repos/Latias94/merman/releases/latest | jq -r .tag_name)

MERMAN_SDK := merman-sdk-wasi-standard-wasm32-wasip1

.PHONY: update update-mermaidjs update-mermanwasm

update: update-mermaidjs update-mermanwasm

update-mermaidjs:
	@version='$(MERMAID_VERSION)'
	[[ -n "$$version" && "$$version" != null ]] || { echo "could not resolve the mermaid release" >&2; exit 1; }
	tmp="$$(mktemp)"
	trap 'rm -f "$$tmp"' EXIT
	curl -fsSL -o "$$tmp" "https://unpkg.com/mermaid@$$version/dist/mermaid.min.js"
	mv "$$tmp" mermaidjs/mermaid.min.js
	echo "mermaidjs: mermaid $$version"

update-mermanwasm:
	@version='$(MERMAN_VERSION)'
	[[ -n "$$version" && "$$version" != null ]] || { echo "could not resolve the merman release" >&2; exit 1; }
	tmp="$$(mktemp -d)"
	trap 'rm -rf "$$tmp"' EXIT
	base="https://github.com/Latias94/merman/releases/download/$$version"
	curl -fsSL -o "$$tmp/$(MERMAN_SDK).tar.xz" "$$base/$(MERMAN_SDK).tar.xz"
	curl -fsSL -o "$$tmp/$(MERMAN_SDK).tar.xz.sha256" "$$base/$(MERMAN_SDK).tar.xz.sha256"
	(cd "$$tmp" && sha256sum -c --quiet "$(MERMAN_SDK).tar.xz.sha256")
	tar -xJf "$$tmp/$(MERMAN_SDK).tar.xz" -C "$$tmp"
	sdk="$$tmp/$(MERMAN_SDK)"
	# The SDK's manifest records the module's checksum; check the module against it.
	sum="$$(jq -r '[..|objects|select(has("lib/merman_wasi.wasm"))|.["lib/merman_wasi.wasm"].sha256][0]' "$$sdk/manifest.json")"
	echo "$$sum  $$sdk/lib/merman_wasi.wasm" | sha256sum -c --quiet -
	rm -rf mermanwasm/THIRD_PARTY_LICENSES
	cp -R "$$sdk/THIRD_PARTY_LICENSES" mermanwasm/
	cp "$$sdk/lib/merman_wasi.wasm" "$$sdk/LICENSE-MIT" "$$sdk/LICENSE-APACHE" \
		"$$sdk/THIRD_PARTY_NOTICES.md" "$$sdk/SOURCE_AVAILABILITY.md" mermanwasm/
	sed -i.bak -e "s/^const Version = \".*\"/const Version = \"$${version#v}\"/" \
		-e "s/^const SHA256 = \".*\"/const SHA256 = \"$$sum\"/" mermanwasm/mermanwasm.go
	sed -i.bak -e "s/MERMAN_VERSION: v[0-9][^ ]*/MERMAN_VERSION: $$version/" .github/workflows/ci.yml
	rm -f mermanwasm/mermanwasm.go.bak .github/workflows/ci.yml.bak
	go test ./mermanwasm/
	echo "mermanwasm: merman $$version"
