// Package mermanwasm renders mermaid diagrams in process with merman's
// WebAssembly build, run by wazero, a pure Go WebAssembly runtime: no browser,
// no merman binary and no cgo. [New] returns an [Engine], which satisfies
// [mermaid_go.Renderer] like the root package's chrome and merman CLI engines.
//
// The package embeds the standard wasm32-wasip1 build from the merman SDK:
// every diagram family, SVG and PNG output, Cytoscape layouts and the bundled
// Cousine fonts, without formulas or ELK layout. For the math or full build,
// pass it to New with [WithModule] or [WithModuleFile]. It lives apart from the
// root package so that only programs importing it link wazero and the module.
//
// The module is distributed under MIT or Apache-2.0, with the third-party
// notices in THIRD_PARTY_NOTICES.md and THIRD_PARTY_LICENSES; a program that
// ships it should ship those too. SOURCE_AVAILABILITY.md names the exact merman
// commit it was built from. make update-mermanwasm in the repository refreshes
// all of them.
package mermanwasm

import _ "embed"

// Version is the merman release the module was taken from.
const Version = "0.11.0"

// SHA256 is the module's checksum as merman's SDK manifest records it, so an
// update can be checked against the release it claims to come from.
const SHA256 = "beb0cee5eb0b7f5569d2adda9d26a79712ed81372133fb3f83ca96b186738228"

// Module is the embedded merman_wasi.wasm reactor, which New runs unless it is
// given another.
//
//go:embed merman_wasi.wasm
var Module []byte
