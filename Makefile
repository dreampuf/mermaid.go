# Updates the assets mermaid.go embeds to their latest releases:
#
#   make update                                   # both
#   make update-mermaidjs MERMAID_VERSION=12.2.0  # pin a release instead
#   make update-mermanwasm MERMAN_VERSION=v0.9.3
#
# The work is done by scripts/update-assets.sh, which lists what it needs.

.PHONY: update update-mermaidjs update-mermanwasm

update: update-mermaidjs update-mermanwasm

update-mermaidjs:
	MERMAID_VERSION="$(MERMAID_VERSION)" ./scripts/update-assets.sh mermaidjs

update-mermanwasm:
	MERMAN_VERSION="$(MERMAN_VERSION)" ./scripts/update-assets.sh mermanwasm
